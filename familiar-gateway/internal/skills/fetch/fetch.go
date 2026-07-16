// Package fetch provides a page-fetching skill that lets the LLM read a
// user-supplied URL. This is intentionally distinct from any automated
// page-fetch fallback for Brave result enrichment — this tool only fires
// when the model (or user) explicitly provides a URL to read.
package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/familiar/gateway/internal/skills"
)

const (
	// maxBodyBytes caps the raw HTML we'll read before extraction.
	// 5 MB is generous for article pages; anything larger is almost
	// certainly not something the LLM should be digesting.
	maxBodyBytes = 5 * 1024 * 1024

	// maxContentChars caps the extracted text returned to the model.
	// ~12k chars ≈ 3k tokens — enough for a full article without
	// blowing the context budget on a single tool result.
	maxContentChars = 12000

	// fetchTimeout is the per-request HTTP timeout.
	fetchTimeout = 15 * time.Second

	// userAgent identifies Familiar so site operators can see what's
	// hitting them. Polite, identifiable, not pretending to be Chrome.
	userAgent = "FamiliarBot/1.0 (page-fetch skill; +https://familiar.dev)"
)

// Skill exposes page fetching as the `fetch_page` tool.
type Skill struct {
	client *http.Client
}

// New creates a fetch skill. The HTTP client is constructed internally
// with an appropriate timeout and an SSRF guard.
//
// SSRF guard (MEDIA/EXTERNAL-READINESS): fetch_page takes an LLM- or
// user-supplied URL, so without a guard a prompt-injection could aim
// it at cloud metadata (169.254.169.254), loopback services
// (127.0.0.1:5432), or RFC1918 LAN hosts. The Dialer.Control hook
// fires with the POST-DNS-resolution IP:port right before the socket
// connects, which is the airtight place to block — it defeats DNS
// rebinding (a host that resolves public on the CheckRedirect pass
// but private at dial time) and applies to every redirect hop because
// redirects reuse this transport.
func New() *Skill {
	return &Skill{
		client: &http.Client{
			Timeout:   fetchTimeout,
			Transport: SafeTransport(),
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("stopped after 5 redirects")
				}
				return nil
			},
		},
	}
}

// blockNonPublicDial is a net.Dialer Control hook that refuses to
// connect to non-public addresses. It fires with the POST-DNS
// resolved IP:port right before the socket connects — the airtight
// place to block SSRF: it defeats DNS rebinding and applies to every
// redirect hop because the transport is reused.
func blockNonPublicDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("fetch: bad dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control always receives a resolved IP literal; a non-IP here
		// means something upstream changed — fail closed.
		return fmt.Errorf("fetch: non-IP dial target %q", host)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("fetch: refusing to connect to non-public address %s", ip)
	}
	return nil
}

// SafeTransport returns an *http.Transport that refuses to connect to
// non-public addresses (loopback, RFC1918, link-local incl. the cloud
// metadata endpoint, …). Reuse it for any code path that fetches a
// caller-supplied URL (e.g. the admin skill-package importer) so those
// share the same SSRF protection as the fetch_page tool.
func SafeTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   blockNonPublicDial,
	}
	return &http.Transport{DialContext: dialer.DialContext}
}

// isBlockedIP reports whether an IP is in a range fetch_page must not
// reach: loopback, RFC1918 private (10/8, 172.16/12, 192.168/16) and
// IPv6 ULA (fc00::/7), link-local (incl. the 169.254.169.254 cloud
// metadata endpoint), unspecified (0.0.0.0/::), and multicast.
//
// Carrier-grade NAT (100.64.0.0/10) is intentionally NOT blocked —
// this deployment runs on a Tailscale fabric in that range and the
// operator's own internal fetches rely on it. If the tool is ever
// exposed to fully untrusted tenants, add a config allowlist and
// block CGNAT here too.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

var fetchPageParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The full URL to fetch (must be http or https)."
    }
  },
  "required": ["url"]
}`)

func (s *Skill) Name() string        { return "fetch" }
func (s *Skill) Description() string { return "Fetch and extract readable content from a web page" }
func (s *Skill) Version() string     { return "1.0.0" }

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "fetch_page",
			Description: "Fetch a web page and extract its main text content. Use this when a user provides a URL they want you to read. Returns the page title and extracted body text.",
			Parameters:  fetchPageParams,
		},
	}
}

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

type fetchPageArgs struct {
	URL string `json:"url"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if toolName != "fetch_page" {
		return skills.ToolResult{}, fmt.Errorf("fetch: unknown tool %q", toolName)
	}

	var args fetchPageArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if args.URL == "" {
		return skills.ToolResult{Error: "url is required"}, nil
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return skills.ToolResult{Error: "url must start with http:// or https://"}, nil
	}

	title, body, err := s.fetchAndExtract(ctx, args.URL)
	if err != nil {
		log.Printf("[fetch] error url=%q err=%v", args.URL, err)
		return skills.ToolResult{Error: fmt.Sprintf("fetch failed: %v", err)}, nil
	}

	content := formatPage(args.URL, title, body)
	log.Printf("[fetch] url=%q title=%q content_len=%d", args.URL, truncStr(title, 60), len(content))

	return skills.ToolResult{
		Content: content,
		Tokens:  len(content) / 4,
	}, nil
}

// fetchAndExtract does the HTTP GET and goquery content extraction.
func (s *Skill) fetchAndExtract(ctx context.Context, rawURL string) (title, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "xml") {
		return "", "", fmt.Errorf("unsupported content type: %s", ct)
	}

	limited := io.LimitReader(resp.Body, maxBodyBytes)
	doc, err := goquery.NewDocumentFromReader(limited)
	if err != nil {
		return "", "", fmt.Errorf("parsing HTML: %w", err)
	}

	title = strings.TrimSpace(doc.Find("title").First().Text())

	// Strip elements that add noise without content value.
	doc.Find("script, style, nav, footer, header, aside, .sidebar, .menu, .nav, .advertisement, .ad, #comments").Remove()

	// Try progressively broader selectors. article > main > body.
	var text string
	for _, sel := range []string{"article", "main", "[role=main]", "body"} {
		node := doc.Find(sel).First()
		if node.Length() == 0 {
			continue
		}
		text = strings.TrimSpace(node.Text())
		if len(text) > 200 { // meaningful content threshold
			break
		}
	}

	if text == "" {
		text = strings.TrimSpace(doc.Find("body").Text())
	}

	// Collapse whitespace runs — goquery's .Text() preserves all the
	// newlines and indentation from the DOM.
	text = collapseWhitespace(text)

	// Truncate to budget.
	if runes := []rune(text); len(runes) > maxContentChars {
		text = string(runes[:maxContentChars]) + "\n\n[…truncated]"
	}

	return title, text, nil
}

// formatPage builds the LLM-facing output.
func formatPage(url, title, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Page: %s\n", url)
	if title != "" {
		fmt.Fprintf(&b, "Title: %s\n", title)
	}
	fmt.Fprintf(&b, "\n%s", body)
	return b.String()
}

// collapseWhitespace normalises runs of whitespace into single spaces
// or single newlines, trimming the result. This turns goquery's raw
// .Text() (which preserves DOM indentation) into something readable.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s) / 2)
	prevNewline := false
	prevSpace := false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			if !prevNewline {
				b.WriteByte('\n')
			}
			prevNewline = true
			prevSpace = false
		case r == ' ' || r == '\t':
			if !prevSpace && !prevNewline {
				b.WriteByte(' ')
			}
			prevSpace = true
		default:
			b.WriteRune(r)
			prevNewline = false
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// truncStr is a log-formatting helper.
func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
