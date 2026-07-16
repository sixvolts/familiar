package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// Sender posts messages to Slack without requiring the event-loop
// adapter to be running. It exists so the scheduled-actions Slack
// deliverer (and any other proactive source) can push messages into
// a channel even when the gateway is launched with a non-Slack
// primary adapter.
//
// The Sender holds its own *slack.Client; it intentionally does NOT
// share state with SlackAdapter so the two lifecycles stay independent.
type Sender struct {
	api *slack.Client
}

// NewSender constructs a Sender from a bot token. Returns nil and an
// error when the token is empty so callers can log and skip
// registering the "slack" proactive target without bringing down
// startup.
// NewSender constructs a Sender. apiBaseURL overrides the Slack API
// root (default https://slack.com/api/) — set ONLY for tests against
// a fake server; pass "" in production.
func NewSender(botToken, apiBaseURL string) (*Sender, error) {
	if botToken == "" {
		return nil, fmt.Errorf("slack sender: empty bot_token")
	}
	opts := []slack.Option{}
	if apiBaseURL != "" {
		if !strings.HasSuffix(apiBaseURL, "/") {
			apiBaseURL += "/"
		}
		opts = append(opts, slack.OptionAPIURL(apiBaseURL))
	}
	return &Sender{api: slack.New(botToken, opts...)}, nil
}

// SendDM opens (or reuses) the bot's DM conversation with a Slack
// user and posts there. conversations.open is idempotent — Slack
// returns the existing IM channel — so no caching layer is needed.
// Requires the im:write scope on the bot token.
func (s *Sender) SendDM(ctx context.Context, slackUserID, text string) error {
	if slackUserID == "" {
		return fmt.Errorf("slack sender: empty slack user id")
	}
	if text == "" {
		return nil
	}
	ch, _, _, err := s.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users: []string{slackUserID},
	})
	if err != nil {
		return fmt.Errorf("slack open dm with %s: %w", slackUserID, err)
	}
	return s.SendProactive(ctx, ch.ID, text)
}

// SendProactive posts `text` to `channelID`. This is the method the
// scheduled-actions Slack deliverer calls. Long messages are sent as
// a single post — the slack-go client splits at the API level if
// needed. We do not use the 4k truncation the adapter applies to
// interactive replies because proactive briefings are commonly long.
func (s *Sender) SendProactive(ctx context.Context, channelID, text string) error {
	if channelID == "" {
		return fmt.Errorf("slack sender: empty channel_id")
	}
	if text == "" {
		return nil
	}
	// Proactive sources (scheduled actions etc.) hand us the engine's
	// CommonMark; Slack renders its own mrkdwn dialect, so a raw "##"
	// / "**" / "---" shows literally. Convert before posting — the
	// interactive adapter already does this on its own path.
	text = toMrkdwn(text)
	_, _, err := s.api.PostMessageContext(ctx, channelID,
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("slack post to %s: %w", channelID, err)
	}
	return nil
}
