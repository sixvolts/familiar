package skillpkg

// Builtin-skill sync tests. These run against the REAL embedded
// builtin/ tree (currently the `research` skill), so unlike the rest
// of the suite the package name is fixed — and the skillpkg_test
// schema persists across runs. cleanBuiltinRows therefore scrubs the
// research instance row before AND after each test so (a) every test
// sees a fresh install and (b) the widened ListChatEnabled/chatPackage
// queries never leak a builtin row into the pre-existing Phase B tests.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const builtinName = "research" // the one embedded package today

func cleanBuiltinRows(t *testing.T, s *Store) {
	t.Helper()
	del := func() {
		// Raw SQL: Delete() now refuses builtins by design.
		_, err := s.pool.ExecContext(context.Background(), `
			DELETE FROM skill_packages
			 WHERE owner_id IS NULL AND (origin = 'builtin' OR name = $1)`,
			builtinName)
		if err != nil {
			t.Fatalf("clean builtin rows: %v", err)
		}
	}
	del()
	t.Cleanup(del)
}

func syncOnce(t *testing.T, s *Store, known map[string]bool) (int, int, []string) {
	t.Helper()
	installed, refreshed, skipped, err := s.SyncBuiltins(context.Background(), known)
	if err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	return installed, refreshed, skipped
}

func TestSyncBuiltins_InstallThenNoop(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()
	known := map[string]bool{"web_search": true, "fetch_page": true}

	installed, refreshed, skipped := syncOnce(t, s, known)
	if installed != 1 || refreshed != 0 || len(skipped) != 0 {
		t.Fatalf("fresh sync = %d/%d/%v, want 1/0/[]", installed, refreshed, skipped)
	}

	pkg, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatalf("GetByName after sync: %v", err)
	}
	if pkg.Origin != "builtin" || pkg.OwnerID != "" || pkg.ImportedBy != "" {
		t.Errorf("row provenance = origin:%q owner:%q imported_by:%q", pkg.Origin, pkg.OwnerID, pkg.ImportedBy)
	}
	if !pkg.ChatEnabled || !pkg.Enabled() {
		t.Errorf("builtin not default-on: chat_enabled=%v enabled=%v", pkg.ChatEnabled, pkg.Enabled())
	}
	if pkg.SourceURL != "" {
		t.Errorf("source_url = %q, want empty", pkg.SourceURL)
	}
	// Advisory tool mapping was computed against knownTools.
	if len(pkg.ToolsMatched) != 2 {
		t.Errorf("tools_matched = %v, want the 2 known ones", pkg.ToolsMatched)
	}
	// Files landed on disk under Root/<name>, references included.
	for _, f := range []string{"SKILL.md", "references/deep-protocol.md", "references/note-template.md"} {
		if _, err := os.Stat(filepath.Join(s.Root, builtinName, f)); err != nil {
			t.Errorf("installed file missing: %v", err)
		}
	}

	// Second sync is a no-op.
	installed, refreshed, skipped = syncOnce(t, s, known)
	if installed != 0 || refreshed != 0 || len(skipped) != 0 {
		t.Fatalf("second sync = %d/%d/%v, want 0/0/[]", installed, refreshed, skipped)
	}
}

