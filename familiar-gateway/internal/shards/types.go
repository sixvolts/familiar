// Package shards is the Phase 1 shard registry: typed Shard/Token
// records plus a Store backed by the shared gateway Postgres pool.
// Higher layers (admin UI, shard invoke endpoint) build on this
// package — the types here are the wire between them.
//
// Design notes for downstream consumers:
//
//   - A Shard is an authorization envelope, not an agent. It has no
//     personality of its own; it has a prompt, a memory scope, a tool
//     allowlist, and a persistence/visibility mode.
//
//   - Tokens are 1:1 with shards. A token's plaintext is returned exactly
//     once from CreateToken; the store keeps a bcrypt hash and an 8-char
//     prefix for UI disambiguation only. ValidateToken re-derives the
//     prefix, looks up candidate rows, and bcrypt-compares.
//
//   - The store enforces a small set of integrity rules the DB alone
//     can't (slug format, enum membership, ephemeral-shards-can't-write).
//     Everything else (FK, UNIQUE, CHECK) is schema-level.
package shards

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// PersistenceMode controls whether a shard keeps sessions and writes
// memory. `ephemeral` shards run pure request/response; `persistent`
// shards accumulate state across invocations.
type PersistenceMode string

const (
	PersistencePersistent PersistenceMode = "persistent"
	PersistenceEphemeral  PersistenceMode = "ephemeral"
)

// Valid reports whether m is one of the two allowed modes.
func (m PersistenceMode) Valid() bool {
	return m == PersistencePersistent || m == PersistenceEphemeral
}

// VisibilityMode controls whether memory written by a shard is surfaced
// to the top-level Familiar retrieval view (`promoted`) or hidden from
// it (`isolated`). There is no default — callers must choose.
type VisibilityMode string

const (
	VisibilityIsolated VisibilityMode = "isolated"
	VisibilityPromoted VisibilityMode = "promoted"
)

// Valid reports whether v is one of the two allowed modes.
func (v VisibilityMode) Valid() bool {
	return v == VisibilityIsolated || v == VisibilityPromoted
}

// Shard is the canonical Go representation of a row in the `shards`
// table. ToolAllowlist is decoded from the JSONB column; InputSchema
// and OutputSchema are opaque JSON blobs (documentation only in Phase
// 1, not validated against requests or responses).
type Shard struct {
	ID              string
	OwnerID         string
	Name            string
	Description     string
	Persistence     PersistenceMode
	Visibility      VisibilityMode
	ScopeTag        string
	ToolAllowlist   []string
	SystemPrompt    string
	ModelPreference string
	TierPreference  string
	InputSchema     json.RawMessage
	OutputSchema    json.RawMessage
	MaxTokens       int
	Temperature     float32
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DisabledAt      *time.Time

	// SHARD-AUTH-SPEC Phase 1 — permission envelope.
	// ConsoleAccess: can a passkey enrolled on this shard sign in
	// to the browser console? Default false; Spec calls this out as
	// the explicit opt-in for headless / kiosk shards.
	// ConsolePanels: which panels render for a shard session. Empty
	// means "all of the owner's panels". Values: "books", "notes",
	// "chat", "memory", "shards", "dashboard".
	// BookAccess: book IDs this shard can address through any
	// surface. Empty means "all of the owner's memberships". The
	// session loader intersects against the owner's actual
	// memberships at session-mint time.
	// ChatEnabled / ApiEnabled: surface-level kill switches. A
	// chat-disabled shard has no chat tab; an api-disabled shard
	// can't be invoked through bearer tokens (existing token
	// machinery still mints, but invocation refuses).
	// SessionMaxAge: per-shard cookie TTL override in seconds.
	// nil falls back to the gateway-wide default. Lets a kitchen-
	// iPad shard sit logged in for 24h while a high-security
	// shard rotates every 30 minutes.
	ConsoleAccess bool
	ConsolePanels []string
	BookAccess    []string
	ChatEnabled   bool
	APIEnabled    bool
	SessionMaxAge *int
}

// Active reports whether the shard is currently invocable. Disabled
// shards persist for audit but cannot be invoked.
func (s *Shard) Active() bool { return s.DisabledAt == nil }

