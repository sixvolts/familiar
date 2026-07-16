package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/engine"
	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const maxMessageLen = 4000

// Responder is the slice of the pipeline the adapter drives — one
// method. Narrowing to an interface (from *pipeline.Pipeline) lets
// the Socket Mode integration test inject a canned-reply fake without
// standing up a full pipeline. *pipeline.Pipeline satisfies it.
type Responder interface {
	Handle(ctx context.Context, sess *session.Session, userMsg string, convCtx *sidecar.ConversationContext) (string, *pipeline.RouteInfo, error)
}

// Conversations is the slice of the workspace conversation store the
// adapter needs to make Slack turns durable: resolve the stable
// conversation behind a DM/thread, and append the user prompt + final
// reply so the gateway's hydration path replays them (and any
// scheduled-action delivery written into the same conversation) on the
// next turn. Narrowed to primitives so the adapter doesn't import the
// admin package; main.go wraps *admin.ConversationStore to satisfy it.
// Optional — when nil the adapter keeps its prior stateless behaviour
// (hash session id, no persistence).
type Conversations interface {
	EnsureExternalConversation(ctx context.Context, userID, externalKey, title string) (convID string, err error)
	AppendMessage(ctx context.Context, convID, role, content, model string) error
}

// SlackAdapter connects Familiar to Slack via Socket Mode.
type SlackAdapter struct {
	pipeline Responder
	sessions *session.Manager
	engine   engine.Service
	cfg      config.SlackConfig
	verbose  bool
	api      *slack.Client
	botID    string
	resolver *identity.Resolver
	// convs persists Slack turns into durable conversations so the bot
	// remembers a DM/thread across turns and restarts, and sees what a
	// scheduled action posted into the same conversation. Optional.
	convs Conversations
	// instance carries the deployment's user-facing URLs + admin
	// contact, referenced in the bootstrap welcome DM so new users
	// know where to go for registration and support. Optional; the
	// DM falls through to a generic message when unset.
	instance config.InstanceConfig
}

// New constructs a SlackAdapter.
func New(p Responder, sm *session.Manager, eng engine.Service, cfg config.SlackConfig, verbose bool) *SlackAdapter {
	return &SlackAdapter{
		pipeline: p,
		sessions: sm,
		engine:   eng,
		cfg:      cfg,
		verbose:  verbose,
	}
}

// SetResolver wires the identity resolver into the adapter. Optional —
// when nil, the adapter falls back to its pre-multi-user behaviour
// (AllowedUsers config gate, no pending-user registration flow).
func (a *SlackAdapter) SetResolver(r *identity.Resolver) {
	a.resolver = r
}

// SetConversations wires the conversation store so Slack turns become
// durable, hydratable history. Optional; call once at startup.
func (a *SlackAdapter) SetConversations(c Conversations) {
	a.convs = c
}

// SetInstance hands the deployment's [instance] config to the
// adapter so the Phase-5 Slack-bootstrap welcome DM can include
// the real admin console URL and admin contact. Call once at
// startup after New.
func (a *SlackAdapter) SetInstance(inst config.InstanceConfig) {
	a.instance = inst
}

