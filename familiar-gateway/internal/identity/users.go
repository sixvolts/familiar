package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UserStatus captures where a canonical user sits in the approval
// lifecycle. Every pipeline-gating decision reads from this value, so
// adapters can bounce pending / denied / disabled users without ever
// touching the LLM.
type UserStatus string

const (
	StatusPending  UserStatus = "pending"
	StatusApproved UserStatus = "approved"
	StatusDenied   UserStatus = "denied"
	StatusDisabled UserStatus = "disabled"
)

// User is the canonical record kept in the users table. display_name
// is what the admin UI shows; id is the value memories.user_id and the
// pipeline's CanonicalID point at.
//
// Role/Email/BootstrapSource were added in Phase 1 of the user-console
// spec. Role drives the admin-vs-user authz split (Phase 2). Email is
// the cross-platform identity key that Open WebUI and the admin
// console use to recognize a Slack-bootstrapped user (Phase 4-5).
// BootstrapSource records how the row was created — "slack",
// "first_run", "manual" — useful for audit and the admin user list.
type User struct {
	ID              string
	DisplayName     string
	Status          UserStatus
	Role            string  // "admin" | "user" — defaults to "user" via DB constraint
	Email           *string // nullable; unique when set
	BootstrapSource *string // "slack" | "first_run" | "manual" | nil
	CreatedAt       time.Time
	ApprovedAt      *time.Time
	ApprovedBy      string
}

// IdentityLink is one platform-specific handle pointing at a User.
type IdentityLink struct {
	Platform    string
	PlatformID  string
	UserID      string
	DisplayName string
	CreatedAt   time.Time
}

// ErrUserNotFound is returned by GetUser / UpdateUserStatus when the id
// does not exist. Callers (admin endpoints) surface it as 404.
var ErrUserNotFound = errors.New("identity: user not found")

// ErrDuplicateLink is returned by LinkIdentity when the (platform,
// platform_id) pair is already mapped to any user. The admin UI treats
// this as a soft error and shows the existing mapping so the operator
// can decide whether to delete it first.
var ErrDuplicateLink = errors.New("identity: platform link already exists")

// ResolveWithStatus returns the canonical ID for a platform identity
// *and* the user's current status. When no link exists the returned
// status is empty string and ok=false, letting the caller distinguish
// "unknown identity, start a registration flow" from "known identity
// but not approved yet".
func (r *Resolver) ResolveWithStatus(platform, platformID string) (canonicalID string, status UserStatus, ok bool) {
	if r == nil || platform == "" || platformID == "" {
		return "", "", false
	}
	key := platform + ":" + platformID
	r.mu.RLock()
	canonical, mapped := r.cache[key]
	var st UserStatus
	if mapped {
		st = r.userStatus[canonical]
	}
	r.mu.RUnlock()
	if !mapped {
		return "", "", false
	}
	return canonical, st, true
}

// userColumns lists every column GetUser / ListUsers select. Defined
// once so adding a new column is a one-line edit instead of two
// matching SELECTs that drift apart.
const userColumns = `id, display_name, status, role, email, bootstrap_source,
                     created_at, approved_at, approved_by`

// scanUser reads one row produced by `SELECT userColumns FROM users …`
// into a User. Centralizes the Null* handling so the GetUser /
// ListUsers paths stay in lockstep.
func scanUser(s interface{ Scan(...any) error }) (User, error) {
	var u User
	var role sql.NullString
	var email sql.NullString
	var bootstrap sql.NullString
	var approvedAt sql.NullTime
	var approvedBy sql.NullString
	if err := s.Scan(
		&u.ID, &u.DisplayName, &u.Status, &role, &email, &bootstrap,
		&u.CreatedAt, &approvedAt, &approvedBy,
	); err != nil {
		return User{}, err
	}
	if role.Valid {
		u.Role = role.String
	} else {
		u.Role = "user" // matches the DB column default
	}
	if email.Valid {
		v := email.String
		u.Email = &v
	}
	if bootstrap.Valid {
		v := bootstrap.String
		u.BootstrapSource = &v
	}
	if approvedAt.Valid {
		t := approvedAt.Time
		u.ApprovedAt = &t
	}
	if approvedBy.Valid {
		u.ApprovedBy = approvedBy.String
	}
	return u, nil
}

