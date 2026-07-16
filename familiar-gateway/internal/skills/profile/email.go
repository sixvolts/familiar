// Package profile hosts user-profile-level skills that let the bot
// maintain self-service metadata about the caller. The first tool is
// update_my_email — the Slack bootstrap flow (Phase 5 of
// FAMILIAR-USER-CONSOLE-SPEC) welcomes users whose Slack profile
// didn't expose an email with a request to reply with one, and this
// skill is how that reply becomes a real users.email + openai
// identity_map row.
//
// Scope is intentionally narrow: the tool updates *only the caller's
// own* email, rejects if an email is already set (changes are an
// admin operation, not a self-service one), and validates the
// email shape so we don't poison the cross-platform identity key
// with garbage. More profile fields can land here later behind the
// same one-skill-per-field pattern.
package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/familiar/gateway/internal/skills"
)

// EmailUpdater is the subset of identity.Resolver the skill needs.
// Kept as a local interface so the package doesn't pull in the full
// resolver surface (and tests can swap a fake without standing up a
// real DB).
type EmailUpdater interface {
	SetUserEmail(ctx context.Context, userID, email string) error
}

// Skill exposes update_my_email.
type Skill struct {
	updater EmailUpdater
}

// New builds the skill. updater may be nil, in which case Execute
// returns a configuration error — matches the pattern other skills
// use for optional backends (memory, instance).
func New(updater EmailUpdater) *Skill {
	return &Skill{updater: updater}
}

func (s *Skill) Name() string { return "profile" }
func (s *Skill) Description() string {
	return "User profile self-service (email binding for cross-platform identity)"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

var updateMyEmailParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "email": {
      "type": "string",
      "description": "The caller's email address. Must be a valid RFC 5322 address."
    }
  },
  "required": ["email"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name: "update_my_email",
			Description: "Bind the caller's canonical user to an email address. " +
				"Call this when the user explicitly states their email (typical after " +
				"a Slack bootstrap welcome asked them to provide one). The email is " +
				"also added to the openai identity_map so the web chat recognizes " +
				"them automatically on their next sign-in. Fails if an email is " +
				"already set on the account — changes to an existing email are an " +
				"admin operation, not a self-service one.",
			Parameters: updateMyEmailParams,
		},
	}
}

type updateMyEmailArgs struct {
	Email string `json:"email"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if toolName != "update_my_email" {
		return skills.ToolResult{Error: "unknown tool: " + toolName}, nil
	}
	if s.updater == nil {
		return skills.ToolResult{Error: "profile: email updater not configured"}, nil
	}

	var args updateMyEmailArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}

	email := strings.TrimSpace(args.Email)
	if email == "" {
		return skills.ToolResult{Error: "email is required"}, nil
	}
	// Parse with net/mail so we reject obviously-bogus input (no @,
	// trailing punctuation from a Slack message copy) before
	// touching the DB.
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return skills.ToolResult{Error: "not a valid email address: " + err.Error()}, nil
	}
	// net/mail accepts "Name <addr>" — normalize to the bare address.
	email = addr.Address

	sc, ok := skills.ContextFrom(ctx)
	if !ok || sc.UserID == "" {
		// No canonical user attached to the call — the pipeline
		// always installs one, so this is a bug-in-caller signal
		// rather than a user-facing problem. Return a generic
		// error rather than leak the missing-context detail.
		return skills.ToolResult{Error: "cannot identify caller; email update refused"}, nil
	}

	if err := s.updater.SetUserEmail(ctx, sc.UserID, email); err != nil {
		// Map known error shapes to user-friendly messages. The
		// resolver returns a single "missing or already has an
		// email" error — we can't distinguish the two without a
		// separate probe, but surfacing the actionable phrase
		// covers both cases well.
		if errors.Is(err, errUserNotFound) ||
			strings.Contains(err.Error(), "already has an email") ||
			strings.Contains(err.Error(), "missing or already") {
			return skills.ToolResult{
				Content: fmt.Sprintf("I couldn't set %s — an email is already on file for this account, or the account isn't provisioned yet. Contact the admin if this looks wrong.", email),
				Tokens:  30,
			}, nil
		}
		// Unique-index violation: different user already has this
		// email. Friendly message keeps attacker enumeration low.
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			return skills.ToolResult{
				Content: fmt.Sprintf("I couldn't set %s — that address is already associated with another account. Contact the admin if you believe it should be yours.", email),
				Tokens:  30,
			}, nil
		}
		return skills.ToolResult{}, fmt.Errorf("update email: %w", err)
	}

	content := fmt.Sprintf("Linked %s to your account. The web chat will recognize you on your next sign-in.", email)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

// errUserNotFound is a sentinel the skill checks for to produce the
// "not provisioned yet" message. The resolver doesn't expose its own
// sentinel for SetUserEmail's missing-user case (it uses a generic
// wrapped error), so this is a placeholder that the errors.Is check
// currently never matches — kept so a future resolver refactor can
// swap in a real sentinel without churning the skill code.
var errUserNotFound = errors.New("profile: user not found")