// socketModeEnvelope is the top-level JSON frame Slack sends over the
// Socket Mode WebSocket. Only fields we dispatch on are decoded.
type socketModeEnvelope struct {
	EnvelopeID     string          `json:"envelope_id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	NumConnections int             `json:"num_connections"`
	Reason         string          `json:"reason"`
}

// Run connects to Slack via Socket Mode and processes events until ctx
// is cancelled. We bypass slack-go's socketmode.Client entirely — its
// managed connection drops at ~10s because the receiver goroutine setup
// prevents conn.ReadJSON from running in time to process Slack's
// server-sent WebSocket PINGs. A raw gorilla/websocket loop with a
// trivial PingHandler holds stable.
func (a *SlackAdapter) Run(ctx context.Context) error {
	if a.cfg.BotToken == "" || a.cfg.AppToken == "" {
		return fmt.Errorf("slack adapter requires bot_token and app_token")
	}

	// slack.Client is used only for REST calls (AuthTest, PostMessage).
	// The app-level token is used directly by openConnection below and
	// no longer needs to live on the client — we dial the WebSocket
	// ourselves instead of going through slack-go's socketmode.
	apiOpts := []slack.Option{slack.OptionDebug(a.cfg.Debug)}
	if a.cfg.APIBaseURL != "" {
		apiOpts = append(apiOpts, slack.OptionAPIURL(a.apiURL()))
	}
	a.api = slack.New(a.cfg.BotToken, apiOpts...)

	authResp, err := a.api.AuthTest()
	if err != nil {
		return fmt.Errorf("slack auth test: %w", err)
	}
	a.botID = authResp.UserID
	log.Printf("[slack] authenticated as %s (user_id: %s)", authResp.User, a.botID)

	// Reconnect loop. apps.connections.open gives us a fresh short-lived
	// WSS URL on each try; network errors and server-requested disconnects
	// both fall through to the same reconnect path with exponential
	// backoff capped at 60s.
	backoff := 2 * time.Second
	const maxBackoff = 60 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := a.runConnection(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("[slack] connection lost: %v; reconnecting in %v", err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runConnection opens one Socket Mode WebSocket, handles frames until
// it dies, and returns the death reason. Its caller retries.
func (a *SlackAdapter) runConnection(ctx context.Context) error {
	wssURL, err := a.openConnection(ctx)
	if err != nil {
		return fmt.Errorf("apps.connections.open: %w", err)
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wssURL, nil)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()

	// PingHandler is what slack-go's managed conn fails to exercise in
	// time. gorilla fires this from inside ReadMessage, so it only runs
	// while we're actively reading — which we are, in the loop below.
	conn.SetPingHandler(func(data string) error {
		return conn.WriteControl(
			websocket.PongMessage,
			[]byte(data),
			time.Now().Add(5*time.Second),
		)
	})

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var env socketModeEnvelope
		if err := conn.ReadJSON(&env); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		// Ack immediately — Slack retries if we don't respond within 3s,
		// and we don't want the pipeline's latency to affect ack timing.
		// Keeping the ack write in the same goroutine as the reader also
		// sidesteps gorilla's one-writer rule.
		if env.EnvelopeID != "" {
			ack := map[string]string{"envelope_id": env.EnvelopeID}
			if err := conn.WriteJSON(ack); err != nil {
				log.Printf("[slack] ack write error: %v", err)
			}
		}

		switch env.Type {
		case "hello":
			log.Printf("[slack] connected (num_connections=%d)", env.NumConnections)
		case "events_api":
			go a.handleEventsAPI(ctx, env.Payload)
		case "disconnect":
			log.Printf("[slack] server requested disconnect: %s", env.Reason)
			return fmt.Errorf("server disconnect: %s", env.Reason)
		}
	}
}

// apiURL is the Slack API root with a trailing slash — the configured
// override (tests) or slack.com. Both the REST client and
// apps.connections.open derive from it.
func (a *SlackAdapter) apiURL() string {
	if a.cfg.APIBaseURL != "" {
		u := a.cfg.APIBaseURL
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
		return u
	}
	return "https://slack.com/api/"
}

// openConnection calls apps.connections.open to obtain a one-shot WSS URL.
func (a *SlackAdapter) openConnection(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		a.apiURL()+"apps.connections.open", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AppToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack api error: %s", result.Error)
	}
	return result.URL, nil
}

// handleEventsAPI parses a raw events_api payload and dispatches any
// user-message callbacks to handleMessage. Non-message callbacks are
// dropped silently.
func (a *SlackAdapter) handleEventsAPI(ctx context.Context, raw json.RawMessage) {
	event, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
	if err != nil {
		log.Printf("[slack] parse event error: %v", err)
		return
	}
	if event.Type != slackevents.CallbackEvent {
		return
	}
	if ev, ok := event.InnerEvent.Data.(*slackevents.MessageEvent); ok {
		a.handleMessage(ctx, ev)
	}
}

// handleMessage processes a single Slack message event.
func (a *SlackAdapter) handleMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	// Skip bot messages and message edits/deletes.
	if ev.BotID != "" || ev.User == "" || ev.User == a.botID {
		return
	}
	if ev.SubType != "" {
		// Subtypes include message_changed, message_deleted, etc.
		return
	}

	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return
	}

	// Determine if this is a DM or channel message.
	isDM := strings.HasPrefix(ev.Channel, "D")

	// Channel filter: if configured, only respond in listed channels (DMs always allowed).
	if !isDM && len(a.cfg.Channels) > 0 && !a.channelAllowed(ev.Channel) {
		return
	}

	// Access control. When a resolver is wired (the normal path), every
	// Slack identity must map to an approved user in the users table.
	// Unknown DMs auto-create a pending request; everything else bounces
	// with a status-appropriate reply. Channel mentions from unapproved
	// users stay silent so we don't pollute public channels with access
	// denial spam.
	replyTS := ev.ThreadTimeStamp
	if replyTS == "" && !isDM {
		replyTS = ev.TimeStamp
	}
	// canonicalUserID is the approved Familiar user behind this Slack
	// sender, captured here so the conversation-persistence path below
	// can own its rows under the same identity the pipeline tags facts
	// with (and the scheduled deliverer keys its DM digest by).
	var canonicalUserID string
	if a.resolver != nil {
		canonical, status, linked := a.resolver.ResolveWithStatus("slack", ev.User)
		canonicalUserID = canonical
		switch {
		case !linked:
			// Unknown Slack identity. Auto-provisioning on first DM was
			// removed — Slack is no longer an onboarding surface, and a
			// reworked onboarding flow is coming. An unrecognized user
			// gets one brief notice in a DM and no access; channel
			// messages stay silent so we don't spam public channels.
			if isDM {
				a.postRaw(ev.Channel, replyTS, "I don't recognize this account yet. Ask the admin to set you up in Familiar.")
			}
			return
		case status == identity.StatusPending:
			if isDM {
				a.postRaw(ev.Channel, replyTS, "Your request is still pending admin approval. You'll hear from me once it's granted.")
			}
			return
		case status == identity.StatusDenied:
			if isDM {
				a.postRaw(ev.Channel, replyTS, "Your previous access request was denied. Contact the admin directly if you think this is a mistake.")
			}
			return
		case status == identity.StatusDisabled:
			if isDM {
				a.postRaw(ev.Channel, replyTS, "This account has been disabled. Contact the admin.")
			}
			return
		case status != identity.StatusApproved:
			log.Printf("[slack] unexpected status %q for %s, bouncing", status, ev.User)
			return
		}
	} else if !a.userAllowed(ev.User) {
		// Legacy path for deployments without a resolver: fall back to
		// the AllowedUsers config gate so existing config.toml files
		// still work.
		log.Printf("[slack] access denied for user %s in %s", ev.User, ev.Channel)
		a.postRaw(ev.Channel, replyTS, "I only respond to my owner.")
		return
	}

	// In channels, only respond when @mentioned.
	mentionPrefix := fmt.Sprintf("<@%s>", a.botID)
	mentioned := strings.Contains(text, mentionPrefix)

	if !isDM && !mentioned {
		return
	}

	// Strip the bot mention from the message.
	text = strings.ReplaceAll(text, mentionPrefix, "")
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	// Threading per spec §1.4:
	//   - DMs: reply inline, no thread.
	//   - Existing thread: reply into the thread.
	//   - Top-level channel mention: start a thread anchored to the message
	//     so we don't pollute the channel's main timeline.
	if isDM {
		replyTS = ""
	}

	// Resolve the session identity. Two modes:
	//
	//   Durable (conv store wired + an approved user behind the
	//   sender): the session id IS a stable conversation UUID, one per
	//   DM (per user) or per thread. Hydration replays that
	//   conversation's history every turn — including anything a
	//   scheduled action posted into it — so the bot can continue
	//   where a digest left off. The user prompt is persisted before
	//   the turn and the reply after, mirroring the workspace's chat
	//   flow so the row sequence (user → tool middle → assistant) is
	//   intact across restarts.
	//
	//   Stateless (no conv store, or no resolved user): the legacy
	//   spec §1.8 hash id. DMs roll over daily; threads stay stable.
	//   No DB persistence — the in-memory session buffer is all the
	//   history there is, exactly as before.
	var (
		sessionID string
		convID    string
	)
	if a.convs != nil && canonicalUserID != "" {
		extKey, title := slackExternalKey(canonicalUserID, ev.Channel, replyTS, isDM)
		cid, err := a.convs.EnsureExternalConversation(ctx, canonicalUserID, extKey, title)
		if err != nil {
			log.Printf("[slack] ensure conversation for %s: %v — falling back to stateless session", ev.User, err)
		} else {
			convID = cid
			sessionID = cid
		}
	}
	if sessionID == "" {
		sessionID = slackSessionID(ev.User, replyTS, isDM)
	}
	sess := a.sessions.GetOrCreateWithID(sessionID, "slack:"+ev.Channel, ev.User)
	sess.SetPlatform("slack")
	// Pin the resolved identity so the pipeline doesn't re-resolve and
	// so fact attribution / the conversation owner agree (resolveIdentity
	// is a no-op once CanonicalID is set).
	if canonicalUserID != "" {
		sess.SetCanonicalID(canonicalUserID)
	}

	log.Printf("[slack] message from %s in %s (sid=%s): %s", ev.User, ev.Channel, sessionID, truncate(text, 80))

	// Persist the user prompt before the turn so the row order matches
	// the LLM-side sequence and a mid-turn restart can still resume.
	if convID != "" {
		if err := a.convs.AppendMessage(ctx, convID, "user", text, ""); err != nil {
			log.Printf("[slack] persist user message (continuing): %v", err)
		}
	}

	// Process through the pipeline.
	response, info, err := a.pipeline.Handle(ctx, sess, text, nil)
	if err != nil {
		log.Printf("[slack] pipeline error for %s: %v", ev.User, err)
		a.postRaw(ev.Channel, replyTS, fmt.Sprintf("Sorry, I encountered an error: %v", err))
		return
	}

	// Persist the final reply after the turn (the pipeline already
	// wrote any intermediate tool messages keyed by the same conv id).
	if convID != "" && strings.TrimSpace(response) != "" {
		model := ""
		if info != nil {
			model = info.ModelID
		}
		if err := a.convs.AppendMessage(ctx, convID, "assistant", response, model); err != nil {
			log.Printf("[slack] persist assistant message (continuing): %v", err)
		}
	}

	if a.verbose && info != nil {
		parts := []string{"via " + info.ModelID}
		if info.MemHits > 0 {
			parts = append(parts, fmt.Sprintf("%d memory hits", info.MemHits))
		}
		log.Printf("[slack] response metadata: %s", strings.Join(parts, ", "))
	}

	// Convert engine output to Slack mrkdwn and deliver.
	a.postMessage(ev.Channel, replyTS, response)
}

// slackExternalKey is the durable conversation key for a Slack
// surface. DMs map to ONE conversation per user (stable across days —
// the bot remembers the DM), so the key embeds the canonical user id,
// which is also what the scheduled-action slack_dm deliverer keys its
// digest by, so a reply lands in the same conversation the digest was
// posted into. Threads map per (channel, thread_ts). The title is only
// applied when the conversation is first created.
func slackExternalKey(canonicalUserID, channel, threadTS string, isDM bool) (key, title string) {
	if isDM {
		return DMExternalKey(canonicalUserID), "Slack DM"
	}
	return "slack:thread:" + channel + ":" + threadTS, "Slack thread"
}

// DMExternalKey is the durable conversation key for a user's Slack DM
// with the bot. Exported so the scheduled-action slack_dm deliverer
// writes its digest into the SAME conversation the user's next DM
// reply will hydrate — the two paths must agree on this string.
func DMExternalKey(canonicalUserID string) string {
	return "slack:dm:" + canonicalUserID
}

// slackSessionID computes the session identity per spec §1.8. For DMs
// the bucket rotates on UTC date; for thread replies the bucket is the
// thread timestamp, stable for the life of the thread.
func slackSessionID(userID, threadTS string, isDM bool) string {
	var seed string
	if isDM || threadTS == "" {
		seed = "slack:" + userID + ":" + time.Now().UTC().Format("2006-01-02")
	} else {
		seed = "slack:" + userID + ":" + threadTS
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}

// slackDisplayName fetches a human-readable name for a Slack user id
// via the API. Returns the raw id as a fallback when the lookup fails
// so a new pending user always has *some* label in the admin UI.
func (a *SlackAdapter) slackDisplayName(userID string) string {
	if a.api == nil {
		return userID
	}
	u, err := a.api.GetUserInfo(userID)
	if err != nil || u == nil {
		return userID
	}
	if u.RealName != "" {
		return u.RealName
	}
	if u.Profile.DisplayName != "" {
		return u.Profile.DisplayName
	}
	if u.Name != "" {
		return u.Name
	}
	return userID
}

// userAllowed returns true when the configured allowlist is empty or
// the user id is present in it.
func (a *SlackAdapter) userAllowed(userID string) bool {
	if len(a.cfg.AllowedUsers) == 0 {
		return true
	}
	for _, u := range a.cfg.AllowedUsers {
		if u == userID {
			return true
		}
	}
	return false
}

// postMessage converts `text` from the engine's CommonMark dialect to
// Slack mrkdwn and delivers it, splitting into multiple messages if the
// content exceeds Slack's 4000-character limit.
func (a *SlackAdapter) postMessage(channel, threadTS, text string) {
	a.postRaw(channel, threadTS, toMrkdwn(text))
}

// postRaw delivers a message verbatim, skipping mrkdwn conversion. Used
// for adapter-originated status text (access denied, error traceback)
// where Markdown transforms would only get in the way.
func (a *SlackAdapter) postRaw(channel, threadTS, text string) {
	// Never post a blank message. A model turn that produced only
	// protocol noise (now scrubbed to "") would otherwise become an
	// empty Slack post — Slack rejects it, and it's meaningless to the
	// user regardless.
	if strings.TrimSpace(text) == "" {
		return
	}
	chunks := splitMessage(text, maxMessageLen)
	for _, chunk := range chunks {
		opts := []slack.MsgOption{
			slack.MsgOptionText(chunk, false),
		}
		if threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}

		_, _, err := a.api.PostMessage(channel, opts...)
		if err != nil {
			log.Printf("[slack] error posting message to %s: %v", channel, err)
			return
		}
	}
}

// channelAllowed checks if a channel is in the allowed list.
func (a *SlackAdapter) channelAllowed(channel string) bool {
	for _, ch := range a.cfg.Channels {
		if ch == channel {
			return true
		}
	}
	return false
}

// splitMessage breaks text into chunks of at most maxLen characters,
// preferring to split at newline boundaries.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find the last newline within the limit.
		cutPoint := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx > 0 {
			cutPoint = idx + 1
		}

		chunks = append(chunks, text[:cutPoint])
		text = text[cutPoint:]
	}

	return chunks
}

// truncate shortens a string for logging.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ──────────────────────────────────────────────────────────────────
// Phase-5 Slack auto-bootstrap
// ──────────────────────────────────────────────────────────────────

// Slack auto-provisioning (bootstrapSlackUser / chooseDisplayName /
// postWelcome) was removed — Slack is no longer an onboarding surface.
// An unknown DM now gets a brief notice and no account (see
// handleMessage). The resolver still exposes BootstrapSlackUser as a
// reusable primitive for the upcoming onboarding rework.
