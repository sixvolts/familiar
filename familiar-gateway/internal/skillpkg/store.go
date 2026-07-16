package skillpkg

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// capSkillBody bounds a SKILL.md body before it becomes a use_skill
// tool result in the LLM loop. Authored bodies are already capped at
// save time (maxSkillFileBytes), but an IMPORTED SKILL.md can be up to
// the multi-MB per-file zip limit, which would blow a turn's context
// window. A well-formed skill's SKILL.md is tiny — detail lives in
// referenced files, fetched via the separately-capped read_skill_file
// — so truncating a pathological one with a marker is safe. ToValidUTF8
// drops any partial rune the byte cut left behind.
func capSkillBody(body string) string {
	if len(body) <= maxSkillFileBytes {
		return body
	}
	truncated := strings.ToValidUTF8(body[:maxSkillFileBytes], "")
	return truncated + "\n\n[... SKILL.md truncated at " +
		strconv.Itoa(maxSkillFileBytes/1024) +
		"KB — move detail into reference files read via read_skill_file]"
}

var ErrNotFound = errors.New("skillpkg: not found")

// ErrBuiltinImmutable refuses destructive operations on builtin rows:
// deleting one is pointless (SyncBuiltins re-installs it on the next
// boot) and would silently resurrect, so the honest answer is "disable
// it instead". Handlers map this to a 400, not a 500.
var ErrBuiltinImmutable = errors.New("skillpkg: built-in skill — disable it instead; it re-syncs at boot")

// maxSkillFileBytes caps read_skill_file responses — references are
// documentation, not datasets.
const maxSkillFileBytes = 256 * 1024

// maxZipBytes / maxZipFileBytes bound import uploads. maxZipTotal and
// maxZipFiles bound the DECOMPRESSED output during extraction: without
// them a 20MB archive of highly-compressible entries could expand to
// many GB in the temp dir before the post-extraction quota check ran
// (a zip bomb). maxZipTotal matches the per-skill disk quota, since no
// legitimate skill package exceeds it.
const (
	maxZipBytes     = 20 << 20
	maxZipFileBytes = 5 << 20
	maxZipTotal     = maxSkillDirBytes
	maxZipFiles     = 2000
)

// USER-SKILLS-SPEC Phase A: per-user quotas + the reserved library
// subtree. User skills live under Root/users/<owner>/<name>; the
// "users" name is reserved at the instance level so an instance skill
// can never collide with (or shadow) the per-user subtree.
const (
	userSubdir       = "users"
	maxUserSkills    = 50
	maxSkillDirBytes = 5 << 20
)

// Package is one skill_packages row joined with what disk holds.
type Package struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Description     string      `json:"description"`
	Version         string      `json:"version,omitempty"`
	Digest          string      `json:"digest"`
	SignatureStatus string      `json:"signature_status"`
	Signer          string      `json:"signer,omitempty"`
	HasWasm         bool        `json:"has_wasm"`
	HasScripts      bool        `json:"has_scripts"`
	SourceURL       string      `json:"source_url,omitempty"`
	Frontmatter     Frontmatter `json:"frontmatter"`
	ToolsMatched    []string    `json:"tools_matched"`
	ToolsUnmatched  []string    `json:"tools_unmatched"`
	ImportedBy      string      `json:"imported_by"`
	ImportedAt      time.Time   `json:"imported_at"`
	DisabledAt      *time.Time  `json:"disabled_at,omitempty"`

	// USER-SKILLS-SPEC Phase A. OwnerID "" = instance-wide skill
	// (admin-managed, today's semantics); non-empty = private user
	// skill. Origin records provenance: 'authored' (workspace editor,
	// Phase C) vs 'imported' (zip/URL) vs 'builtin' (embedded in the
	// gateway binary, synced at boot — see builtin.go; ImportedBy
	// scans as "" for those, NULL in the row = shipped with the
	// gateway). ChatEnabled is the Phase B trusted-path opt-in.
	OwnerID     string `json:"owner_id,omitempty"`
	Origin      string `json:"origin"`
	ChatEnabled bool   `json:"chat_enabled"`
}

func (p *Package) Enabled() bool { return p.DisabledAt == nil }

// Store owns the skill_packages/shard_skills tables and the on-disk
// library under Root.
type Store struct {
	pool *db.Pool
	Root string
}

func NewStore(pool *db.Pool, root string) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("skillpkg: nil pool")
	}
	if root == "" {
		return nil, fmt.Errorf("skillpkg: empty root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("skillpkg: create root: %w", err)
	}
	return &Store{pool: pool, Root: root}, nil
}

