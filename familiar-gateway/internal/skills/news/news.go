// Package news provides the Familiar news/RSS skill.
//
// Two tools are exposed:
//
//   - get_news:    aggregate headlines across a topic's configured RSS feeds
//   - search_news: delegate to Brave search for open-ended news queries
//
// RSS feeds are fetched in parallel per call with a short timeout, and
// results are cached for 15 minutes so a morning briefing that touches
// multiple topics doesn't refetch the same feeds back to back. The
// cache is intentionally tiny (one map, one mutex) to match the style
// of the weather skill — skills here are leaf utilities, not services.
package news

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/familiar/gateway/internal/brave"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/skills"
)

const defaultCacheTTL = 15 * time.Minute

// Skill implements the Familiar news/RSS skill. Both dependencies
// (feeds map and brave client) are optional — missing feeds disables
// get_news, and a missing brave client disables search_news. The skill
// registers regardless so misconfiguration surfaces as a user-facing
// tool error instead of a silent absence.
type Skill struct {
	parser *gofeed.Parser
	brave  *brave.Client
	feeds  map[string][]string
	ttl    time.Duration
	cache  *ttlCache
}

// New constructs a news skill.
func New(cfg config.NewsConfig, b *brave.Client) *Skill {
	ttl := defaultCacheTTL
	if cfg.CacheMinutes > 0 {
		ttl = time.Duration(cfg.CacheMinutes) * time.Minute
	}
	return &Skill{
		parser: gofeed.NewParser(),
		brave:  b,
		feeds:  cfg.Feeds,
		ttl:    ttl,
		cache:  newTTLCache(),
	}
}

func (s *Skill) Name() string { return "news" }
func (s *Skill) Description() string {
	return "Headlines from topic-scoped RSS feeds and Brave news search"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

var getNewsParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "topics": {
      "type": "array",
      "items": {"type": "string"},
      "description": "One or more topic labels to aggregate. Example: [\"ai\", \"cybersecurity\"]."
    },
    "max_items": {
      "type": "integer",
      "description": "Maximum total headlines to return across all topics. Defaults to 10.",
      "minimum": 1,
      "maximum": 50
    }
  },
  "required": ["topics"]
}`)

var searchNewsParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Free-text news query. Example: \"rivian earnings\"."
    },
    "max_items": {
      "type": "integer",
      "description": "Maximum results to return. Defaults to 5.",
      "minimum": 1,
      "maximum": 20
    }
  },
  "required": ["query"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "get_news",
			Description: "Aggregate recent headlines from one or more configured topic feeds.",
			Parameters:  getNewsParams,
		},
		{
			Name:        "search_news",
			Description: "Search the web for news on a free-text query via Brave Search.",
			Parameters:  searchNewsParams,
		},
	}
}

type getNewsArgs struct {
	Topics   []string `json:"topics"`
	MaxItems *int     `json:"max_items,omitempty"`
}

type searchNewsArgs struct {
	Query    string `json:"query"`
	MaxItems *int   `json:"max_items,omitempty"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	switch toolName {
	case "get_news":
		var args getNewsArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if len(args.Topics) == 0 {
			return skills.ToolResult{Error: "topics is required"}, nil
		}
		max := 10
		if args.MaxItems != nil && *args.MaxItems > 0 {
			max = *args.MaxItems
		}
		return s.getNews(ctx, args.Topics, max)

	case "search_news":
		var args searchNewsArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		if args.Query == "" {
			return skills.ToolResult{Error: "query is required"}, nil
		}
		max := 5
		if args.MaxItems != nil && *args.MaxItems > 0 {
			max = *args.MaxItems
		}
		return s.searchNews(ctx, args.Query, max)

	default:
		return skills.ToolResult{}, fmt.Errorf("news: unknown tool %q", toolName)
	}
}

// Article is the common shape returned to the LLM. Kept flat so the
// Data field stays easy for downstream consumers to walk.
type Article struct {
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Source    string    `json:"source"`
	Summary   string    `json:"summary,omitempty"`
	Published time.Time `json:"published,omitempty"`
	Topic     string    `json:"topic,omitempty"`
}

// --- get_news --------------------------------------------------------------

func (s *Skill) getNews(ctx context.Context, topics []string, max int) (skills.ToolResult, error) {
	if len(s.feeds) == 0 {
		return skills.ToolResult{Error: "news: no feeds configured"}, nil
	}

	// Fan out across every (topic, feedURL) pair so one slow feed can't
	// serialize the rest. Individual feed failures are logged into the
	// result text and dropped from the aggregate; they don't fail the
	// whole call.
	type job struct {
		topic string
		url   string
	}
	var jobs []job
	var missing []string
	for _, topic := range topics {
		urls, ok := s.feeds[topic]
		if !ok || len(urls) == 0 {
			missing = append(missing, topic)
			continue
		}
		for _, u := range urls {
			jobs = append(jobs, job{topic: topic, url: u})
		}
	}
	if len(jobs) == 0 {
		return skills.ToolResult{
			Error: fmt.Sprintf("no feeds configured for topic(s): %s", strings.Join(missing, ", ")),
		}, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		all      []Article
		failures []string
	)
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			arts, err := s.fetchFeed(fetchCtx, j.topic, j.url)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", j.url, err))
				return
			}
			all = append(all, arts...)
		}()
	}
	wg.Wait()

	// Sort newest first, then trim.
	sort.SliceStable(all, func(i, k int) bool {
		return all[i].Published.After(all[k].Published)
	})
	if len(all) > max {
		all = all[:max]
	}

	content := formatArticles(topics, all, failures, missing)
	data, _ := json.Marshal(all)
	return skills.ToolResult{
		Content: content,
		Data:    data,
		Tokens:  len(content) / 4,
	}, nil
}