// A deploy upgrade looks like "row digest no longer matches the embed"
// (the embed in THIS binary is always current, so the test fakes the
// old deploy by staling the row + mutating disk). Sync must rewrite
// both — while preserving the admin's disabled + chat-off toggles.
func TestSyncBuiltins_RefreshPreservesToggles(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()

	syncOnce(t, s, nil)
	pkg, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}
	embedDigest := pkg.Digest

	// Admin toggles set before the "upgrade".
	if err := s.SetDisabled(ctx, pkg.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChatEnabled(ctx, pkg.ID, false); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-upgrade state: stale row digest + drifted disk.
	if _, err := s.pool.ExecContext(ctx, `
		UPDATE skill_packages SET digest = 'stale-digest' WHERE id = $1::uuid`, pkg.ID); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(s.Root, builtinName, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("locally mutated"), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, refreshed, skipped := syncOnce(t, s, nil)
	if installed != 0 || refreshed != 1 || len(skipped) != 0 {
		t.Fatalf("upgrade sync = %d/%d/%v, want 0/1/[]", installed, refreshed, skipped)
	}
	got, err := s.Get(ctx, pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != embedDigest {
		t.Errorf("row digest = %q, want the embed's %q", got.Digest, embedDigest)
	}
	if got.Enabled() || got.ChatEnabled {
		t.Errorf("refresh clobbered toggles: enabled=%v chat_enabled=%v, want false/false", got.Enabled(), got.ChatEnabled)
	}
	raw, err := os.ReadFile(skillPath)
	if err != nil || strings.Contains(string(raw), "locally mutated") {
		t.Errorf("disk not restored from embed: %v %q", err, string(raw[:min(40, len(raw))]))
	}

	// Converged: the next sync is a no-op again.
	if i, r, sk := syncOnce(t, s, nil); i != 0 || r != 0 || len(sk) != 0 {
		t.Fatalf("post-upgrade sync = %d/%d/%v, want 0/0/[]", i, r, sk)
	}
}

// A name already held by an admin-imported (or authored) instance
// skill is never clobbered — reported in skipped, row untouched.
func TestSyncBuiltins_NameCollisionSkipped(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()
	user := uniqueName("spk-bi-col")
	seedUserAndShard(t, s, user, uniqueName("spk-bi-colsh"))

	// Seed an imported INSTANCE row squatting on the builtin's name.
	if _, err := s.pool.ExecContext(ctx, `
		INSERT INTO skill_packages (name, description, digest, frontmatter, imported_by, origin)
		VALUES ($1, 'squatter', 'squat-digest', '{}'::jsonb, $2, 'imported')`,
		builtinName, user); err != nil {
		t.Fatal(err)
	}

	installed, refreshed, skipped := syncOnce(t, s, nil)
	if installed != 0 || refreshed != 0 {
		t.Fatalf("collision sync = %d/%d, want 0/0", installed, refreshed)
	}
	if len(skipped) != 1 || skipped[0] != builtinName {
		t.Fatalf("skipped = %v, want [%s]", skipped, builtinName)
	}
	got, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "imported" || got.Digest != "squat-digest" {
		t.Errorf("squatter row touched: origin=%q digest=%q", got.Origin, got.Digest)
	}
	// And no builtin files were written over whatever the import holds.
	if _, err := os.Stat(filepath.Join(s.Root, builtinName)); !os.IsNotExist(err) {
		t.Error("sync wrote the builtin directory despite the collision")
	}
}

func TestSyncBuiltins_DeleteRefused(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()

	syncOnce(t, s, nil)
	pkg, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Delete(ctx, pkg.ID)
	if err == nil {
		t.Fatal("Delete accepted a builtin")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Errorf("unexpected refusal: %v", err)
	}
	// Row and directory both survive.
	if _, err := s.Get(ctx, pkg.ID); err != nil {
		t.Errorf("row gone after refused delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, builtinName, "SKILL.md")); err != nil {
		t.Errorf("directory gone after refused delete: %v", err)
	}
}

// The trusted-path exception: a builtin serves to ANY user (body +
// referenced files, prompt-block listing) while ordinary cross-user
// authz is unchanged, and both off switches still cut access.
func TestSyncBuiltins_TrustedChatAccess(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()
	alice := uniqueName("spk-bi-a")
	bob := uniqueName("spk-bi-b")
	seedUserAndShard(t, s, alice, uniqueName("spk-bi-ash"))
	seedUserAndShard(t, s, bob, uniqueName("spk-bi-bsh"))

	syncOnce(t, s, nil)
	pkg, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}

	// Any user reads the builtin body + references; anonymous doesn't.
	body, err := s.BodyForUser(ctx, alice, builtinName)
	if err != nil || !strings.Contains(body, "Tier selection") {
		t.Fatalf("BodyForUser(alice, builtin): %v", err)
	}
	if _, err := s.BodyForUser(ctx, bob, builtinName); err != nil {
		t.Errorf("BodyForUser(bob, builtin): %v", err)
	}
	ref, err := s.FileForUser(ctx, alice, builtinName, "references/deep-protocol.md")
	if err != nil || !strings.Contains(string(ref), "Deep-tier protocol") {
		t.Fatalf("FileForUser(builtin reference): %v", err)
	}
	if _, err := s.BodyForUser(ctx, "", builtinName); err == nil {
		t.Error("builtin served with no identity")
	}

	// The builtin exception must not widen USER-skill authz: bob still
	// can't read alice's chat-enabled authored skill.
	ownName := uniqueName("own")
	own, err := s.SaveAuthored(ctx, alice, ownName, "Alice's own.", "# Own\n\nOWNBODY", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetChatEnabled(ctx, own.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BodyForUser(ctx, bob, ownName); err == nil {
		t.Error("BodyForUser crossed owners via the builtin widening")
	}

	// ListChatEnabled merges the owner's skills with the builtins,
	// ordered by name.
	on, err := s.ListChatEnabled(ctx, alice)
	if err != nil || len(on) != 2 {
		t.Fatalf("ListChatEnabled = %d items (%v), want 2", len(on), err)
	}
	names := []string{on[0].Name, on[1].Name}
	if !(names[0] < names[1]) {
		t.Errorf("ListChatEnabled not name-ordered: %v", names)
	}
	seen := map[string]bool{}
	for _, p := range on {
		seen[p.Name] = true
	}
	if !seen[builtinName] || !seen[ownName] {
		t.Errorf("ListChatEnabled = %v, want builtin + own skill", names)
	}
	// Bob gets the builtin only.
	if bon, _ := s.ListChatEnabled(ctx, bob); len(bon) != 1 || bon[0].Name != builtinName {
		t.Errorf("ListChatEnabled(bob) = %d items", len(bon))
	}

	// Chat toggle off pulls it from serving + the prompt-block set...
	if err := s.SetChatEnabled(ctx, pkg.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BodyForUser(ctx, alice, builtinName); err == nil {
		t.Error("chat-disabled builtin still served")
	}
	if bon, _ := s.ListChatEnabled(ctx, bob); len(bon) != 0 {
		t.Errorf("chat-disabled builtin still listed: %d", len(bon))
	}
	// ...and a library-level disable trumps a chat re-enable.
	if err := s.SetChatEnabled(ctx, pkg.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, pkg.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BodyForUser(ctx, alice, builtinName); err == nil {
		t.Error("disabled builtin still served")
	}
}

// The trusted path serves builtin content from the BINARY EMBED, not
// the mutable skills dir: a locally tampered SKILL.md or reference
// file never reaches a prompt under the BUILT-IN badge, and an admin
// Rescan neither adopts the drifted digest nor auto-disables the row
// when the directory vanishes — the boot sync owns builtin disk
// lifecycle. (Review findings: disk-served builtin bodies + Rescan
// interplay.)
func TestBuiltins_TamperAndRescanResistance(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()
	user := uniqueName("spk-bi-t")
	seedUserAndShard(t, s, user, uniqueName("spk-bi-tsh"))

	syncOnce(t, s, nil)
	pkg, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with the on-disk copy.
	dir := filepath.Join(s.Root, builtinName)
	tampered := []byte("---\nname: research\ndescription: tampered\n---\n\nINJECTED BODY")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), tampered, 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "deep-protocol.md"), []byte("INJECTED REF"), 0o644); err != nil {
		t.Fatalf("tamper ref: %v", err)
	}

	// Served content is the embed's, not the tampered disk.
	body, err := s.BodyForUser(ctx, user, builtinName)
	if err != nil {
		t.Fatalf("BodyForUser: %v", err)
	}
	if strings.Contains(body, "INJECTED") || !strings.Contains(body, "Tier selection") {
		t.Errorf("BodyForUser served tampered disk content:\n%s", body[:120])
	}
	ref, err := s.FileForUser(ctx, user, builtinName, "references/deep-protocol.md")
	if err != nil {
		t.Fatalf("FileForUser: %v", err)
	}
	if strings.Contains(string(ref), "INJECTED") {
		t.Error("FileForUser served tampered reference")
	}

	// Rescan must not adopt the tampered digest into the row.
	_, updated, _, errs := s.Rescan(ctx, user, nil)
	if len(errs) > 0 {
		t.Fatalf("rescan errs: %v", errs)
	}
	if updated != 0 {
		t.Errorf("rescan refreshed %d rows — builtin digest adopted from tampered disk", updated)
	}
	after, err := s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}
	if after.Digest != pkg.Digest {
		t.Errorf("builtin row digest changed across rescan: %s → %s", pkg.Digest, after.Digest)
	}

	// Rescan's missing-dir reconciler must not auto-disable a builtin:
	// the boot sync re-materializes it, so disabling would stick.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove builtin dir: %v", err)
	}
	_, _, missing, errs := s.Rescan(ctx, user, nil)
	if len(errs) > 0 {
		t.Fatalf("rescan errs: %v", errs)
	}
	for _, name := range missing {
		if name == builtinName {
			t.Error("rescan auto-disabled the builtin for a missing directory")
		}
	}
	after, err = s.GetByName(ctx, builtinName)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Enabled() {
		t.Error("builtin disabled after rescan with missing dir")
	}
	// And the next boot sync heals the directory.
	if installed, refreshed, _ := func() (int, int, []string) {
		i, r, sk, err := s.SyncBuiltins(ctx, nil)
		if err != nil {
			t.Fatalf("re-sync: %v", err)
		}
		return i, r, sk
	}(); installed != 0 || refreshed != 1 {
		t.Errorf("healing sync = %d installed / %d refreshed, want 0/1", installed, refreshed)
	}
}

// A user's personal skill sharing a builtin's name is advertised ONCE
// in the trusted prompt block — the owner's copy, matching what
// chatPackage serves. (Review finding: double-advertised name.)
func TestBuiltins_UserSkillShadowsInChatList(t *testing.T) {
	s := storeForTest(t)
	cleanBuiltinRows(t, s)
	ctx := context.Background()
	user := uniqueName("spk-bi-sh")
	seedUserAndShard(t, s, user, uniqueName("spk-bi-shsh"))

	syncOnce(t, s, nil)
	own, err := s.SaveAuthored(ctx, user, builtinName, "my custom research", "# Mine\n\nCustomized.", nil)
	if err != nil {
		t.Fatalf("SaveAuthored: %v", err)
	}
	if err := s.SetChatEnabled(ctx, own.ID, true); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}

	list, err := s.ListChatEnabled(ctx, user)
	if err != nil {
		t.Fatalf("ListChatEnabled: %v", err)
	}
	var hits []*Package
	for _, p := range list {
		if p.Name == builtinName {
			hits = append(hits, p)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("chat list carries %d entries named %q, want exactly 1", len(hits), builtinName)
	}
	if hits[0].OwnerID != user {
		t.Errorf("chat list advertises owner %q, want the owner's shadowing copy (%q)", hits[0].OwnerID, user)
	}
	// And chatPackage serves the same one the list advertises.
	body, err := s.BodyForUser(ctx, user, builtinName)
	if err != nil {
		t.Fatalf("BodyForUser: %v", err)
	}
	if !strings.Contains(body, "Customized.") {
		t.Errorf("BodyForUser served the builtin, want the owner's copy:\n%s", body)
	}
}
