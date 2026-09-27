package research

// Autonomous synthesis (RESEARCH-SKILL-SPEC §6.7): when a deep run's
// last round finishes, the skill writes the note from the evidence and
// delivers it. The evidence was gathered from web pages by workers that
// read hostile content, so it never reaches a turn with a useful tool:
//
//  1. the note is a no-tools completion over the evidence (read here,
//     server-side, and inlined), which the skill writes into the stub;
//  2. the memory pass is a turn whose only tool is save_fact, over the
//     note just written, and its reply is the chat summary.
//
// It used to be one owner-path turn with every tool and the user's
// memories in context, told to read_page the evidence: text a worker
// copied off a page could make it fetch a URL carrying what it knew, or
// rewrite notes. read_page also cut the middle out of a long evidence
// page, so the middle workers' findings never reached the note.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/pipeline"
)

// DeliverFunc posts a run's outcome into its conversation and sends the
// mobile push titled title. Best-effort: the run's status is the durable
// signal. Built in main.go (conversation store, push sender).
type DeliverFunc func(ctx context.Context, run *admin.ResearchRun, title, text string)

const (
	// synthStubText is the note stub's placeholder until the note lands.
	synthStubText = "_Writing up the research…_"

	// synthesisTier routes both synthesis turns to the chat model
	// unless a writer model is configured (Options.WriterModel).
	synthesisTier = "technical"

	// synthesisMaxTokens is the note turn's answer budget: the reply IS
	// the note, and a deep note runs 900-1,600+ words plus sources.
	synthesisMaxTokens = 8192

	// storeRetries bounds the retries of a run-status write or read
	// before the run is given up on; a transient DB error at a
	// transition used to leave the run active with nothing driving it.
	storeRetries = 3
)

// storeRetryBackoff is the wait between those retries (a var so tests
// don't sleep).
var storeRetryBackoff = 2 * time.Second

func terminalStatus(st string) bool {
	return st == admin.RunStatusDone || st == admin.RunStatusFailed
}

// retryStore runs f until it succeeds or storeRetries attempts fail.
func retryStore(f func(ctx context.Context) error) error {
	var err error
	for i := 0; i < storeRetries; i++ {
		if i > 0 {
			time.Sleep(storeRetryBackoff)
		}
		ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
		err = f(ctx)
		cancel()
		if err == nil || errors.Is(err, admin.ErrRunNotFound) {
			return err
		}
	}
	return err
}

func (s *Skill) loadRun(runID string) (*admin.ResearchRun, error) {
	var run *admin.ResearchRun
	err := retryStore(func(ctx context.Context) error {
		var e error
		run, e = s.opts.Runs.Get(ctx, runID)
		return e
	})
	return run, err
}

// setStatus compare-and-sets a run's status (non-terminal runs only),
// retrying store errors. applied=false with a nil error means the run
// was already terminal (a stop won).
func (s *Skill) setStatus(runID string, p admin.RunPatch) (bool, error) {
	var applied bool
	err := retryStore(func(ctx context.Context) error {
		var e error
		applied, e = s.opts.Runs.UpdateIfActive(ctx, runID, p)
		return e
	})
	return applied, err
}

// failRun marks an active run failed and tells the user. A run that is
// already terminal (stopped, or finished) gets no second message. run
// may be nil when it couldn't be loaded; the message then can't be
// delivered, but the run still stops blocking its conversation.
func (s *Skill) failRun(runID string, run *admin.ResearchRun, reason string) {
	log.Printf("[research] run %s failed: %s", runID, reason)
	st := admin.RunStatusFailed
	applied, err := s.setStatus(runID, admin.RunPatch{Status: &st, Error: &reason})
	if err != nil {
		log.Printf("[research] run %s: couldn't mark it failed: %v (it's reconciled at the next restart)", runID, err)
	}
	if !applied || run == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	defer cancel()
	s.opts.Deliver(ctx, run, "Research didn't finish: "+run.Topic,
		"Research on \""+run.Topic+"\" didn't finish: "+reason+". Ask me to retry when you like.")
}