// Token is the canonical Go representation of a row in the
// `shard_tokens` table. The plaintext bearer value is never stored on
// this struct; CreateToken returns it out-of-band exactly once.
type Token struct {
	ID          string
	ShardID     string
	OwnerID     string
	Label       string
	TokenPrefix string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

// Active reports whether the token is currently valid for invocation.
// Phase 1 does not enforce ExpiresAt — that check lands with the UI
// for expiration configuration; the schema column is present so no
// migration is required when enforcement turns on.
func (t *Token) Active() bool { return t.RevokedAt == nil }

// Sentinel errors let callers map failures to HTTP status without
// string-matching. The shard invoke endpoint translates these to
// 401/403/404/410 per spec.
var (
	ErrShardNotFound   = errors.New("shards: shard not found")
	ErrShardDisabled   = errors.New("shards: shard disabled")
	ErrTokenNotFound   = errors.New("shards: token not found")
	ErrTokenRevoked    = errors.New("shards: token revoked")
	ErrTokenMismatch   = errors.New("shards: token does not match shard")
	ErrInvalidSlug     = errors.New("shards: invalid slug")
	ErrInvalidScopeTag = errors.New("shards: invalid scope_tag")
	ErrInvalidMode     = errors.New("shards: invalid persistence or visibility mode")
	ErrWriteToolOnEph  = errors.New("shards: ephemeral shards cannot allowlist write-capable tools")
	ErrUnknownTool     = errors.New("shards: tool not registered")
)

// slugRE is the allowed shape for shard IDs: lowercase alphanumeric
// plus hyphens, must start with alphanumeric, max 63 chars. This
// matches the public-facing URL segment in /v1/shards/{id}/invoke
// so callers can rely on it for routing hygiene.
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateSlug enforces the public slug format. Returned error wraps
// ErrInvalidSlug so handlers can map it to 400/422 cleanly.
func ValidateSlug(s string) error {
	if !slugRE.MatchString(s) {
		return fmt.Errorf("%w: %q", ErrInvalidSlug, s)
	}
	return nil
}

// ValidateScopeTag rejects empty values and anything obviously unsafe
// for a memory scope label. It is intentionally lenient on content —
// operators choose the label convention (`shard:ticket-triage`,
// `ticket-triage-v2`, etc.).
func ValidateScopeTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("%w: empty", ErrInvalidScopeTag)
	}
	if len(tag) > 128 {
		return fmt.Errorf("%w: too long (%d > 128)", ErrInvalidScopeTag, len(tag))
	}
	for _, r := range tag {
		if r == 0 || r == '\n' || r == '\r' || r == '\t' {
			return fmt.Errorf("%w: contains control character", ErrInvalidScopeTag)
		}
	}
	return nil
}

// writeCapableMemoryTools are the tool names that mutate memory. An
// ephemeral shard's allowlist is rejected if it contains any of these.
// Names are bare (matching the globally-unique convention the skill
// registry enforces — see internal/skills.Registry.Register), not
// dotted <skill>.<tool> form; the admin UI may display them dotted
// for readability but storage and dispatch use bare names.
//
// Kept in this package (rather than pulled from the skill registry) so
// validation is a pure data check with no import cycle. If the memory
// skill ever adds a new write-capable tool, update this list in
// lockstep — ephemeral-shard safety depends on it being complete.
var writeCapableMemoryTools = map[string]bool{
	"save_fact":    true, // persists a fact to long-term memory
	"remember":     true, // explicit user-requested remember
	"forget_fact":  true, // deletes a memory
	"correct_fact": true, // updates an existing memory
}

// IsWriteCapable reports whether a tool name is known to mutate memory.
// Exported so the admin UI can surface the same answer the backend
// uses (checkbox disabling for ephemeral shards).
func IsWriteCapable(tool string) bool { return writeCapableMemoryTools[tool] }

// ValidateAllowlist enforces:
//   - no duplicates (gives a clearer error than a UNIQUE constraint ever could)
//   - no write-capable tools for ephemeral shards
//
// Membership against the live skill registry is not checked here; the
// caller (admin handler) supplies a `known` set and this function
// rejects unknowns if `known` is non-nil. Passing nil skips the
// registry check, which the shard invoke path relies on so a tool
// removed from the gateway after a shard was created doesn't wedge
// invocation — it just gets skipped at dispatch with a warn log.
func ValidateAllowlist(tools []string, persistence PersistenceMode, known map[string]bool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if _, dup := seen[t]; dup {
			return fmt.Errorf("shards: duplicate tool in allowlist: %q", t)
		}
		seen[t] = struct{}{}
		if persistence == PersistenceEphemeral && writeCapableMemoryTools[t] {
			return fmt.Errorf("%w: %q", ErrWriteToolOnEph, t)
		}
		if known != nil && !known[t] {
			// Common SQL/script-author mistake: copying the spec's
			// older `<skill>.<tool>` examples and inserting dotted
			// names. The registry indexes bare names (uniqueness is
			// enforced on the bare form), so a dotted lookup
			// silently misses. Surface a targeted hint when we see
			// the dotted shape so the caller doesn't have to spelunk
			// through registry source.
			if strings.Contains(t, ".") {
				return fmt.Errorf("%w: %q — tool names should be the bare registry name (e.g. %q not %q); the dotted <skill>.<tool> form is a display convention, not a storage format",
					ErrUnknownTool, t, stripSkillPrefix(t), t)
			}
			return fmt.Errorf("%w: %q", ErrUnknownTool, t)
		}
	}
	return nil
}

// stripSkillPrefix returns the substring after the first dot, or the
// input unchanged when there's no dot. Used only by the allowlist
// hint above — keeps the suggestion ("did you mean save_fact?")
// constructive when the caller sent "memory.save_fact".
func stripSkillPrefix(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	return s
}