const pkgCols = `
	id::text, name, description, version, digest, signature_status,
	COALESCE(signer, ''), has_wasm, has_scripts, source_url,
	frontmatter, tools_matched, tools_unmatched, COALESCE(imported_by, ''),
	imported_at, disabled_at, COALESCE(owner_id, ''), origin, chat_enabled`

func scanPackage(sc interface{ Scan(...any) error }) (*Package, error) {
	var p Package
	var fm, tm, tu []byte
	err := sc.Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.Digest,
		&p.SignatureStatus, &p.Signer, &p.HasWasm, &p.HasScripts,
		&p.SourceURL, &fm, &tm, &tu, &p.ImportedBy, &p.ImportedAt, &p.DisabledAt,
		&p.OwnerID, &p.Origin, &p.ChatEnabled)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(fm, &p.Frontmatter)
	_ = json.Unmarshal(tm, &p.ToolsMatched)
	_ = json.Unmarshal(tu, &p.ToolsUnmatched)
	return &p, nil
}

// dir returns a package's on-disk directory: Root/<name> for instance
// skills, Root/users/<owner>/<name> for user skills.
func (s *Store) dir(p *Package) string {
	if p.OwnerID == "" {
		return filepath.Join(s.Root, p.Name)
	}
	return filepath.Join(s.Root, userSubdir, p.OwnerID, p.Name)
}

// ImportStaged validates a staged skill directory and admits it into
// the instance library: files move to Root/<name>, a row records trust
// state. Every import is signature_status='unsigned' until the signing
// convention lands; the IMPORT ITSELF is the admin approval (the
// handler gates on role + an explicit confirm).
func (s *Store) ImportStaged(ctx context.Context, stagedDir, importedBy, sourceURL string, knownTools map[string]bool) (*Package, error) {
	return s.importStagedAs(ctx, "", stagedDir, importedBy, sourceURL, knownTools)
}

// importStagedAs is the owner-aware core: owner "" installs into the
// instance library, otherwise into the user's Root/users/<owner>/
// subtree with per-user quotas applied.
func (s *Store) importStagedAs(ctx context.Context, owner, stagedDir, importedBy, sourceURL string, knownTools map[string]bool) (*Package, error) {
	loaded, err := loadStaged(stagedDir)
	if err != nil {
		return nil, err
	}
	name := loaded.Frontmatter.Name
	if owner == "" && name == userSubdir {
		return nil, fmt.Errorf("skillpkg: %q is a reserved name", userSubdir)
	}

	var dest string
	if owner == "" {
		dest = filepath.Join(s.Root, name)
	} else {
		if n, err := s.countForOwner(ctx, owner); err != nil {
			return nil, err
		} else if n >= maxUserSkills {
			return nil, fmt.Errorf("skillpkg: skill limit reached (%d) — delete one first", maxUserSkills)
		}
		if size, err := treeSize(stagedDir); err != nil {
			return nil, err
		} else if size > maxSkillDirBytes {
			return nil, fmt.Errorf("skillpkg: skill exceeds the %dMB size cap", maxSkillDirBytes>>20)
		}
		dest = filepath.Join(s.Root, userSubdir, owner, name)
	}

	if _, err := os.Stat(dest); err == nil {
		// A directory with no matching row is a stale leftover (e.g. a
		// deleted user's cascade removed the row but not the tree) —
		// self-heal by replacing it. A live row is a real conflict.
		if _, err := s.getScoped(ctx, owner, name); err == nil || owner == "" {
			return nil, fmt.Errorf("skillpkg: %q is already installed — delete it first to re-import", name)
		}
		_ = os.RemoveAll(dest)
	}
	if err := copyTree(stagedDir, dest); err != nil {
		return nil, fmt.Errorf("skillpkg: install: %w", err)
	}
	pkg, err := s.insertFromLoaded(ctx, loaded, owner, importedBy, sourceURL, "imported", knownTools)
	if err != nil {
		_ = os.RemoveAll(dest)
		return nil, err
	}
	return pkg, nil
}

// ImportZip stages a zip upload (zip-slip-proofed) and imports it
// into the instance library. The archive may wrap the skill in one
// top-level directory (the GitHub-download shape) or be the skill's
// contents directly.
func (s *Store) ImportZip(ctx context.Context, data []byte, importedBy, sourceURL string, knownTools map[string]bool) (*Package, error) {
	return s.importZipAs(ctx, "", data, importedBy, sourceURL, knownTools)
}

// ImportZipForUser imports into the caller's private library
// (USER-SKILLS-SPEC Phase A). importedBy == owner by construction;
// origin is recorded as 'imported'.
func (s *Store) ImportZipForUser(ctx context.Context, owner string, data []byte, sourceURL string, knownTools map[string]bool) (*Package, error) {
	if owner == "" {
		return nil, fmt.Errorf("skillpkg: user import requires an owner")
	}
	return s.importZipAs(ctx, owner, data, owner, sourceURL, knownTools)
}

