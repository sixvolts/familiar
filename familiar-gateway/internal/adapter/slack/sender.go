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
// scheduled-actions Slack deliverer calls. Proactive briefings are
// commonly long: past proactiveMaxLen they go as several posts (split
// like interactive replies, at a larger size). They were sent as one
// post on the belief that slack-go splits long messages; it doesn't,
// and Slack truncates text past 40,000 characters without an error.
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
	for _, chunk := range splitMessage(text, proactiveMaxLen) {
		_, _, err := s.api.PostMessageContext(ctx, channelID,
			slack.MsgOptionText(chunk, false),
			slack.MsgOptionDisableLinkUnfurl(),
		)
		if err != nil {
			return fmt.Errorf("slack post to %s: %w", channelID, err)
		}
	}
	return nil
}

// proactiveMaxLen is the largest proactive post, in bytes: under
// Slack's 40,000-character limit whatever the characters.
const proactiveMaxLen = 35000

// IsUserOrDMID reports a Slack id that names a person (U…, W…) or a DM
// (D…) rather than a channel. chat.postMessage accepts a user id as
// the channel and DMs that user from the bot.
func IsUserOrDMID(id string) bool {
	return strings.HasPrefix(id, "U") || strings.HasPrefix(id, "W") || strings.HasPrefix(id, "D")
}

// ChannelOwner is who a scheduled action's channel post is made for.
type ChannelOwner struct {
	SlackUserID string // the owner's linked Slack identity; "" when none
	IsAdmin     bool
}

// PostForOwner posts an action owner's report to a channel, only where
// that owner may post: never a user or DM id (slack_dm is the target
// for those), only a channel in allowed when the operator restricted
// the bot to some ([adapter.slack] channels), and, for anyone but an
// admin, only a channel the owner's own Slack account is in. Any
// approved user could make the bot post any text to any channel or
// person before, private channels included.
//
// Membership comes from conversations.members, which needs the bot's
// channels:read (and groups:read, for private channels) scope; when it
// can't be checked, nothing is posted.
func (s *Sender) PostForOwner(ctx context.Context, channelID string, owner ChannelOwner, allowed []string, text string) error {
	if channelID == "" {
		return fmt.Errorf("slack sender: empty channel_id")
	}
	if IsUserOrDMID(channelID) {
		return fmt.Errorf("slack target %s is a person or a DM, not a channel — use the slack_dm target", channelID)
	}
	if len(allowed) > 0 && !containsID(allowed, channelID) {
		return fmt.Errorf("slack channel %s is not one of the bot's [adapter.slack] channels", channelID)
	}
	if !owner.IsAdmin {
		if owner.SlackUserID == "" {
			return fmt.Errorf("posting to slack channel %s needs a linked Slack identity on the action's owner", channelID)
		}
		member, err := s.IsChannelMember(ctx, channelID, owner.SlackUserID)
		if err != nil {
			return fmt.Errorf("could not check the owner's membership of %s (the bot needs channels:read and groups:read): %w", channelID, err)
		}
		if !member {
			return fmt.Errorf("the action's owner is not a member of slack channel %s", channelID)
		}
	}
	return s.SendProactive(ctx, channelID, text)
}

// IsChannelMember reports whether userID is in channelID
// (conversations.members, paginated).
func (s *Sender) IsChannelMember(ctx context.Context, channelID, userID string) (bool, error) {
	cursor := ""
	for page := 0; page < 100; page++ {
		ids, next, err := s.api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channelID, Cursor: cursor, Limit: 1000,
		})
		if err != nil {
			return false, err
		}
		if containsID(ids, userID) {
			return true, nil
		}
		if next == "" {
			return false, nil
		}
		cursor = next
	}
	return false, fmt.Errorf("slack: %s has too many members to check", channelID)
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
