package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/push"
	"github.com/familiar/gateway/internal/session"
	researchskill "github.com/familiar/gateway/internal/skills/research"
)

// makeResearchSynthesizer builds the §6.7 autonomy closure the research
// skill calls when a deep run's workers finish: it creates the note
// stub (deterministic slug), drives one owner-path pl.Handle turn that
// reads the evidence and fills the note + memory pass, delivers the
// summary into the originating conversation (+ mobile push), and marks
// the run done/failed. Built here because it needs collaborators
// (pipeline, conversation store, push) constructed after the skill.
func makeResearchSynthesizer(
	rSkill *researchskill.Skill,
	pl *pipeline.Pipeline,
	sm *session.Manager,
	wiki *admin.WikiStore,
	conv *admin.ConversationStore,
	runs *admin.ResearchRunStore,
	pushSender *push.Sender,
) researchskill.SynthesizeFunc {
	return func(turnCtx context.Context, runID string) {
		// bookkeeping runs on a DETACHED context so a cancelled synthesis
		// turn (shutdown) still lets the terminal status land — a run
		// left non-terminal would perma-block the conversation. turnCtx
		// (cancellable) drives only the long model turn.
		bg := func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 30*time.Second)
		}
		bctx, bcancel := bg()
		run, err := runs.Get(bctx, runID)
		bcancel()
		if err != nil {
			log.Printf("[research] synth: load run %s: %v", runID, err)
			return
		}
		// Cancelled before synthesis started (user hit stop → failed) —
		// don't write a note or deliver anything.
		if run.Status == admin.RunStatusFailed || run.Status == admin.RunStatusDone {
			log.Printf("[research] synth run %s: already %s, skipping", runID, run.Status)
			return
		}
		fail := func(reason string) {
			log.Printf("[research] synth run %s failed: %s", runID, reason)
			fctx, fcancel := bg()
			defer fcancel()
			st := admin.RunStatusFailed
			_ = runs.Update(fctx, runID, admin.RunPatch{Status: &st, Error: &reason})
			deliverResearch(fctx, conv, pushSender, run,
				"Research on \""+run.Topic+"\" didn't finish: "+reason+". Ask me to retry when you like.")
		}

		// Note stub in the personal book — deterministic slug so the run
		// row (and the "open note" link) know where the write-up lands.
		bctx, bcancel = bg()
		personal, err := wiki.EnsurePersonalBook(bctx, run.UserID)
		if err != nil {
			bcancel()
			fail("couldn't open your notes")
			return
		}
		title := "Research: " + run.Topic
		stub, err := wiki.CreatePage(bctx, personal.ID, run.UserID, title,
			"_Writing up the research…_", "")
		bcancel()
		if err != nil {
			fail("couldn't create the note")
			return
		}
		bctx, bcancel = bg()
		nb, np := personal.Slug, stub.Slug
		_ = runs.Update(bctx, runID, admin.RunPatch{NoteBookSlug: &nb, NotePageSlug: &np})
		bcancel()

		// Owner-path synthesis turn in a dedicated run session. The ID
		// MUST carry the research:run: prefix (GetOrCreateWithID, not
		// GetOrCreate — the latter mints a random UUID) so the skill's
		// spawn-refusal guard fires: without the prefix a synthesis turn
		// could spawn a nested run.
		sessKey := "research:run:" + runID
		sess := sm.GetOrCreateWithID(sessKey, sessKey, run.UserID)
		sess.SetIdentity("research", run.UserID)
		prompt := rSkill.SynthesisPrompt(run.Topic, slugifyTopic(run.Topic),
			researchEvidenceBookSlug(run.UserID), run.EvidencePageSlug, personal.Slug, stub.Slug)
		summary, synthInfo, turnErr := pl.Handle(turnCtx, sess, prompt, nil)

		// The note LANDING — not the turn's exit code — decides success.
		// A deep synthesis routinely writes the note and a 20-fact memory
		// pass, then hits the pipeline's 300s turn cap before emitting a
		// final summary; the deliverable exists, so that is a SUCCESS, not
		// a failure (a "failed" run + error notification on a delivered
		// note is the exact bug this closes). Re-read the stub: if the
		// model replaced the placeholder, we're done regardless of turnErr.
		noteLanded := false
		bctx, bcancel = bg()
		if p, gErr := wiki.GetPage(bctx, personal.ID, stub.Slug); gErr == nil {
			body := strings.TrimSpace(p.Content)
			noteLanded = body != "" && !strings.Contains(body, "Writing up the research")
		}
		bcancel()
		if !noteLanded {
			// Salvage: synthesis didn't land a note, but if the workers
			// gathered substantial evidence the run isn't a dead loss. Copy the
			// findings into the note stub and deliver a recoverable partial
			// instead of a blank "didn't finish" -- the false-failure this
			// closes. Thin/empty evidence still fails normally below.
			salvaged := false
			bctx, bcancel = bg()
			if eb, ebErr := wiki.GetBookBySlug(bctx, researchEvidenceBookSlug(run.UserID), run.UserID, false); ebErr == nil {
				if ep, epErr := wiki.GetPage(bctx, eb.ID, run.EvidencePageSlug); epErr == nil {
					ev := strings.TrimSpace(ep.Content)
					if len(ev) >= 800 {
						body := "> _The final synthesis didn't complete -- below are the research findings the workers gathered. Ask me to write it up and I'll polish them into a proper note._\n\n" + ev
						if _, uErr := wiki.UpdatePage(bctx, personal.ID, stub.Slug, run.UserID, admin.PagePatch{Content: &body}); uErr == nil {
							salvaged = true
						}
					}
				}
			}
			bcancel()
			if salvaged {
				// Don't deliver a salvage note if the user stopped the run.
				bctx, bcancel = bg()
				cur, gErr := runs.Get(bctx, runID)
				bcancel()
				if gErr == nil && cur != nil && (cur.Status == admin.RunStatusFailed || cur.Status == admin.RunStatusDone) {
					log.Printf("[research] synth run %s: %s before salvage (stopped) — not delivering", runID, cur.Status)
					return
				}
				bctx, bcancel = bg()
				msg := "I gathered the research on \"" + run.Topic + "\" but the write-up timed out before I could synthesize the final note. I've saved the findings to your notes -- ask me to write it up and I'll turn them into a polished note.\n\n" + researchNoteLink(personal.Slug, stub.Slug, title)
				deliverResearch(bctx, conv, pushSender, run, msg)
				st := admin.RunStatusDone
				_, _ = runs.UpdateIfActive(bctx, runID, admin.RunPatch{Status: &st})
				bcancel()
				log.Printf("[research] synth run %s: synth errored, evidence salvaged to note -- delivered as partial", runID)
				return
			}
			if turnErr != nil {
				fail("the write-up turn errored before the note was written")
			} else {
				fail("the write-up produced no note")
			}
			return
		}
		if turnErr != nil {
			log.Printf("[research] synth run %s: turn errored (%v) but the note landed — delivering as done", runID, turnErr)
		}
		summary = strings.TrimSpace(summary)
		if summary == "" {
			summary = "I've written up the research in your notes: " + title + "."
		}
		// Attach the compact completed-research card — research-blocks.js
		// renders it inline in the transcript with the note as a prominent
		// CTA, so a finished deep run reads as a durable artifact rather
		// than a bare dropped link. Built server-side because the deep path
		// has no SSE client to assemble it; degrades to a readable key/value
		// block if the script didn't load. Skip if the model already left a
		// note link in the summary (the card's CTA would duplicate it).
		if !strings.Contains(summary, "#note/") {
			// note-writing (synthesis) token usage — the second half of the
			// per-item in/out split the card shows (workers vs note).
			var noteIn, noteOut int64
			if synthInfo != nil {
				noteIn = int64(synthInfo.InputTokens)
				noteOut = int64(synthInfo.OutputTokens)
			}
			summary += "\n\n" + researchCardBlock(run, personal.Slug, stub.Slug, title, noteIn, noteOut)
		}

		// A stop during the (multi-minute) synthesis marks the run failed.
		// Re-check before delivering so a cancelled run doesn't post a
		// summary + fire a push.
		bctx, bcancel = bg()
		cur, gErr := runs.Get(bctx, runID)
		bcancel()
		if gErr == nil && cur != nil && (cur.Status == admin.RunStatusFailed || cur.Status == admin.RunStatusDone) {
			log.Printf("[research] synth run %s: %s before delivery (stopped) — not delivering", runID, cur.Status)
			return
		}
		bctx, bcancel = bg()
		defer bcancel()
		deliverResearch(bctx, conv, pushSender, run, summary)
		// Compare-and-set: a stop landing in the delivery window leaves the
		// run failed rather than reverting it to done.
		st := admin.RunStatusDone
		if applied, err := runs.UpdateIfActive(bctx, runID, admin.RunPatch{Status: &st}); err != nil {
			log.Printf("[research] synth run %s: mark done failed: %v", runID, err)
		} else if !applied {
			log.Printf("[research] synth run %s: stopped during delivery — left failed", runID)
		}
		log.Printf("[research] synth run %s: note %s/%s written + delivered", runID, personal.Slug, stub.Slug)
	}
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

