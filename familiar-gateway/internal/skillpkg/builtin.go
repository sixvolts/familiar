package skillpkg

// Built-in skills (RESEARCH-SKILL-SPEC §5): first-party skill packages
// embedded in the gateway binary and synced into the instance library
// at boot, so skills like `research` ship with a deploy instead of via
// zip import.
//
// Trust rationale: a builtin's body is versioned gateway source — it
// rides the same review-and-deploy path as prompts/tiers/*.md, which
// already enter the trusted prompt verbatim. That is why builtins are
// exempt from the USER-SKILLS-SPEC v1 "instance skills stay shard-only"
// rule and serve on the trusted chat path (chatPackage/ListChatEnabled
// admit origin='builtin'), default-enabled and chat-enabled on install.
// The admin off switches (disable, chat toggle) are preserved across
// re-syncs; delete is refused because the next boot would resurrect it.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:builtin
var builtinFS embed.FS

// builtinSyncResult is one skill's outcome inside SyncBuiltins.
type builtinSyncResult int

const (
	builtinUnchanged builtinSyncResult = iota
	builtinInstalled
	builtinRefreshed
	builtinSkipped
)

// SyncBuiltins reconciles every embedded builtin package into the
// instance library: absent ones install (enabled + chat_enabled — the
// whole point of shipping first-party skills is that they work out of
// the box), stale ones refresh (disk and row follow the embed; the
// admin's disabled_at / chat_enabled toggles survive), and names held
// by an admin-imported or authored instance skill are skipped loudly
// rather than clobbered. Per-skill failures are joined into err so one
// bad package can't block the rest; the caller treats the whole sync
// as non-fatal.
func (s *Store) SyncBuiltins(ctx context.Context, knownTools map[string]bool) (installed, refreshed int, skipped []string, err error) {
	entries, rdErr := fs.ReadDir(builtinFS, "builtin")
	if rdErr != nil {
		return 0, 0, nil, fmt.Errorf("skillpkg: read embedded builtins: %w", rdErr)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		res, sErr := s.syncBuiltin(ctx, name, knownTools)
		if sErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, sErr))
			continue
		}
		switch res {
		case builtinInstalled:
			installed++
		case builtinRefreshed:
			refreshed++
		case builtinSkipped:
			skipped = append(skipped, name)
		}
	}
	return installed, refreshed, skipped, errors.Join(errs...)
}

