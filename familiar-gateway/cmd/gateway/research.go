package main

import (
	"context"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/push"
	researchskill "github.com/familiar/gateway/internal/skills/research"
)

// makeResearchDeliver builds the research skill's delivery hook (§6.7):
// the skill runs synthesis itself and hands the outcome here to post
// into the run's conversation and push to the user's phone. Built here
// because the conversation store and push sender are constructed after
// the skill.
func makeResearchDeliver(conv *admin.ConversationStore, pushSender *push.Sender) researchskill.DeliverFunc {
	return func(ctx context.Context, run *admin.ResearchRun, title, text string) {
		deliverResearch(ctx, conv, pushSender, run, title, text)
	}
}

// deliverResearch posts text into the run's originating conversation
// (ownership-checked) and fires a mobile Web Push titled title,
// deep-linking back to it. Both best-effort: the run's status is the
// durable signal, the message + push are the proactive nudge.
func deliverResearch(ctx context.Context, conv *admin.ConversationStore, pushSender *push.Sender, run *admin.ResearchRun, title, text string) {
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
			Title: title,
			Body:  pushPreview(text),
			URL:   "/#chat/" + run.ConversationID,
			Tag:   "research:" + run.ConversationID,
		}); err != nil {
			log.Printf("[research] deliver: push to %s: %v", run.UserID, err)
		}
	}
}

// pushPreview condenses a research message into a one-line notification
// body — the full text lives in the conversation behind the tap, and Web
// Push payloads are size-capped, so we send a teaser only.
func pushPreview(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.TrimLeft(s, "#*->• ")) // drop leading markdown markers
	const max = 140
	if len(s) > max {
		// Back up to a rune boundary: a byte cut could end the preview
		// in half a character.
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	if s == "" {
		return "New research update"
	}
	return s
}