// researchEvidenceBookSlug mirrors admin.researchSlug for the synthesis
// prompt (the evidence book is per-user, slug research:{userID}).
func researchEvidenceBookSlug(userID string) string { return "research:" + userID }

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

// deliverResearch posts text into the run's originating conversation
// (ownership-checked) and fires a mobile Web Push deep-linking back to
// it. Both best-effort: the run's status is the durable signal, the
// message + push are the proactive nudge.
func deliverResearch(ctx context.Context, conv *admin.ConversationStore, pushSender *push.Sender, run *admin.ResearchRun, text string) {
	if conv != nil {
		if owns, err := conv.OwnsConversation(ctx, run.ConversationID, run.UserID); err == nil && owns {
			if _, err := conv.AppendMessage(ctx, &admin.Message{
				ConversationID: run.ConversationID,
				Role:           "assistant",
				Content:        text,
				Model:          "research",
			}); err != nil {
				log.Printf("[research] deliver: append to %s: %v", run.ConversationID, err)
			}
		}
	}
	if pushSender != nil {
		if _, err := pushSender.Send(ctx, run.UserID, push.Payload{
			Title: "Research ready: " + run.Topic,
			Body:  pushPreview(text),
			URL:   "/#chat/" + run.ConversationID,
			Tag:   "research:" + run.ConversationID,
		}); err != nil {
			log.Printf("[research] deliver: push to %s: %v", run.UserID, err)
		}
	}
}

// pushPreview condenses a scheduled action's output into a one-line
// notification body — the full text lives in the conversation behind the
// tap, and Web Push payloads are size-capped, so we send a teaser only.
func pushPreview(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.TrimLeft(s, "#*->• ")) // drop leading markdown markers
	const max = 140
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	if s == "" {
		return "New scheduled update"
	}
	return s
}