// syncBuiltin reconciles one embedded package. The embed is staged to
// a temp dir first so LoadDir applies the exact validation every other
// admission path gets (frontmatter shape, name == dir name, digest).
func (s *Store) syncBuiltin(ctx context.Context, name string, knownTools map[string]bool) (builtinSyncResult, error) {
	if name == userSubdir {
		// Same reservation ImportStaged enforces: the per-user subtree
		// must never be shadowed by an instance package.
		return 0, fmt.Errorf("skillpkg: %q is a reserved name", userSubdir)
	}
	staging, err := os.MkdirTemp("", "familiar-builtin-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(staging)
	stagedDir := filepath.Join(staging, name)
	if err := materializeBuiltin(name, stagedDir); err != nil {
		return 0, err
	}
	// Same total-size bound the user import path applies — embedded
	// files are first-party, so this only guards against a bloated
	// package sneaking through review.
	if size, err := treeSize(stagedDir); err != nil {
		return 0, err
	} else if size > maxSkillDirBytes {
		return 0, fmt.Errorf("skillpkg: builtin exceeds the %dMB size cap", maxSkillDirBytes>>20)
	}
	loaded, err := LoadDir(stagedDir)
	if err != nil {
		return 0, err
	}

	dest := filepath.Join(s.Root, name)
	// A user-scoped skill sharing the name doesn't block the instance
	// install (per-scope uniqueness; the owner's copy deliberately
	// wins on their trusted path — that's the duplicate-as-mine
	// customization escape hatch), but the shadowing should be loud:
	// those users won't see builtin upgrades.
	if owners, oErr := s.userScopedOwners(ctx, name); oErr == nil && len(owners) > 0 {
		log.Printf("[skillpkg] builtin %q: user-scoped skills with the same name shadow it for their owners: %v", name, owners)
	}
	existing, err := s.getScoped(ctx, "", name)
	switch {
	case errors.Is(err, ErrNotFound):
		// A directory with no row carries no trust state (a stale
		// leftover, or an operator cp that never got rescanned) — the
		// builtin name is first-party-reserved, so replace it.
		if _, statErr := os.Stat(dest); statErr == nil {
			log.Printf("[skillpkg] builtin %q: replacing an unindexed directory on disk", name)
			_ = os.RemoveAll(dest)
		}
		if err := copyTree(stagedDir, dest); err != nil {
			return 0, fmt.Errorf("skillpkg: install builtin: %w", err)
		}
		if _, err := s.insertBuiltin(ctx, loaded, knownTools); err != nil {
			_ = os.RemoveAll(dest)
			return 0, err
		}
		return builtinInstalled, nil
	case err != nil:
		return 0, err
	case existing.Origin != "builtin":
		log.Printf("[skillpkg] builtin %q: name taken by an admin-%s skill — not clobbering; delete it and reboot to adopt the built-in", name, existing.Origin)
		return builtinSkipped, nil
	}

	// Existing builtin row: converge disk and row on the embed. Disk
	// is compared by its own digest (heals deletion AND local edits,
	// with or without an intervening rescan); the row by its recorded
	// digest (a deploy upgrade). refreshFromLoaded touches digest /
	// frontmatter / tool mapping only — never disabled_at or
	// chat_enabled, so admin toggles survive the upgrade.
	changed := false
	if diskDigest, dErr := DigestDir(dest); dErr != nil || diskDigest != loaded.Digest {
		if err := os.RemoveAll(dest); err != nil {
			return 0, err
		}
		if err := copyTree(stagedDir, dest); err != nil {
			return 0, fmt.Errorf("skillpkg: restore builtin: %w", err)
		}
		changed = true
	}
	if existing.Digest != loaded.Digest {
		if err := s.refreshFromLoaded(ctx, existing.ID, loaded, knownTools); err != nil {
			return 0, err
		}
		changed = true
	}
	if changed {
		return builtinRefreshed, nil
	}
	return builtinUnchanged, nil
}

// insertBuiltin admits a builtin row: instance scope, no importing
// user (imported_by NULL = shipped with the gateway), no source URL,
// and — unlike every other admission path — chat_enabled from birth.
// Signature status stays 'unsigned' like all current packages; the
// binary embed IS the provenance until the signing convention lands.
func (s *Store) insertBuiltin(ctx context.Context, l *Loaded, knownTools map[string]bool) (*Package, error) {
	matched, unmatched := MapAllowedTools(l.Frontmatter.AllowedTools, knownTools)
	fm, _ := json.Marshal(l.Frontmatter)
	tm, _ := json.Marshal(matched)
	tu, _ := json.Marshal(unmatched)
	row := s.pool.QueryRowContext(ctx, `
		INSERT INTO skill_packages (
		    name, description, version, digest, signature_status,
		    has_wasm, has_scripts, source_url, frontmatter,
		    tools_matched, tools_unmatched, imported_by, owner_id,
		    origin, chat_enabled
		) VALUES ($1,$2,$3,$4,'unsigned',$5,$6,'',$7,$8,$9,NULL,NULL,'builtin',true)
		RETURNING `+pkgCols,
		l.Frontmatter.Name, l.Frontmatter.Description,
		l.Frontmatter.Metadata["version"], l.Digest,
		l.HasWasm, l.HasScripts, fm, tm, tu)
	p, err := scanPackage(row)
	if err != nil {
		return nil, fmt.Errorf("skillpkg: insert builtin: %w", err)
	}
	return p, nil
}

// userScopedOwners lists users holding a personal skill with this
// name — shadow detection for the install log above.
func (s *Store) userScopedOwners(ctx context.Context, name string) ([]string, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT owner_id FROM skill_packages WHERE name = $1 AND owner_id IS NOT NULL ORDER BY owner_id`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		owners = append(owners, o)
	}
	return owners, rows.Err()
}

// BuiltinFile reads one file straight from the embedded package —
// bypassing disk, DB, and authz — for gateway components that ship
// their prompts inside a builtin package (the research skill's worker
// and writer role prompts, RESEARCH-SKILL-SPEC §6.3/§6.5). The embed
// is the source of truth the boot sync converges everything else to,
// so reading it directly can never observe a locally tampered copy.
// relPath is slash-separated and relative to the skill root.
func BuiltinFile(skill, relPath string) ([]byte, error) {
	clean := strings.TrimPrefix(strings.TrimSuffix(relPath, "/"), "/")
	if clean == "" || strings.Contains(clean, "..") {
		return nil, fmt.Errorf("skillpkg: invalid builtin file path %q", relPath)
	}
	return builtinFS.ReadFile("builtin/" + skill + "/" + clean)
}

// materializeBuiltin writes one embedded package to dest. Symlinks
// cannot exist in an embed.FS, so the only guard needed is the same
// per-file size cap the zip extractor applies.
func materializeBuiltin(name, dest string) error {
	root := "builtin/" + name
	return fs.WalkDir(builtinFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		if len(content) > maxZipFileBytes {
			return fmt.Errorf("skillpkg: %s exceeds the per-file size cap", p)
		}
		return os.WriteFile(target, content, 0o644)
	})
}