func (s *Store) importZipAs(ctx context.Context, owner string, data []byte, importedBy, sourceURL string, knownTools map[string]bool) (*Package, error) {
	if len(data) > maxZipBytes {
		return nil, fmt.Errorf("skillpkg: archive exceeds %d bytes", maxZipBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("skillpkg: not a zip archive: %w", err)
	}
	staged, err := os.MkdirTemp("", "familiar-skill-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staged)

	var totalBytes int64
	var fileCount int
	for _, f := range zr.File {
		// Bound the entry count so a central directory stuffed with
		// tiny (or zero-byte) entries can't spin extraction unbounded.
		if fileCount++; fileCount > maxZipFiles {
			return nil, fmt.Errorf("skillpkg: archive has more than %d entries", maxZipFiles)
		}
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return nil, fmt.Errorf("skillpkg: unsafe path %q in archive", f.Name)
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("skillpkg: symlink %q not allowed in a skill package", f.Name)
		}
		target := filepath.Join(staged, name)
		if !strings.HasPrefix(target, staged+string(os.PathSeparator)) {
			return nil, fmt.Errorf("skillpkg: unsafe path %q in archive", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if f.UncompressedSize64 > maxZipFileBytes {
			return nil, fmt.Errorf("skillpkg: %q exceeds the per-file size cap", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxZipFileBytes+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if len(content) > maxZipFileBytes {
			return nil, fmt.Errorf("skillpkg: %q exceeds the per-file size cap", f.Name)
		}
		// Accumulate ACTUAL decompressed bytes (not the attacker-set
		// UncompressedSize64 header) and abort before writing past the
		// cumulative cap, so a bomb can't fill the temp dir mid-loop.
		totalBytes += int64(len(content))
		if totalBytes > maxZipTotal {
			return nil, fmt.Errorf("skillpkg: archive decompresses past the %dMB total cap", maxZipTotal>>20)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return nil, err
		}
	}

	root, err := findSkillRoot(staged)
	if err != nil {
		return nil, err
	}
	return s.importStagedAs(ctx, owner, root, importedBy, sourceURL, knownTools)
}

// PreviewZip stages and parses an archive WITHOUT admitting it —
// the dry-run half of the approve-on-import flow. The handler shows
// this to the admin; a second call with confirm performs ImportZip.
func (s *Store) PreviewZip(data []byte, knownTools map[string]bool) (*Loaded, []string, []string, error) {
	if len(data) > maxZipBytes {
		return nil, nil, nil, fmt.Errorf("skillpkg: archive exceeds %d bytes", maxZipBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("skillpkg: not a zip archive: %w", err)
	}
	staged, err := os.MkdirTemp("", "familiar-skill-preview-*")
	if err != nil {
		return nil, nil, nil, err
	}
	defer os.RemoveAll(staged)
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) ||
			f.Mode()&os.ModeSymlink != 0 || f.FileInfo().IsDir() {
			continue
		}
		target := filepath.Join(staged, name)
		if !strings.HasPrefix(target, staged+string(os.PathSeparator)) {
			continue
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			continue
		}
		content, _ := io.ReadAll(io.LimitReader(rc, maxZipFileBytes))
		rc.Close()
		_ = os.WriteFile(target, content, 0o644)
	}
	root, err := findSkillRoot(staged)
	if err != nil {
		return nil, nil, nil, err
	}
	loaded, err := loadStaged(root)
	if err != nil {
		return nil, nil, nil, err
	}
	matched, unmatched := MapAllowedTools(loaded.Frontmatter.AllowedTools, knownTools)
	return loaded, matched, unmatched, nil
}

// Rescan walks Root and admits any well-formed skill directory not
// yet indexed (an operator cp -r'ing a skill in, then clicking
// Rescan, IS the approval). Existing rows get their digest +
// frontmatter refreshed when disk changed; trust status is kept.
func (s *Store) Rescan(ctx context.Context, importedBy string, knownTools map[string]bool) (added, updated int, missing, errs []string) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return 0, 0, nil, []string{err.Error()}
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// The per-user subtree (USER-SKILLS-SPEC) is never part of the
		// instance rescan — user skills are managed via /skills/mine.
		if e.Name() == userSubdir {
			continue
		}
		dir := filepath.Join(s.Root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			continue
		}
		// Present-on-disk counts as seen even if the load fails below —
		// a broken SKILL.md is reported via errs, not as "missing".
		// Installed dirs enforce name==dirname, so the dir name is the
		// package name.
		seen[e.Name()] = true
		loaded, err := LoadDir(dir)
		if err != nil {
			errs = append(errs, e.Name()+": "+err.Error())
			continue
		}
		existing, err := s.GetByName(ctx, loaded.Frontmatter.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			if _, err := s.insertFromLoaded(ctx, loaded, "", importedBy, "", "imported", knownTools); err != nil {
				errs = append(errs, e.Name()+": "+err.Error())
				continue
			}
			added++
		case err != nil:
			errs = append(errs, e.Name()+": "+err.Error())
		case existing.Origin == "builtin":
			// Builtins are owned by the boot sync, and their embed is
			// the source of truth: adopting a drifted disk digest here
			// would bless locally tampered content under the BUILT-IN
			// badge. Leave the row alone; SyncBuiltins converges disk
			// back to the embed at next boot.
		case existing.Digest != loaded.Digest:
			if err := s.refreshFromLoaded(ctx, existing.ID, loaded, knownTools); err != nil {
				errs = append(errs, e.Name()+": "+err.Error())
				continue
			}
			updated++
		}
	}

	// Reconcile deletions: a catalog row whose directory is gone would
	// otherwise stay listed and enabled — shards keep advertising it
	// and use_skill fails opaquely at read time. Disable it and report
	// it so the admin sees the catalog tell the truth. If the
	// directory comes back, the row stays disabled (we can't tell an
	// auto-disable from a deliberate one) — re-enabling is one click.
	// Builtins are exempt: the boot sync re-materializes their
	// directory from the embed, so auto-disabling one here would
	// contradict the "re-syncs at boot" contract and stick (the sync
	// deliberately never touches disabled_at).
	pkgs, err := s.List(ctx)
	if err != nil {
		errs = append(errs, "reconcile: "+err.Error())
		return added, updated, missing, errs
	}
	for _, p := range pkgs {
		if seen[p.Name] || !p.Enabled() || p.Origin == "builtin" {
			continue
		}
		if err := s.SetDisabled(ctx, p.ID, true); err != nil {
			errs = append(errs, p.Name+": disable missing: "+err.Error())
			continue
		}
		missing = append(missing, p.Name)
	}
	return added, updated, missing, errs
}

func (s *Store) insertFromLoaded(ctx context.Context, l *Loaded, owner, importedBy, sourceURL, origin string, knownTools map[string]bool) (*Package, error) {
	matched, unmatched := MapAllowedTools(l.Frontmatter.AllowedTools, knownTools)
	fm, _ := json.Marshal(l.Frontmatter)
	tm, _ := json.Marshal(matched)
	tu, _ := json.Marshal(unmatched)
	row := s.pool.QueryRowContext(ctx, `
		INSERT INTO skill_packages (
		    name, description, version, digest, signature_status,
		    has_wasm, has_scripts, source_url, frontmatter,
		    tools_matched, tools_unmatched, imported_by, owner_id, origin
		) VALUES ($1,$2,$3,$4,'unsigned',$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13)
		RETURNING `+pkgCols,
		l.Frontmatter.Name, l.Frontmatter.Description,
		l.Frontmatter.Metadata["version"], l.Digest,
		l.HasWasm, l.HasScripts, sourceURL, fm, tm, tu, importedBy, owner, origin)
	p, err := scanPackage(row)
	if err != nil {
		return nil, fmt.Errorf("skillpkg: insert: %w", err)
	}
	return p, nil
}

func (s *Store) refreshFromLoaded(ctx context.Context, id string, l *Loaded, knownTools map[string]bool) error {
	matched, unmatched := MapAllowedTools(l.Frontmatter.AllowedTools, knownTools)
	fm, _ := json.Marshal(l.Frontmatter)
	tm, _ := json.Marshal(matched)
	tu, _ := json.Marshal(unmatched)
	_, err := s.pool.ExecContext(ctx, `
		UPDATE skill_packages SET
		    description = $2, version = $3, digest = $4, has_wasm = $5,
		    has_scripts = $6, frontmatter = $7, tools_matched = $8,
		    tools_unmatched = $9
		WHERE id = $1::uuid`,
		id, l.Frontmatter.Description, l.Frontmatter.Metadata["version"],
		l.Digest, l.HasWasm, l.HasScripts, fm, tm, tu)
	return err
}

// List returns the INSTANCE library only (owner_id IS NULL) — the
// admin catalog and the rescan reconciler both operate on it. User
// skills are listed per-owner via ListForOwner.
func (s *Store) List(ctx context.Context) ([]*Package, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT `+pkgCols+` FROM skill_packages WHERE owner_id IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Package{}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListForOwner returns one user's private skills.
func (s *Store) ListForOwner(ctx context.Context, owner string) ([]*Package, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT `+pkgCols+` FROM skill_packages WHERE owner_id = $1 ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Package{}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) countForOwner(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM skill_packages WHERE owner_id = $1`, owner).Scan(&n)
	return n, err
}

func (s *Store) Get(ctx context.Context, id string) (*Package, error) {
	p, err := scanPackage(s.pool.QueryRowContext(ctx,
		`SELECT `+pkgCols+` FROM skill_packages WHERE id = $1::uuid`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// GetOwned returns a package by id iff it belongs to owner — the
// authorization primitive for every /skills/mine mutation.
func (s *Store) GetOwned(ctx context.Context, owner, id string) (*Package, error) {
	p, err := scanPackage(s.pool.QueryRowContext(ctx,
		`SELECT `+pkgCols+` FROM skill_packages WHERE id = $1::uuid AND owner_id = $2`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// GetByName resolves an INSTANCE skill by name (rescan's lookup).
func (s *Store) GetByName(ctx context.Context, name string) (*Package, error) {
	return s.getScoped(ctx, "", name)
}

// getScoped resolves (owner, name) where owner "" means the instance
// namespace.
func (s *Store) getScoped(ctx context.Context, owner, name string) (*Package, error) {
	p, err := scanPackage(s.pool.QueryRowContext(ctx, `
		SELECT `+pkgCols+` FROM skill_packages
		 WHERE name = $1
		   AND ((owner_id IS NULL AND $2 = '') OR owner_id = NULLIF($2, ''))`,
		name, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) SetDisabled(ctx context.Context, id string, disabled bool) error {
	q := `UPDATE skill_packages SET disabled_at = NOW() WHERE id = $1::uuid AND disabled_at IS NULL`
	if !disabled {
		q = `UPDATE skill_packages SET disabled_at = NULL WHERE id = $1::uuid`
	}
	res, err := s.pool.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 && disabled {
		// Either missing or already disabled; distinguish for 404s.
		if _, err := s.Get(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the row AND the directory — the library reflects
// disk, so a deleted package must not resurrect on the next rescan.
// Builtins are refused: they WOULD resurrect (SyncBuiltins re-installs
// on every boot), so a delete could only mislead — disabling is the
// supported off switch.
func (s *Store) Delete(ctx context.Context, id string) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if p.Origin == "builtin" {
		return fmt.Errorf("%q: %w", p.Name, ErrBuiltinImmutable)
	}
	if _, err := s.pool.ExecContext(ctx,
		`DELETE FROM skill_packages WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	dir := s.dir(p)
	if resolved, rErr := filepath.EvalSymlinks(dir); rErr == nil && strings.HasPrefix(resolved+string(os.PathSeparator), mustResolve(s.Root)+string(os.PathSeparator)) {
		_ = os.RemoveAll(dir)
	}
	return nil
}

// ── Shard bindings ───────────────────────────────────────────────

// SetShardSkills replaces a shard's bound-skill set. Cross-owner
// binding is refused at the SQL level (USER-SKILLS-SPEC Phase A): a
// shard may bind instance skills and its OWN owner's user skills,
// never another user's — enforced here in addition to the handler so
// no future call site can regress it.
func (s *Store) SetShardSkills(ctx context.Context, shardID string, skillIDs []string) error {
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM shard_skills WHERE shard_id = $1`, shardID); err != nil {
		return err
	}
	for _, id := range skillIDs {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO shard_skills (shard_id, skill_id)
			SELECT $1, p.id FROM skill_packages p, shards sh
			 WHERE p.id = $2::uuid AND sh.id = $1
			   AND (p.owner_id IS NULL OR p.owner_id = sh.owner_id)`,
			shardID, id)
		if err != nil {
			return fmt.Errorf("skillpkg: bind %s: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("skillpkg: skill %s is not available to this shard's owner", id)
		}
	}
	return tx.Commit()
}

// ListShardSkills returns the ENABLED packages bound to a shard —
// the set the augmenter advertises and the tools authorize against.
func (s *Store) ListShardSkills(ctx context.Context, shardID string) ([]*Package, error) {
	rows, err := s.pool.QueryContext(ctx, `
		SELECT `+pkgCols+` FROM skill_packages p
		  JOIN shard_skills ss ON ss.skill_id = p.id
		 WHERE ss.shard_id = $1 AND p.disabled_at IS NULL
		 ORDER BY p.name`, shardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Package{}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── Shard-scoped content access (the two native tools) ───────────

// BodyForShard returns a skill's SKILL.md body iff the skill is
// enabled and bound to the shard — the use_skill authorization.
func (s *Store) BodyForShard(ctx context.Context, shardID, name string) (string, error) {
	p, err := s.boundPackage(ctx, shardID, name)
	if err != nil {
		return "", err
	}
	return s.bodyFor(p)
}

// FileForShard serves one referenced file (references/, assets/, or
// any other in-package path) iff the skill is bound to the shard.
// Path-jailed: cleaned, relative, no dot-dot, symlink-resolved
// containment, size-capped.
func (s *Store) FileForShard(ctx context.Context, shardID, name, relPath string) ([]byte, error) {
	p, err := s.boundPackage(ctx, shardID, name)
	if err != nil {
		return nil, err
	}
	return s.fileFor(p, relPath)
}

// bodyFor resolves a package's SKILL.md body. Builtins read from the
// binary embed — the source of truth the boot sync converges disk to —
// so a locally tampered directory can never serve under the BUILT-IN
// badge on either the shard or trusted path (the same invariant
// BuiltinFile documents for the role prompts). Everything else reads
// from disk as before.
func (s *Store) bodyFor(p *Package) (string, error) {
	if p.Origin == "builtin" {
		raw, err := BuiltinFile(p.Name, "SKILL.md")
		if err != nil {
			return "", err
		}
		_, body, err := ParseSkillMD(raw)
		if err != nil {
			return "", err
		}
		return capSkillBody(body), nil
	}
	loaded, err := LoadDir(s.dir(p))
	if err != nil {
		return "", err
	}
	return capSkillBody(loaded.Body), nil
}

// fileFor mirrors bodyFor for referenced files: builtins serve from
// the embed (fs.ValidPath is the jail — embed paths cannot traverse),
// with the same size cap serveFile applies; everything else takes the
// disk path jail.
func (s *Store) fileFor(p *Package, relPath string) ([]byte, error) {
	if p.Origin == "builtin" {
		clean := filepath.ToSlash(filepath.Clean(relPath))
		if !fs.ValidPath(clean) || clean == "." {
			return nil, fmt.Errorf("skillpkg: invalid path %q", relPath)
		}
		b, err := BuiltinFile(p.Name, clean)
		if err != nil {
			return nil, fmt.Errorf("skillpkg: %s: file %q not found", p.Name, relPath)
		}
		if len(b) > maxSkillFileBytes {
			return nil, fmt.Errorf("skillpkg: file %q exceeds the %dKB read cap", relPath, maxSkillFileBytes>>10)
		}
		return b, nil
	}
	return s.serveFile(p, relPath)
}

// serveFile is the shared path-jailed file read behind FileForShard /
// FileForUser: cleaned, relative, no dot-dot, symlink-resolved
// containment, size-capped.
func (s *Store) serveFile(p *Package, relPath string) ([]byte, error) {
	clean := filepath.Clean(relPath)
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return nil, fmt.Errorf("skillpkg: invalid path %q", relPath)
	}
	skillDir, err := filepath.EvalSymlinks(s.dir(p))
	if err != nil {
		return nil, err
	}
	full := filepath.Join(skillDir, clean)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, fmt.Errorf("skillpkg: file %q not found in skill %q", clean, p.Name)
	}
	if !strings.HasPrefix(resolved, skillDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("skillpkg: invalid path %q", relPath)
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("skillpkg: file %q not found in skill %q", clean, p.Name)
	}
	if info.Size() > maxSkillFileBytes {
		return nil, fmt.Errorf("skillpkg: %q exceeds the %dKB read cap", clean, maxSkillFileBytes/1024)
	}
	return os.ReadFile(resolved)
}

// ── User-scoped content access (USER-SKILLS-SPEC Phase B) ────────
//
// The trusted-path twins of BodyForShard/FileForShard: authorization
// is "owned by this user AND enabled AND chat_enabled". Instance
// skills are deliberately unreachable here (spec: they stay shard-only
// in v1) with one exception: origin='builtin' packages are versioned
// gateway source — the same trust class as the prompts/tiers/*.md that
// already enter the trusted prompt — so they serve to every user (see
// builtin.go). Another user's skills read as not-found.

// SetChatEnabled flips the trusted-path opt-in.
func (s *Store) SetChatEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.pool.ExecContext(ctx,
		`UPDATE skill_packages SET chat_enabled = $2 WHERE id = $1::uuid`, id, enabled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListChatEnabled returns the owner's enabled + chat-enabled skills
// merged with the instance builtins — the set the trusted-path prompt
// block advertises.
func (s *Store) ListChatEnabled(ctx context.Context, owner string) ([]*Package, error) {
	// DISTINCT ON (name) with owner-first ordering: when a user's
	// personal skill shares a name with a builtin (the documented
	// duplicate-as-mine customization path), advertise ONE entry — the
	// owner's — matching what chatPackage will actually serve. Without
	// it the prompt block lists the same name twice with two
	// descriptions and the model can't reach the second one.
	rows, err := s.pool.QueryContext(ctx, `
		SELECT DISTINCT ON (name) `+pkgCols+` FROM skill_packages
		 WHERE (owner_id = $1 OR (owner_id IS NULL AND origin = 'builtin'))
		   AND disabled_at IS NULL AND chat_enabled
		 ORDER BY name, owner_id NULLS LAST`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Package{}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) chatPackage(ctx context.Context, userID, name string) (*Package, error) {
	if userID == "" {
		return nil, fmt.Errorf("skillpkg: no user identity on this turn")
	}
	// Owned by the caller OR a builtin (trusted gateway source — see
	// the section comment above). Per-scope name uniqueness allows a
	// user skill and a builtin to share a name; the caller's own skill
	// wins deterministically (owned rows sort before the NULL owner).
	p, err := scanPackage(s.pool.QueryRowContext(ctx, `
		SELECT `+pkgCols+` FROM skill_packages
		 WHERE (owner_id = $1 OR (owner_id IS NULL AND origin = 'builtin'))
		   AND name = $2 AND disabled_at IS NULL AND chat_enabled
		 ORDER BY owner_id NULLS LAST
		 LIMIT 1`,
		userID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("skillpkg: skill %q is not enabled for chat", name)
	}
	return p, err
}

// BodyForUser returns a skill's SKILL.md body iff the caller owns it
// and has enabled it for chat — the trusted-path use_skill authz.
func (s *Store) BodyForUser(ctx context.Context, userID, name string) (string, error) {
	p, err := s.chatPackage(ctx, userID, name)
	if err != nil {
		return "", err
	}
	return s.bodyFor(p)
}

// FileForUser serves one referenced file from the caller's own
// chat-enabled skill (or a builtin, from the embed). Same path jail
// as FileForShard.
func (s *Store) FileForUser(ctx context.Context, userID, name, relPath string) ([]byte, error) {
	p, err := s.chatPackage(ctx, userID, name)
	if err != nil {
		return nil, err
	}
	return s.fileFor(p, relPath)
}

// ── Authoring (USER-SKILLS-SPEC Phase C) ─────────────────────────
//
// Authored skills are composed server-side from (name, description,
// body): the workspace editor never writes raw frontmatter, so a
// user can't wedge their own library with a syntax error. The
// directory stays the source of truth — SaveAuthored writes SKILL.md
// then re-loads and re-digests from disk like every other admission
// path.

// SaveAuthored creates or updates an authored skill owned by owner.
// Updates preserve any extra frontmatter (license, metadata,
// allowed-tools) and any non-SKILL.md files (references/ from a
// duplicate). Imported skills are read-only: origin is immutable and
// editing one requires Duplicate first.
func (s *Store) SaveAuthored(ctx context.Context, owner, name, description, body string, knownTools map[string]bool) (*Package, error) {
	if owner == "" {
		return nil, fmt.Errorf("skillpkg: authoring requires an owner")
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if len(body) > maxSkillFileBytes {
		return nil, fmt.Errorf("skillpkg: skill body exceeds the %dKB cap", maxSkillFileBytes/1024)
	}

	fm := Frontmatter{Name: name, Description: strings.TrimSpace(description)}
	existing, err := s.getScoped(ctx, owner, name)
	switch {
	case err == nil:
		if existing.Origin != "authored" {
			return nil, fmt.Errorf("skillpkg: %q was imported and is read-only — duplicate it as an authored skill to edit", name)
		}
		// Carry forward frontmatter extras the editor doesn't surface.
		fm.License = existing.Frontmatter.License
		fm.Compatibility = existing.Frontmatter.Compatibility
		fm.Metadata = existing.Frontmatter.Metadata
		fm.AllowedTools = existing.Frontmatter.AllowedTools
	case errors.Is(err, ErrNotFound):
		if n, err := s.countForOwner(ctx, owner); err != nil {
			return nil, err
		} else if n >= maxUserSkills {
			return nil, fmt.Errorf("skillpkg: skill limit reached (%d) — delete one first", maxUserSkills)
		}
	default:
		return nil, err
	}

	content, err := ComposeSkillMD(fm, body)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(s.Root, userSubdir, owner, name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("skillpkg: create skill dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), content, 0o644); err != nil {
		return nil, fmt.Errorf("skillpkg: write SKILL.md: %w", err)
	}
	loaded, err := LoadDir(dest)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := s.refreshFromLoaded(ctx, existing.ID, loaded, knownTools); err != nil {
			return nil, err
		}
		return s.Get(ctx, existing.ID)
	}
	pkg, err := s.insertFromLoaded(ctx, loaded, owner, owner, "", "authored", knownTools)
	if err != nil {
		_ = os.RemoveAll(dest)
		return nil, err
	}
	return pkg, nil
}

// ContentForOwner returns a skill's parsed content for the editor —
// any origin, owner-scoped, independent of chat_enabled (viewing your
// own library isn't a trust boundary; executing it in chat is).
func (s *Store) ContentForOwner(ctx context.Context, owner, id string) (*Loaded, *Package, error) {
	p, err := s.GetOwned(ctx, owner, id)
	if err != nil {
		return nil, nil, err
	}
	loaded, err := LoadDir(s.dir(p))
	if err != nil {
		return nil, nil, err
	}
	return loaded, p, nil
}

// Duplicate copies one of the owner's skills into a NEW authored
// skill: the whole tree (references/ included) is copied, the
// frontmatter is renamed, and origin becomes 'authored' so it is
// editable. source_url is preserved — provenance survives the copy
// and the UI's chat-enable warning keys on it.
func (s *Store) Duplicate(ctx context.Context, owner, srcID, newName string, knownTools map[string]bool) (*Package, error) {
	if err := ValidateName(newName); err != nil {
		return nil, err
	}
	src, err := s.GetOwned(ctx, owner, srcID)
	if err != nil {
		return nil, err
	}
	if _, err := s.getScoped(ctx, owner, newName); err == nil {
		return nil, fmt.Errorf("skillpkg: you already have a skill named %q", newName)
	}
	if n, err := s.countForOwner(ctx, owner); err != nil {
		return nil, err
	} else if n >= maxUserSkills {
		return nil, fmt.Errorf("skillpkg: skill limit reached (%d) — delete one first", maxUserSkills)
	}

	dest := filepath.Join(s.Root, userSubdir, owner, newName)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("skillpkg: %q already exists on disk", newName)
	}
	if err := copyTree(s.dir(src), dest); err != nil {
		return nil, fmt.Errorf("skillpkg: duplicate: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dest) }

	// Rewrite SKILL.md with the new name (LoadDir enforces name ==
	// dirname). Everything else carries over verbatim.
	raw, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		cleanup()
		return nil, err
	}
	fm, body, err := ParseSkillMD(raw)
	if err != nil {
		cleanup()
		return nil, err
	}
	fm.Name = newName
	content, err := ComposeSkillMD(fm, body)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), content, 0o644); err != nil {
		cleanup()
		return nil, err
	}
	loaded, err := LoadDir(dest)
	if err != nil {
		cleanup()
		return nil, err
	}
	pkg, err := s.insertFromLoaded(ctx, loaded, owner, owner, src.SourceURL, "authored", knownTools)
	if err != nil {
		cleanup()
		return nil, err
	}
	return pkg, nil
}

// ExportZip renders one of the owner's skills as a zip (the inverse
// of ImportZip): entries live under a "<name>/" wrapper so the
// archive re-imports cleanly here or in any agentskills.io consumer.
func (s *Store) ExportZip(ctx context.Context, owner, id string) (string, []byte, error) {
	p, err := s.GetOwned(ctx, owner, id)
	if err != nil {
		return "", nil, err
	}
	root := s.dir(p)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("skillpkg: symlink %q not allowed in a skill package", path)
		}
		rel, _ := filepath.Rel(root, path)
		w, err := zw.Create(p.Name + "/" + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(content)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	if err := zw.Close(); err != nil {
		return "", nil, err
	}
	return p.Name + ".zip", buf.Bytes(), nil
}

func (s *Store) boundPackage(ctx context.Context, shardID, name string) (*Package, error) {
	if shardID == "" {
		return nil, fmt.Errorf("skillpkg: skills are only available inside a shard")
	}
	p, err := scanPackage(s.pool.QueryRowContext(ctx, `
		SELECT `+pkgCols+` FROM skill_packages p
		  JOIN shard_skills ss ON ss.skill_id = p.id
		 WHERE ss.shard_id = $1 AND p.name = $2 AND p.disabled_at IS NULL`,
		shardID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("skillpkg: skill %q is not available to this shard", name)
	}
	return p, err
}

// ── helpers ──────────────────────────────────────────────────────

// findSkillRoot locates the directory holding SKILL.md inside a
// staged extraction: either the staging root itself or exactly one
// top-level wrapper directory (the GitHub-zip shape).
func findSkillRoot(staged string) (string, error) {
	if _, err := os.Stat(filepath.Join(staged, "SKILL.md")); err == nil {
		return staged, nil
	}
	entries, err := os.ReadDir(staged)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		candidate := filepath.Join(staged, dirs[0])
		if _, err := os.Stat(filepath.Join(candidate, "SKILL.md")); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("skillpkg: archive does not contain a SKILL.md at its root")
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}

func mustResolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// treeSize sums the file bytes under dir — the per-user skill-size
// quota check at import time.
func treeSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