// finishRun delivers a finished run's message under title and marks it
// done, unless a stop landed first (the run is then left failed and
// says nothing).
func (s *Skill) finishRun(runID string, run *admin.ResearchRun, title, text string) {
	if s.isCancelled(runID) {
		log.Printf("[research] run %s: stopped before delivery — not delivering", runID)
		return
	}
	if cur, err := s.loadRun(runID); err == nil && terminalStatus(cur.Status) {
		log.Printf("[research] run %s: %s before delivery (stopped) — not delivering", runID, cur.Status)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	s.opts.Deliver(ctx, run, title, text)
	cancel()
	st := admin.RunStatusDone
	if applied, err := s.setStatus(runID, admin.RunPatch{Status: &st}); err != nil {
		log.Printf("[research] run %s: mark done failed: %v", runID, err)
	} else if !applied {
		log.Printf("[research] run %s: stopped during delivery — left failed", runID)
	}
}

// synthesize is the default SynthesizeFunc: note, memory pass, delivery.
// It marks the run done or failed itself.
func (s *Skill) synthesize(ctx context.Context, runID string) {
	run, err := s.loadRun(runID)
	if err != nil {
		s.failRun(runID, nil, "couldn't load the run")
		return
	}
	if terminalStatus(run.Status) {
		log.Printf("[research] synth run %s: already %s, skipping", runID, run.Status)
		return
	}

	bctx, bcancel := context.WithTimeout(context.Background(), statusAppendTimeout)
	evidence, err := s.readEvidence(bctx, run)
	bcancel()
	if err != nil {
		s.failRun(runID, run, "couldn't read the evidence")
		return
	}
	if findingsCount(evidence) == 0 {
		// Every worker failed or came back empty (a search quota, a
		// model down). A note written now would come from the model's
		// own memory with invented citations, and its facts would be
		// saved as research.
		s.failRun(runID, run, "the research workers found nothing to write up")
		return
	}

	bctx, bcancel = context.WithTimeout(context.Background(), statusAppendTimeout)
	personal, err := s.opts.Backend.EnsurePersonalBook(bctx, run.UserID)
	if err != nil {
		bcancel()
		s.failRun(runID, run, "couldn't open your notes")
		return
	}
	title := "Research: " + run.Topic
	stub, err := s.opts.Backend.CreatePage(bctx, personal.ID, run.UserID, title, synthStubText, "")
	if err != nil {
		bcancel()
		s.failRun(runID, run, "couldn't create the note")
		return
	}
	nb, np := personal.Slug, stub.Slug
	_ = s.opts.Runs.Update(bctx, runID, admin.RunPatch{NoteBookSlug: &nb, NotePageSlug: &np})
	bcancel()

	// The run session keeps the research:run: prefix, so the spawn tool
	// refuses inside it even if a later change widens the envelopes.
	sessKey := runSessionPrefix + runID
	sess := s.opts.Sessions.GetOrCreateWithID(sessKey, sessKey, run.UserID)
	sess.SetIdentity("research", run.UserID)
	turnCtx := pipeline.BoundToCaller(ctx)
	topicSlug := slugifyTopic(run.Topic)

	noteText, noteInfo, noteErr := s.opts.Invoke(turnCtx, sess, evidenceMessage(capEvidence(evidence)),
		s.synthesisEnvelope("research-synthesis", s.renderPrompt(s.synthesisTmpl, run.Topic, topicSlug), nil, synthesisMaxTokens))
	if s.isCancelled(runID) {
		log.Printf("[research] synth run %s: stopped during the write-up", runID)
		return
	}
	if s.rootCtx.Err() != nil {
		// Shutting down: the run is left active and the next boot's
		// reconcile fails it and tells the user; a salvage racing the
		// teardown could land without its status and be told twice.
		log.Printf("[research] synth run %s: gateway shutting down during the write-up", runID)
		return
	}
	note := unfence(strings.TrimSpace(noteText))
	if noteErr != nil || ctx.Err() != nil || note == "" {
		// The note didn't come: the findings still exist, so they go in
		// the note as they are (without the skill's status lines) rather
		// than the run failing outright.
		log.Printf("[research] synth run %s: note turn failed (err=%v ctx=%v, %d chars) — salvaging the evidence", runID, noteErr, ctx.Err(), len(note))
		body := "> _The final synthesis didn't complete — below are the research findings the workers gathered. Ask me to write it up and I'll polish them into a proper note._\n\n" + stripStatusLines(evidence)
		if err := s.deliverToStub(run.UserID, personal, stub, stub.UpdatedAt, body); err != nil {
			s.failRun(runID, run, "the write-up didn't finish and the findings couldn't be saved")
			return
		}
		s.finishRun(runID, run, "Research findings saved: "+run.Topic, "I gathered the research on \""+run.Topic+"\" but the write-up didn't finish, so I've saved the findings to your notes as they are — ask me to write it up and I'll turn them into a polished note.\n\n"+researchNoteLink(personal.Slug, stub.Slug, title))
		return
	}
	// Written with IfMatch as created: an edit the user made meanwhile
	// is kept (the note is appended after it).
	if err := s.deliverToStub(run.UserID, personal, stub, stub.UpdatedAt, note); err != nil {
		s.failRun(runID, run, "the note couldn't be saved")
		return
	}

	// Memory pass + summary. Its failure costs the summary and the facts,
	// not the note.
	summary, memInfo, memErr := s.opts.Invoke(turnCtx, sess, "<note>\n"+neutralizeTag(note, "note")+"\n</note>",
		s.synthesisEnvelope("research-memory", s.renderPrompt(s.memoryPassTmpl, run.Topic, topicSlug), []string{"save_fact"}, 0))
	if memErr != nil {
		log.Printf("[research] synth run %s: memory pass failed: %v", runID, memErr)
		summary = ""
	}
	summary = strings.TrimSpace(summary)
	if summary == "" || strings.HasPrefix(summary, "[No final answer") {
		summary = "I've written up the research in your notes: " + title + "."
	}
	var noteIn, noteOut int64
	for _, info := range []*pipeline.RouteInfo{noteInfo, memInfo} {
		if info != nil {
			noteIn += int64(info.InputTokens)
			noteOut += int64(info.OutputTokens)
		}
	}
	if !strings.Contains(summary, "#note/") {
		summary += "\n\n" + researchCardBlock(run, personal.Slug, stub.Slug, title, noteIn, noteOut)
	}
	s.finishRun(runID, run, "Research ready: "+run.Topic, summary)
	log.Printf("[research] synth run %s: note %s/%s written + delivered", runID, personal.Slug, stub.Slug)
}

// synthesisEnvelope is a synthesis turn's envelope: no session, no
// commit, no memory retrieval (the shard path never retrieves), only
// the listed tools, on the writer model when one is configured.
func (s *Skill) synthesisEnvelope(id, systemPrompt string, tools []string, maxTokens int) *pipeline.ShardOverrides {
	if tools == nil {
		tools = []string{}
	}
	ov := &pipeline.ShardOverrides{
		ShardID:              id,
		SystemPrompt:         systemPrompt,
		SkipSessionHydration: true,
		SkipCommit:           true,
		ToolAllowlist:        tools,
		TierHint:             synthesisTier,
		MaxTokens:            maxTokens,
	}
	if m := s.writerModelID(); m != "" {
		ov.ModelOverride = m
	}
	return ov
}

func (s *Skill) renderPrompt(tmpl, topic, topicSlug string) string {
	return strings.NewReplacer("{{TOPIC}}", topic, "{{TOPIC_SLUG}}", topicSlug).Replace(tmpl)
}

// readEvidence loads the run's evidence page from the user's research
// book.
func (s *Skill) readEvidence(ctx context.Context, run *admin.ResearchRun) (string, error) {
	book, err := s.opts.Backend.EnsureResearchBook(ctx, run.UserID)
	if err != nil {
		return "", err
	}
	page, err := s.opts.Backend.GetPage(ctx, book.ID, run.EvidencePageSlug)
	if err != nil {
		return "", err
	}
	return page.Content, nil
}

// evidenceMessage is the note turn's user message: the evidence, fenced
// as data.
func evidenceMessage(evidence string) string {
	return "<evidence>\n" + neutralizeTag(evidence, "evidence") + "\n</evidence>"
}

// neutralizeTag defangs any <tag> or </tag> inside text, so the text
// can't close its own fence and pose as instructions after it.
func neutralizeTag(text, tag string) string {
	re := regexp.MustCompile(`(?i)<(/?)(` + regexp.QuoteMeta(tag) + `)`)
	return re.ReplaceAllString(text, "‹$1$2")
}

// capEvidence cuts evidence at maxEvidenceChars, on a line boundary
// when one is reasonably close (keeps citations whole), else on a rune
// boundary (a byte cut could hand the model invalid UTF-8).
func capEvidence(evidence string) string {
	if len(evidence) <= maxEvidenceChars {
		return evidence
	}
	cut := maxEvidenceChars
	if nl := strings.LastIndexByte(evidence[:cut], '\n'); nl > maxEvidenceChars/2 {
		cut = nl
	} else {
		for cut > 0 && !utf8.RuneStart(evidence[cut]) {
			cut--
		}
	}
	return evidence[:cut] + "\n\n[evidence truncated at the writer's context cap]"
}

// unfence strips a code fence wrapped around the whole reply (a model
// asked for markdown sometimes returns it as a ```markdown block).
func unfence(s string) string {
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || strings.Count(s, "```") != 2 {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	return strings.TrimSpace(strings.TrimSuffix(s[nl+1:], "```"))
}

var (
	// citedBullet is a worker's finding line: "- finding — [Source](url)".
	citedBullet = regexp.MustCompile(`^[-*] .*\]\(https?://`)
	// statusLine matches the lines the skill appends itself.
	statusLine = regexp.MustCompile(`(?m)^(?:---\n)?(?:worker \d+ \(.*\) failed: .*|run \S+ complete: .*)\n?`)
)

// findingsCount counts worker content on an evidence page: "### "
// headings and cited bullets under "## Findings" (the whole page when
// there's no such heading). The lines the skill appends itself — a
// worker's failure, a round's completion — never count, so a page of
// errors can't pass for research.
func findingsCount(page string) int {
	if i := strings.Index(page, "## Findings"); i >= 0 {
		page = page[i:]
	}
	n := 0
	for _, line := range strings.Split(page, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "### ") || citedBullet.MatchString(l) {
			n++
		}
	}
	return n
}

// stripStatusLines drops the skill's own status lines from evidence
// before it is shown to the user as findings.
func stripStatusLines(evidence string) string {
	return strings.TrimSpace(statusLine.ReplaceAllString(evidence, ""))
}

// researchNoteLink renders the workspace note-open link the frontend's
// #note/ click-delegation understands: the parts are URL-encoded exactly
// like the client's encodeURIComponent so a book slug's colon
// (personal:{userID}) round-trips through decodeURIComponent. Brackets
// are stripped from the label so they can't break the link markdown.
func researchNoteLink(bookSlug, pageSlug, title string) string {
	label := strings.NewReplacer("[", "", "]", "").Replace(title)
	if label == "" {
		label = "the note"
	}
	return "**[📄 Open " + label + " →](#note/" +
		url.QueryEscape(bookSlug) + "/" + url.QueryEscape(pageSlug) + ")**"
}

// researchCardBlock renders the delivered message's completed-research
// card as a fenced ```research-card block. research-blocks.js turns it
// into an inline card (note as a CTA) on both live delivery and reload;
// without the script it degrades to a readable key/value block. Fields
// are newline-stripped so a topic/title can't break the fence, and the
// book/page slugs are raw (the frontend URL-encodes them into #note/).
func researchCardBlock(run *admin.ResearchRun, bookSlug, pageSlug, title string, noteIn, noteOut int64) string {
	oneLine := func(s string) string {
		return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	}
	b := "```research-card\n" +
		"topic: " + oneLine(run.Topic) + "\n" +
		"worker_in: " + compactTokens(run.InputTokens) + "\n" +
		"worker_out: " + compactTokens(run.OutputTokens) + "\n"
	// note in/out omitted when unknown (0) — the frontend then shows the
	// Note-written row without a token tail.
	if noteIn > 0 || noteOut > 0 {
		b += "note_in: " + compactTokens(noteIn) + "\n" +
			"note_out: " + compactTokens(noteOut) + "\n"
	}
	b += "book: " + oneLine(bookSlug) + "\n" +
		"page: " + oneLine(pageSlug) + "\n" +
		"title: " + oneLine(title) + "\n" +
		"```"
	return b
}

// compactTokens formats a token count for the card's meta row
// (193210 → "193k", 1_250_000 → "1.2M").
func compactTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// slugifyTopic makes a short tag slug from a topic for the memory pass
// ("Meow Wolf history" → "meow-wolf-history").
func slugifyTopic(topic string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(topic) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