// GetUser loads a single user by canonical id. Returns ErrUserNotFound
// when absent. Used by the admin UI detail panel.
func (r *Resolver) GetUser(ctx context.Context, id string) (*User, error) {
	if r == nil || r.db == nil {
		return nil, ErrUserNotFound
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("identity: get user: %w", err)
	}
	return &u, nil
}

// ListUsers returns every user, optionally filtered to a status subset.
// Ordering is newest-first on created_at so pending requests surface at
// the top of the admin view.
func (r *Resolver) ListUsers(ctx context.Context, statuses []UserStatus) ([]User, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	query := `SELECT ` + userColumns + ` FROM users`
	var args []any
	if len(statuses) > 0 {
		placeholders := ""
		for i, s := range statuses {
			if i > 0 {
				placeholders += ","
			}
			placeholders += fmt.Sprintf("$%d", i+1)
			args = append(args, string(s))
		}
		query += " WHERE status IN (" + placeholders + ")"
	}
	query += " ORDER BY created_at DESC"
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListIdentitiesForUser returns every platform link pointing at a
// given user. Used by the admin UI to show "this user is linked to
// Slack U123 and OpenAI alice".
func (r *Resolver) ListIdentitiesForUser(ctx context.Context, userID string) ([]IdentityLink, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT platform, platform_id, canonical_id, COALESCE(display_name,''), created_at
		FROM identity_map
		WHERE canonical_id = $1
		ORDER BY platform, platform_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: list links: %w", err)
	}
	defer rows.Close()
	var out []IdentityLink
	for rows.Next() {
		var l IdentityLink
		if err := rows.Scan(&l.Platform, &l.PlatformID, &l.UserID, &l.DisplayName, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// (RegisterPending — auto-create a pending user from a platform DM —
// was removed. Its only callers were the Slack auto-provision path
// (dropped: Slack is no longer an onboarding surface) and a never-built
// request-access flow. Onboarding is admin-invite only; an admin
// creates users via InviteUser. A `pending` user is still reachable by
// a manual status change, which doesn't need this primitive.)

// LinkIdentity attaches an additional platform identity (typically
// OpenAI / Open WebUI) to an already-approved user. Returns
// ErrDuplicateLink if the (platform, platform_id) pair is already
// pointing at any user — callers must unlink first if they want to
// re-home it.
func (r *Resolver) LinkIdentity(ctx context.Context, userID, platform, platformID, displayName string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if userID == "" || platform == "" || platformID == "" {
		return fmt.Errorf("identity: user_id, platform, platform_id required")
	}
	// Ensure the user exists so we don't orphan links.
	if _, err := r.GetUser(ctx, userID); err != nil {
		return err
	}
	// Refuse to overwrite an existing link silently.
	r.mu.RLock()
	_, existing := r.cache[platform+":"+platformID]
	r.mu.RUnlock()
	if existing {
		return ErrDuplicateLink
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (platform, platform_id) DO NOTHING`,
		platform, platformID, userID, displayName); err != nil {
		return fmt.Errorf("identity: link: %w", err)
	}
	r.mu.Lock()
	r.cache[platform+":"+platformID] = userID
	delete(r.warned, platform+":"+platformID)
	r.mu.Unlock()
	return nil
}

// UnlinkIdentity removes a (platform, platform_id) row. Used by the
// admin UI to detach stray mappings. The underlying user is left
// untouched.
func (r *Resolver) UnlinkIdentity(ctx context.Context, platform, platformID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM identity_map WHERE platform = $1 AND platform_id = $2`,
		platform, platformID); err != nil {
		return fmt.Errorf("identity: unlink: %w", err)
	}
	r.mu.Lock()
	delete(r.cache, platform+":"+platformID)
	r.mu.Unlock()
	return nil
}

// SetUserStatus transitions a user to a new status. approver is the
// admin's webauthn user_id and is recorded on approval so the audit
// trail shows who granted access. The cache is updated in place so
// adapters see the new status on the next request without a reload.
func (r *Resolver) SetUserStatus(ctx context.Context, userID string, status UserStatus, approver string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	switch status {
	case StatusPending, StatusApproved, StatusDenied, StatusDisabled:
	default:
		return fmt.Errorf("identity: invalid status %q", status)
	}
	var (
		res sql.Result
		err error
	)
	if status == StatusApproved {
		res, err = r.db.ExecContext(ctx, `
			UPDATE users
			   SET status = $1, approved_at = NOW(), approved_by = $2
			 WHERE id = $3`,
			string(status), approver, userID)
	} else {
		res, err = r.db.ExecContext(ctx, `
			UPDATE users SET status = $1 WHERE id = $2`,
			string(status), userID)
	}
	if err != nil {
		return fmt.Errorf("identity: set status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	r.mu.Lock()
	r.userStatus[userID] = status
	r.mu.Unlock()
	return nil
}

// SearchUsers returns up to `limit` users whose id, display_name,
// or email starts with `query` (case-insensitive). Used by the
// any-role lookup endpoint that powers in-app pickers (e.g. the
// book-members modal). Empty query → empty result so the endpoint
// can never accidentally list everyone. limit clamps to [1, 25].
func (r *Resolver) SearchUsers(ctx context.Context, query string, limit int) ([]User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("identity: nil resolver")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 25 {
		limit = 25
	}
	pattern := q + "%"
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+userColumns+`
		   FROM users
		  WHERE id ILIKE $1
		     OR display_name ILIKE $1
		     OR email ILIKE $1
		  ORDER BY (LOWER(id) = LOWER($2)) DESC,
		           (LOWER(email) = LOWER($2)) DESC,
		           (LOWER(display_name) = LOWER($2)) DESC,
		           created_at DESC
		  LIMIT $3`, pattern, q, limit)
	if err != nil {
		return nil, fmt.Errorf("identity: search users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetByEmail looks up a user by email. Returns (nil, nil) when no
// user carries that email — distinct from ErrUserNotFound so the
// caller (registerBegin's email-keyed lookup) can distinguish
// "unknown email, reject" from a genuine DB error. Email comparison
// uses the DB's default case/ collation — operators pasting mixed
// case match exactly what identity_map stored.
func (r *Resolver) GetByEmail(ctx context.Context, email string) (*User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("identity: nil resolver")
	}
	if email == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`, email)
	u, err := scanUser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("identity: get by email: %w", err)
	}
	return &u, nil
}

// DeriveCanonicalID turns a human display name into a lowercase,
// underscore-separated candidate canonical id: "Sam Smith" →
// "sam_smith", "Al--Green 😀" → "al_green". Non-alphanumerics
// (including punctuation, emoji, accents) are either collapsed to
// "_" when they're a separator or dropped. Collapses runs of "_"
// and trims leading/trailing underscores. Returns "user" for
// all-symbol inputs so the dedup loop always has something to
// increment.
func DeriveCanonicalID(display string) string {
	lower := strings.ToLower(display)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range lower {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastUnderscore = false
		case r == ' ' || r == '_' || r == '-' || r == '.':
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		default:
			// drop
		}
	}
	id := strings.Trim(b.String(), "_")
	if id == "" {
		return "user"
	}
	return id
}

// BootstrapSlackUser auto-provisions a brand-new canonical user on
// first contact from Slack — the user lands as status=approved,
// role=user, bootstrap_source=slack. NOTE: the Slack adapter no longer
// calls this (Slack was dropped as an onboarding surface); it's kept as
// a reusable primitive in case a future onboarding flow wants it.
//
// displayName should be the preferred human label (real name first,
// display name fallback). email may be empty when the workspace
// doesn't expose the address; the caller is expected to follow up
// with the update_my_email skill on the user's next message if a
// cross-platform link is needed. All inserts run in one transaction
// so a partial provision never leaves the user half-registered.
//
// Canonical id is derived from displayName and deduplicated against
// the users table — "Sam Smith" becomes "sam_smith", or "sam_smith_2"
// if that's taken.
func (r *Resolver) BootstrapSlackUser(ctx context.Context, slackID, displayName, email string) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("identity: nil resolver")
	}
	if slackID == "" {
		return "", fmt.Errorf("identity: bootstrap slack: slackID required")
	}
	if displayName == "" {
		displayName = slackID
	}

	candidate := DeriveCanonicalID(displayName)
	canonical, err := r.dedupeCanonicalID(ctx, candidate)
	if err != nil {
		return "", err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("identity: bootstrap slack: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var emailVal any
	if email != "" {
		emailVal = email
	} else {
		emailVal = nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, display_name, email, status, role, bootstrap_source, approved_at)
		VALUES ($1, $2, $3, 'approved', 'user', 'slack', NOW())`,
		canonical, displayName, emailVal); err != nil {
		return "", fmt.Errorf("identity: bootstrap slack: insert user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
		VALUES ('slack', $1, $2, $3)
		ON CONFLICT (platform, platform_id) DO NOTHING`,
		slackID, canonical, displayName); err != nil {
		return "", fmt.Errorf("identity: bootstrap slack: insert slack link: %w", err)
	}
	if email != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
			VALUES ('openai', $1, $2, $3)
			ON CONFLICT (platform, platform_id) DO NOTHING`,
			email, canonical, displayName); err != nil {
			return "", fmt.Errorf("identity: bootstrap slack: insert openai link: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("identity: bootstrap slack: commit: %w", err)
	}

	// Update in-memory caches so the caller's immediate re-resolve
	// picks the new state up without a round-trip.
	r.mu.Lock()
	r.cache["slack:"+slackID] = canonical
	if email != "" {
		r.cache["openai:"+email] = canonical
	}
	r.userStatus[canonical] = StatusApproved
	r.userName[canonical] = displayName
	delete(r.warned, "slack:"+slackID)
	r.mu.Unlock()
	return canonical, nil
}

// dedupeCanonicalID returns candidate when it's unused in the users
// table, or candidate_2 / candidate_3 / ... for the first available
// suffix. Falls back to a time-suffixed id after 100 attempts (only
// reachable if someone is pathological about naming collisions).
func (r *Resolver) dedupeCanonicalID(ctx context.Context, candidate string) (string, error) {
	id := candidate
	for i := 2; i < 100; i++ {
		var exists bool
		err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists)
		if err != nil {
			return "", fmt.Errorf("identity: dedupe: %w", err)
		}
		if !exists {
			return id, nil
		}
		id = fmt.Sprintf("%s_%d", candidate, i)
	}
	return fmt.Sprintf("%s_%d", candidate, time.Now().Unix()), nil
}

// SetUserEmail is the write side of the update_my_email skill. Sets
// users.email for a user whose row currently has no email, and
// inserts a matching openai identity_map row so cross-platform
// routing (Open WebUI → canonical user) works on the next request.
// Refuses when the user already has an email set — changes to an
// existing email are an admin operation, not a self-service one.
func (r *Resolver) SetUserEmail(ctx context.Context, userID, email string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if userID == "" {
		return fmt.Errorf("identity: set email: userID required")
	}
	if email == "" {
		return fmt.Errorf("identity: set email: email required")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: set email: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Require the current email to be NULL — overwriting an existing
	// email is an admin operation (race between Slack profile update
	// and self-serve skill), not something a user tool should do
	// silently.
	res, err := tx.ExecContext(ctx, `
		UPDATE users SET email = $1 WHERE id = $2 AND email IS NULL`,
		email, userID)
	if err != nil {
		return fmt.Errorf("identity: set email: update users: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the user doesn't exist, or already has an email set.
		// Both should fail with a useful message; the skill distinguishes
		// via a follow-up GetUser check at its call site.
		return fmt.Errorf("identity: set email: user %q missing or already has an email", userID)
	}

	// Add the openai identity_map row so Open WebUI's email header
	// routes to this canonical user. Silent on duplicate (someone
	// else's row with the same email would have failed the unique
	// index on users.email above, so duplicates here are re-runs).
	displayName := r.userName[userID]
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity_map (platform, platform_id, canonical_id, display_name)
		VALUES ('openai', $1, $2, $3)
		ON CONFLICT (platform, platform_id) DO NOTHING`,
		email, userID, displayName); err != nil {
		return fmt.Errorf("identity: set email: insert openai link: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: set email: commit: %w", err)
	}

	r.mu.Lock()
	r.cache["openai:"+email] = userID
	r.mu.Unlock()
	return nil
}

// Refresh reloads identity_map + user status from the database. Used
// after transactional provisioning paths (Slack bootstrap) that want
// to confirm the cache is coherent, and available to admin tooling
// that edits the DB out-of-band.
func (r *Resolver) Refresh(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.reload(ctx)
}

// CreateFirstRun seeds the very first admin row on a fresh deploy.
// Called from registerBegin's first-run branch (when the credentials
// table is empty) — subsequent users register through Slack or email
// lookup, not this path. Stamps status=approved and role=admin so the
// new owner is immediately usable without a separate approval flow.
//
// Idempotent on the row's id: registerBegin inserts this row BEFORE
// the WebAuthn ceremony completes, so an abandoned/failed ceremony
// leaves a credential-less first_run row behind. Without the upsert,
// the next attempt (credentials still empty → still the first-run
// branch) collided on users_pkey and bricked the fresh deploy forever.
// The upsert re-asserts the intended values and lets the retry finish.
// A collision is only reachable here on the credentials-empty fresh
// path, so it can only be that same abandoned first_run row.
func (r *Resolver) CreateFirstRun(ctx context.Context, id, displayName, email string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if id == "" {
		return fmt.Errorf("identity: create first run: id required")
	}
	if email == "" {
		return fmt.Errorf("identity: create first run: email required")
	}
	var emailVal any = email
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO users (id, display_name, email, status, role, bootstrap_source, approved_at)
		VALUES ($1, $2, $3, 'approved', 'admin', 'first_run', NOW())
		ON CONFLICT (id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			email        = EXCLUDED.email,
			status       = 'approved',
			role         = 'admin'`,
		id, displayName, emailVal)
	if err != nil {
		return fmt.Errorf("identity: create first run: %w", err)
	}
	// Keep caches in sync so the next request recognizes the row
	// without waiting for a full reload.
	r.mu.Lock()
	r.userStatus[id] = StatusApproved
	r.userName[id] = displayName
	r.mu.Unlock()
	return nil
}

// InviteUser is the high-level admin-invite path: derives a
// canonical id from displayName (falling back to the email local
// part when displayName is empty), dedupes against existing ids,
// inserts the user row, and returns the freshly-created *User so
// the caller can surface it (and, e.g., mint an enrollment-token
// link). The status is forced to approved; role defaults to "user"
// when blank.
//
// Returns ErrDuplicateLink when the email is already mapped to
// another user — admins should resolve that via the existing user
// detail flow rather than creating a parallel row.
func (r *Resolver) InviteUser(ctx context.Context, displayName, email, role string) (*User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("identity: nil resolver")
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, fmt.Errorf("identity: invite: email required")
	}
	if existing, err := r.GetByEmail(ctx, email); err == nil && existing != nil {
		return nil, ErrDuplicateLink
	}
	dn := strings.TrimSpace(displayName)
	if dn == "" {
		// Fall back to the email local part so the user row has a
		// usable label out of the gate. The user can rename via the
		// self-service profile editor.
		at := strings.IndexByte(email, '@')
		if at > 0 {
			dn = email[:at]
		} else {
			dn = email
		}
	}
	candidate := DeriveCanonicalID(dn)
	if candidate == "" {
		candidate = DeriveCanonicalID(email)
	}
	id, err := r.dedupeCanonicalID(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if err := r.CreateInvited(ctx, id, dn, email, role); err != nil {
		return nil, err
	}
	return r.GetUser(ctx, id)
}

// CreateInvited inserts a user row for an admin-initiated email
// invite. Mirrors CreateFirstRun's shape but takes role as an
// argument (default "user", "admin" optional) and stamps
// bootstrap_source='invite' so audit can tell invited users apart
// from Slack auto-provision and first-run owner.
//
// status is set to "approved" — the invite IS the approval, so the
// recipient can register a passkey and sign in immediately without
// a second admin step.
//
// Caller is responsible for handling the user-facing follow-up
// (typically: mint a cross-domain enrollment-token + share the URL
// with the new user). Returns the standard duplicate-link / err
// sentinels so callers can map them to 409 / 500 cleanly.
func (r *Resolver) CreateInvited(ctx context.Context, id, displayName, email, role string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if id == "" {
		return fmt.Errorf("identity: create invited: id required")
	}
	if email == "" {
		return fmt.Errorf("identity: create invited: email required")
	}
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		return fmt.Errorf("identity: create invited: role must be 'user' or 'admin' (got %q)", role)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO users (id, display_name, email, status, role, bootstrap_source, approved_at)
		VALUES ($1, $2, $3, 'approved', $4, 'invite', NOW())`,
		id, displayName, email, role)
	if err != nil {
		return fmt.Errorf("identity: create invited: %w", err)
	}
	// Keep caches in sync — subsequent registerBegin / GetByEmail
	// lookups should see the new row without waiting for a refresh.
	r.mu.Lock()
	r.userStatus[id] = StatusApproved
	r.userName[id] = displayName
	r.mu.Unlock()
	return nil
}

// SetUserRole writes the role column for an existing user. Caller
// (admin handler) is responsible for the last-admin guardrail —
// this method does not enforce "at least one admin must remain"
// so bulk-admin tools can bypass it when appropriate. Only valid
// role values are "admin" and "user"; anything else errors out.
func (r *Resolver) SetUserRole(ctx context.Context, userID, role string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	if role != "admin" && role != "user" {
		return fmt.Errorf("identity: invalid role %q (must be admin|user)", role)
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET role = $1 WHERE id = $2`, role, userID)
	if err != nil {
		return fmt.Errorf("identity: set role: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetUserDisplayName updates the human-readable label for a user.
// Used by the admin PATCH endpoint when the operator fixes a typo or
// switches to a preferred name.
func (r *Resolver) SetUserDisplayName(ctx context.Context, userID, displayName string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("identity: nil resolver")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET display_name = $1 WHERE id = $2`, displayName, userID)
	if err != nil {
		return fmt.Errorf("identity: set display_name: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	// Keep the in-memory name cache (used by pipeline logs / admin
	// dropdowns) in sync so the next request sees the new label.
	r.mu.Lock()
	r.userName[userID] = displayName
	r.mu.Unlock()
	return nil
}

// CountAdmins returns the number of users with role='admin'. Used by
// the admin handler's last-admin guard to refuse demotions that
// would lock everyone out.
func (r *Resolver) CountAdmins(ctx context.Context) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("identity: nil resolver")
	}
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("identity: count admins: %w", err)
	}
	return n, nil
}

// UserStatusOf looks up the cached status for a canonical user id.
// Returns empty string when the user is unknown to the cache.
func (r *Resolver) UserStatusOf(userID string) UserStatus {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.userStatus[userID]
}

// newUserID returns a short, opaque canonical id of the form
// "u_<6 hex>". Opaque on purpose — the display_name is the human label
// and the id only needs to be unique and stable.
func newUserID() (string, error) {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("identity: new id: %w", err)
	}
	return "u_" + hex.EncodeToString(buf[:]), nil
}