// fetchFeed retrieves and parses one RSS/Atom URL, using the cache when
// fresh. Only the top ~15 items per feed are returned — the get_news
// caller sorts and trims across feeds.
func (s *Skill) fetchFeed(ctx context.Context, topic, url string) ([]Article, error) {
	if entry, ok := s.cache.get(url); ok {
		// Re-tag cached articles with the requesting topic so the same
		// feed reused under a different topic label shows up correctly.
		out := make([]Article, len(entry))
		copy(out, entry)
		for i := range out {
			out[i].Topic = topic
		}
		return out, nil
	}

	feed, err := s.parser.ParseURLWithContext(url, ctx)
	if err != nil {
		return nil, err
	}

	source := feed.Title
	if source == "" {
		source = url
	}

	limit := 15
	if len(feed.Items) < limit {
		limit = len(feed.Items)
	}
	arts := make([]Article, 0, limit)
	for i := 0; i < limit; i++ {
		item := feed.Items[i]
		if item == nil {
			continue
		}
		published := time.Time{}
		if item.PublishedParsed != nil {
			published = *item.PublishedParsed
		} else if item.UpdatedParsed != nil {
			published = *item.UpdatedParsed
		}
		arts = append(arts, Article{
			Title:     strings.TrimSpace(item.Title),
			URL:       item.Link,
			Source:    source,
			Summary:   trimSummary(item.Description),
			Published: published,
			Topic:     topic,
		})
	}

	// Cache without the Topic tag so reuse across topics stays correct.
	cached := make([]Article, len(arts))
	copy(cached, arts)
	for i := range cached {
		cached[i].Topic = ""
	}
	s.cache.set(url, cached, s.ttl)

	return arts, nil
}

// --- search_news -----------------------------------------------------------

func (s *Skill) searchNews(ctx context.Context, query string, max int) (skills.ToolResult, error) {
	if s.brave == nil {
		return skills.ToolResult{Error: "news: search_news requires brave client"}, nil
	}

	// Bias the Brave query toward news content. Brave's API doesn't
	// expose a "news" vertical on the free tier, so appending "news"
	// to the query is the lightest-touch bias we can apply without a
	// separate endpoint.
	results, err := s.brave.Search(ctx, query+" news")
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("search_news: brave: %w", err)
	}
	if max > 0 && max < len(results) {
		results = results[:max]
	}

	arts := make([]Article, 0, len(results))
	for _, r := range results {
		arts = append(arts, Article{
			Title:   r.Title,
			URL:     r.URL,
			Source:  "brave",
			Summary: trimSummary(r.Description),
		})
	}

	content := formatSearchArticles(query, arts)
	data, _ := json.Marshal(arts)
	return skills.ToolResult{
		Content: content,
		Data:    data,
		Tokens:  len(content) / 4,
	}, nil
}

// --- formatting ------------------------------------------------------------

func formatArticles(topics []string, arts []Article, failures, missing []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Headlines for %s:\n", strings.Join(topics, ", "))
	if len(arts) == 0 {
		b.WriteString("(no items)\n")
	}
	for _, a := range arts {
		when := ""
		if !a.Published.IsZero() {
			when = " (" + a.Published.Format("Jan 2 15:04") + ")"
		}
		tag := ""
		if a.Topic != "" {
			tag = "[" + a.Topic + "] "
		}
		fmt.Fprintf(&b, "- %s%s — %s%s\n", tag, a.Title, a.Source, when)
		if a.URL != "" {
			fmt.Fprintf(&b, "  %s\n", a.URL)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "(no feeds configured for: %s)\n", strings.Join(missing, ", "))
	}
	if len(failures) > 0 {
		fmt.Fprintf(&b, "(failed to fetch %d feed(s))\n", len(failures))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatSearchArticles(query string, arts []Article) string {
	if len(arts) == 0 {
		return fmt.Sprintf("No news results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "News results for %q:\n", query)
	for _, a := range arts {
		fmt.Fprintf(&b, "- %s\n  %s\n", a.Title, a.URL)
		if a.Summary != "" {
			fmt.Fprintf(&b, "  %s\n", a.Summary)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// trimSummary strips HTML-ish whitespace noise and caps length so a
// chatty RSS description doesn't blow the tool result token budget.
func trimSummary(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse runs of whitespace (incl. inside pulled-out HTML).
	fields := strings.Fields(s)
	s = strings.Join(fields, " ")
	if len(s) > 280 {
		s = s[:277] + "..."
	}
	return s
}

// --- tiny TTL cache --------------------------------------------------------

type cacheRecord struct {
	articles []Article
	expires  time.Time
}

type ttlCache struct {
	mu sync.Mutex
	m  map[string]cacheRecord
}

func newTTLCache() *ttlCache {
	return &ttlCache{m: make(map[string]cacheRecord)}
}

func (c *ttlCache) get(key string) ([]Article, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(rec.expires) {
		delete(c.m, key)
		return nil, false
	}
	return rec.articles, true
}

func (c *ttlCache) set(key string, arts []Article, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = cacheRecord{articles: arts, expires: time.Now().Add(ttl)}
}
