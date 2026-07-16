package skillpkg

// Parse/validate/digest are pure; store + binding + the shard-scoped
// content access run against Postgres (FAMILIAR_TEST_DSN-gated) in a
// dedicated schema, same pattern as internal/actions.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/familiar/gateway/internal/db"
)

const sampleSkill = `---
name: %s
description: Answers dice-rolling questions. Use when the user mentions dice.
license: MIT
metadata:
  version: "2.1"
  author: tester
allowed-tools: web_search Bash(git:*) read_page
---

# Dice Rolling

Always reply with the word DICEWORD.

See [the table](references/tables.md) for loaded-dice odds.
`

func writeSkillDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte(fmt.Sprintf(sampleSkill, name)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "tables.md"),
		[]byte("# Tables\n\nSECRET_TABLE_CONTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseSkillMD(t *testing.T) {
	fm, body, err := ParseSkillMD([]byte(fmt.Sprintf(sampleSkill, "dice-roller")))
	if err != nil {
		t.Fatalf("ParseSkillMD: %v", err)
	}
	if fm.Name != "dice-roller" || fm.License != "MIT" ||
		fm.Metadata["version"] != "2.1" ||
		!strings.Contains(fm.AllowedTools, "web_search") {
		t.Errorf("frontmatter = %+v", fm)
	}
	if !strings.Contains(body, "DICEWORD") || strings.Contains(body, "---") {
		t.Errorf("body = %q", body)
	}

	bad := []string{
		"no frontmatter at all",
		"---\nname: Bad-Upper\ndescription: x\n---\nbody",
		"---\nname: -lead\ndescription: x\n---\nbody",
		"---\nname: a--b\ndescription: x\n---\nbody",
		"---\nname: ok-name\ndescription: \"\"\n---\nbody",
		"---\nname: ok-name\ndescription: x", // unterminated
	}
	for _, c := range bad {
		if _, _, err := ParseSkillMD([]byte(c)); err == nil {
			t.Errorf("accepted invalid SKILL.md: %q", c[:min(40, len(c))])
		}
	}
}

func TestLoadDirEnforcesNameMatchAndDigests(t *testing.T) {
	parent := t.TempDir()
	dir := writeSkillDir(t, parent, "dice-roller")
	loaded, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if loaded.HasScripts || loaded.HasWasm {
		t.Errorf("flags = scripts:%v wasm:%v", loaded.HasScripts, loaded.HasWasm)
	}
	d1 := loaded.Digest

	// Digest is content-sensitive.
	if err := os.WriteFile(filepath.Join(dir, "references", "tables.md"),
		[]byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Digest == d1 {
		t.Error("digest unchanged after content edit")
	}

	// Directory/name mismatch refused.
	misnamed := filepath.Join(parent, "wrong-name")
	if err := os.Rename(dir, misnamed); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(misnamed); err == nil {
		t.Error("accepted name/directory mismatch")
	}
}

func TestMapAllowedTools(t *testing.T) {
	known := map[string]bool{"web_search": true, "read_page": true}
	matched, unmatched := MapAllowedTools("web_search Bash(git:*) read_page WeirdTool", known)
	if len(matched) != 2 || matched[0] != "web_search" || matched[1] != "read_page" {
		t.Errorf("matched = %v", matched)
	}
	if len(unmatched) != 2 {
		t.Errorf("unmatched = %v", unmatched)
	}
}

// ── DB-backed store tests ─────────────────────────────────────────

func storeForTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FAMILIAR_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping: FAMILIAR_TEST_DSN not set")
	}
	ctx := context.Background()
	adminPool, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	if _, err := adminPool.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS skillpkg_test`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path=skillpkg_test,public"))
	if err != nil {
		t.Fatalf("db.Open scoped: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := NewStore(pool, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func seedUserAndShard(t *testing.T, s *Store, user, shard string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ($1, $1, 'approved', 'user') ON CONFLICT (id) DO NOTHING`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.ExecContext(ctx, `
		INSERT INTO shards (id, owner_id, name, persistence, visibility, scope_tag, system_prompt)
		VALUES ($1, $2, $1, 'persistent', 'isolated', $1, 'x')
		ON CONFLICT (id) DO NOTHING`, shard, user); err != nil {
		t.Fatal(err)
	}
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

func zipSkill(t *testing.T, name string, wrap bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := ""
	if wrap {
		prefix = name + "-main/" // the GitHub-download shape
	}
	add := func(path, content string) {
		w, err := zw.Create(prefix + path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	add("SKILL.md", fmt.Sprintf(sampleSkill, name))
	add("references/tables.md", "# Tables\nSECRET_TABLE_CONTENT\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStore_ImportZipBindAndServe(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	user := uniqueName("spk-user")
	shard := uniqueName("spk-shard")
	other := uniqueName("spk-other")
	seedUserAndShard(t, s, user, shard)
	seedUserAndShard(t, s, user, other)
	name := uniqueName("dice")

	known := map[string]bool{"web_search": true, "read_page": true}
	pkg, err := s.ImportZip(ctx, zipSkill(t, name, true), user, "https://example.test/skill.zip", known)
	if err != nil {
		t.Fatalf("ImportZip: %v", err)
	}
	if pkg.SignatureStatus != "unsigned" || pkg.Version != "2.1" || pkg.HasScripts {
		t.Errorf("pkg = %+v", pkg)
	}
	if len(pkg.ToolsMatched) != 2 || len(pkg.ToolsUnmatched) != 1 {
		t.Errorf("tool mapping = %v / %v", pkg.ToolsMatched, pkg.ToolsUnmatched)
	}
	// Files landed on disk under Root/<name>.
	if _, err := os.Stat(filepath.Join(s.Root, name, "SKILL.md")); err != nil {
		t.Fatalf("installed file missing: %v", err)
	}
	// Re-import of the same name refused.
	if _, err := s.ImportZip(ctx, zipSkill(t, name, false), user, "", known); err == nil {
		t.Error("duplicate import accepted")
	}

	// Unbound shard: no access.
	if _, err := s.BodyForShard(ctx, shard, name); err == nil {
		t.Error("unbound shard read the skill body")
	}

	// Bind → body + files served; the OTHER shard still refused.
	if err := s.SetShardSkills(ctx, shard, []string{pkg.ID}); err != nil {
		t.Fatalf("SetShardSkills: %v", err)
	}
	body, err := s.BodyForShard(ctx, shard, name)
	if err != nil || !strings.Contains(body, "DICEWORD") {
		t.Fatalf("BodyForShard: %q %v", body, err)
	}
	content, err := s.FileForShard(ctx, shard, name, "references/tables.md")
	if err != nil || !strings.Contains(string(content), "SECRET_TABLE_CONTENT") {
		t.Fatalf("FileForShard: %v", err)
	}
	if _, err := s.BodyForShard(ctx, other, name); err == nil {
		t.Error("unbound (other) shard read the skill body")
	}
	if _, err := s.BodyForShard(ctx, "", name); err == nil {
		t.Error("trusted path (empty shard) read the skill body")
	}

	// Path jail: traversal and absolute paths refused.
	for _, evil := range []string{"../../../etc/passwd", "..", "/etc/passwd", "references/../../" + name + "/SKILL.md"} {
		if _, err := s.FileForShard(ctx, shard, name, evil); err == nil {
			t.Errorf("path jail let through %q", evil)
		}
	}

	// Disable kills access without unbinding.
	if err := s.SetDisabled(ctx, pkg.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if _, err := s.BodyForShard(ctx, shard, name); err == nil {
		t.Error("disabled package still served")
	}
	listed, _ := s.ListShardSkills(ctx, shard)
	if len(listed) != 0 {
		t.Errorf("disabled package still listed for shard: %d", len(listed))
	}

	// Delete removes row AND directory.
	if err := s.SetDisabled(ctx, pkg.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, pkg.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, name)); !os.IsNotExist(err) {
		t.Error("directory survived delete")
	}
}

func TestStore_ZipSlipRefused(t *testing.T) {
	s := storeForTest(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../escape.txt")
	w.Write([]byte("pwned"))
	zw.Close()
	if _, err := s.ImportZip(context.Background(), buf.Bytes(), "u", "", nil); err == nil {
		t.Fatal("zip-slip archive accepted")
	}
}

// A decompression bomb — many highly-compressible entries whose total
// decompressed size dwarfs the small compressed archive — must be
// rejected DURING extraction, before it can fill the temp dir, rather
// than by the post-extraction quota check.
func TestStore_ZipBombRefused(t *testing.T) {
	s := storeForTest(t)

	// Two 3MB zero-filled files: each under the 5MB per-file cap, but
	// 6MB together exceeds the 5MB cumulative cap. Zeros compress to
	// almost nothing, so the archive itself stays tiny.
	big := make([]byte, 3<<20)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"SKILL.md", "data.bin"} {
		w, _ := zw.Create(n)
		w.Write(big)
	}
	zw.Close()
	if len(buf.Bytes()) > maxZipBytes {
		t.Fatalf("test archive unexpectedly large (%d bytes)", len(buf.Bytes()))
	}
	if _, err := s.ImportZip(context.Background(), buf.Bytes(), "u", "", nil); err == nil {
		t.Fatal("decompression bomb accepted")
	}

	// A central directory stuffed past the entry cap is refused too.
	var buf2 bytes.Buffer
	zw2 := zip.NewWriter(&buf2)
	for i := 0; i <= maxZipFiles; i++ {
		w, _ := zw2.Create(fmt.Sprintf("f%d.txt", i))
		w.Write([]byte("x"))
	}
	zw2.Close()
	if _, err := s.ImportZip(context.Background(), buf2.Bytes(), "u", "", nil); err == nil {
		t.Fatal("over-count archive accepted")
	}
}

func TestStore_Rescan(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	user := uniqueName("spk-rescan")
	seedUserAndShard(t, s, user, uniqueName("spk-rs"))
	name := uniqueName("scanned")
	writeSkillDir(t, s.Root, name)

	added, updated, _, errs := s.Rescan(ctx, user, nil)
	if added != 1 || updated != 0 || len(errs) != 0 {
		t.Fatalf("rescan #1 = %d/%d/%v", added, updated, errs)
	}
	// Unchanged second pass is a no-op; an edit shows as updated.
	if a, u, _, _ := s.Rescan(ctx, user, nil); a != 0 || u != 0 {
		t.Fatalf("rescan #2 = %d/%d", a, u)
	}
	if err := os.WriteFile(filepath.Join(s.Root, name, "SKILL.md"),
		[]byte(fmt.Sprintf(sampleSkill, name)+"\nMore.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, u, _, _ := s.Rescan(ctx, user, nil); a != 0 || u != 1 {
		t.Fatalf("rescan #3 = %d/%d", a, u)
	}
}

func TestStore_RescanReconcilesMissingDirs(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	user := uniqueName("spk-missing")
	seedUserAndShard(t, s, user, uniqueName("spk-md"))
	name := uniqueName("vanishing")
	writeSkillDir(t, s.Root, name)

	if a, _, _, errs := s.Rescan(ctx, user, nil); a != 1 || len(errs) != 0 {
		t.Fatalf("seed rescan = %d/%v", a, errs)
	}
	pkg, err := s.GetByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	// Remove the directory; the next rescan must disable the row and
	// report it — not leave the catalog advertising a dead skill.
	if err := os.RemoveAll(filepath.Join(s.Root, name)); err != nil {
		t.Fatal(err)
	}
	_, _, missing, errs := s.Rescan(ctx, user, nil)
	if len(errs) != 0 {
		t.Fatalf("rescan errs: %v", errs)
	}
	found := false
	for _, m := range missing {
		if m == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing = %v, want it to contain %s", missing, name)
	}
	got, err := s.Get(ctx, pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled() {
		t.Fatal("vanished package still enabled after rescan")
	}

	// Already-disabled rows aren't re-reported on the next pass.
	_, _, missing2, _ := s.Rescan(ctx, user, nil)
	for _, m := range missing2 {
		if m == name {
			t.Fatal("disabled package reported missing again")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── USER-SKILLS-SPEC Phase A ─────────────────────────────────────

// The ownership matrix: a user skill is visible/usable by its owner
// only, lives under Root/users/<uid>/, shares a name with other
// scopes without colliding, and can only be bound to the OWNER's
// shards.
func TestStore_UserSkills_OwnershipAndScoping(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	alice := uniqueName("spk-alice")
	bob := uniqueName("spk-bob")
	aliceShard := uniqueName("spk-ash")
	bobShard := uniqueName("spk-bsh")
	seedUserAndShard(t, s, alice, aliceShard)
	seedUserAndShard(t, s, bob, bobShard)
	name := uniqueName("mine")
	known := map[string]bool{"web_search": true, "read_page": true}

	pkg, err := s.ImportZipForUser(ctx, alice, zipSkill(t, name, false), "", known)
	if err != nil {
		t.Fatalf("ImportZipForUser: %v", err)
	}
	if pkg.OwnerID != alice || pkg.Origin != "imported" || pkg.ChatEnabled {
		t.Errorf("pkg ownership = %+v", pkg)
	}
	// Disk lands in the per-user subtree.
	if _, err := os.Stat(filepath.Join(s.Root, "users", alice, name, "SKILL.md")); err != nil {
		t.Fatalf("user skill not under users/<uid>/: %v", err)
	}

	// Listing scope: owner sees it; the other user and the instance
	// library do not.
	mine, _ := s.ListForOwner(ctx, alice)
	if len(mine) != 1 || mine[0].ID != pkg.ID {
		t.Errorf("ListForOwner(alice) = %d items", len(mine))
	}
	if theirs, _ := s.ListForOwner(ctx, bob); len(theirs) != 0 {
		t.Error("bob sees alice's skill")
	}
	inst, _ := s.List(ctx)
	for _, p := range inst {
		if p.ID == pkg.ID {
			t.Error("user skill leaked into the instance list")
		}
	}
	if _, err := s.GetOwned(ctx, bob, pkg.ID); err == nil {
		t.Error("GetOwned crossed owners")
	}

	// Per-scope name uniqueness: bob and the instance can both hold
	// the same name.
	if _, err := s.ImportZipForUser(ctx, bob, zipSkill(t, name, false), "", known); err != nil {
		t.Fatalf("bob import of same name: %v", err)
	}
	if _, err := s.ImportZip(ctx, zipSkill(t, name, false), alice, "", known); err != nil {
		t.Fatalf("instance import of same name: %v", err)
	}
	// But the owner re-importing their own name is refused.
	if _, err := s.ImportZipForUser(ctx, alice, zipSkill(t, name, false), "", known); err == nil {
		t.Error("duplicate user import accepted")
	}

	// Binding: alice's shard binds + serves; bob's shard is refused
	// at bind time (SQL guard).
	if err := s.SetShardSkills(ctx, aliceShard, []string{pkg.ID}); err != nil {
		t.Fatalf("bind to owner's shard: %v", err)
	}
	body, err := s.BodyForShard(ctx, aliceShard, name)
	if err != nil || !strings.Contains(body, "DICEWORD") {
		t.Fatalf("BodyForShard(owner): %q %v", body, err)
	}
	if err := s.SetShardSkills(ctx, bobShard, []string{pkg.ID}); err == nil {
		t.Error("cross-owner shard binding accepted")
	}

	// Delete removes the per-user directory.
	if err := s.Delete(ctx, pkg.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "users", alice, name)); !os.IsNotExist(err) {
		t.Error("user skill directory survived delete")
	}
}

// The instance rescan must neither admit skills from the per-user
// subtree nor disable user rows during reconciliation.
func TestStore_RescanSkipsUserSubtree(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	user := uniqueName("spk-rsu")
	seedUserAndShard(t, s, user, uniqueName("spk-rsush"))
	known := map[string]bool{}

	userName := uniqueName("private")
	pkg, err := s.ImportZipForUser(ctx, user, zipSkill(t, userName, false), "", known)
	if err != nil {
		t.Fatalf("ImportZipForUser: %v", err)
	}
	instName := uniqueName("public")
	writeSkillDir(t, s.Root, instName)

	added, _, missing, errs := s.Rescan(ctx, user, known)
	if added != 1 || len(errs) != 0 {
		t.Fatalf("rescan = added %d errs %v (want 1 added: the instance skill only)", added, errs)
	}
	for _, m := range missing {
		if m == userName {
			t.Error("rescan reconciliation flagged a user skill as missing")
		}
	}
	got, err := s.GetOwned(ctx, user, pkg.ID)
	if err != nil || !got.Enabled() {
		t.Errorf("user skill after rescan: %+v %v (want enabled)", got, err)
	}
	// And the user skill was not double-admitted as an instance row.
	if _, err := s.GetByName(ctx, userName); err == nil {
		t.Error("user skill admitted into the instance namespace by rescan")
	}
}

// The per-user count quota refuses import #51.
func TestStore_UserImportQuota(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	user := uniqueName("spk-quota")
	seedUserAndShard(t, s, user, uniqueName("spk-qsh"))
	// Seed maxUserSkills rows directly — cheaper than 50 zip imports.
	for i := 0; i < maxUserSkills; i++ {
		if _, err := s.pool.ExecContext(ctx, `
			INSERT INTO skill_packages (name, description, digest, frontmatter, imported_by, owner_id)
			VALUES ($1, 'd', 'x', '{}'::jsonb, $2, $2)`,
			fmt.Sprintf("q-%d", i), user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ImportZipForUser(ctx, user, zipSkill(t, uniqueName("over"), false), "", nil); err == nil {
		t.Error("import over the quota accepted")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("unexpected quota error: %v", err)
	}
}

// ── USER-SKILLS-SPEC Phase B ─────────────────────────────────────

// The trusted-path content authz: BodyForUser/FileForUser require
// owner + enabled + chat_enabled; ListChatEnabled is the prompt-block
// source set.
func TestStore_UserChatAccess(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	alice := uniqueName("spk-chat-a")
	bob := uniqueName("spk-chat-b")
	seedUserAndShard(t, s, alice, uniqueName("spk-chat-ash"))
	seedUserAndShard(t, s, bob, uniqueName("spk-chat-bsh"))
	name := uniqueName("chatty")

	pkg, err := s.ImportZipForUser(ctx, alice, zipSkill(t, name, false), "", nil)
	if err != nil {
		t.Fatalf("ImportZipForUser: %v", err)
	}

	// Not chat-enabled yet: refused, and not in the prompt-block set.
	if _, err := s.BodyForUser(ctx, alice, name); err == nil {
		t.Error("BodyForUser served without chat_enabled")
	}
	if on, _ := s.ListChatEnabled(ctx, alice); len(on) != 0 {
		t.Errorf("ListChatEnabled before opt-in = %d", len(on))
	}

	if err := s.SetChatEnabled(ctx, pkg.ID, true); err != nil {
		t.Fatalf("SetChatEnabled: %v", err)
	}

	// Owner reads body + referenced file; the block set has it.
	body, err := s.BodyForUser(ctx, alice, name)
	if err != nil || !strings.Contains(body, "DICEWORD") {
		t.Fatalf("BodyForUser: %q %v", body, err)
	}
	content, err := s.FileForUser(ctx, alice, name, "references/tables.md")
	if err != nil || !strings.Contains(string(content), "SECRET_TABLE_CONTENT") {
		t.Fatalf("FileForUser: %v", err)
	}
	on, _ := s.ListChatEnabled(ctx, alice)
	if len(on) != 1 || !on[0].ChatEnabled {
		t.Errorf("ListChatEnabled after opt-in = %+v", on)
	}

	// Cross-user and anonymous: refused.
	if _, err := s.BodyForUser(ctx, bob, name); err == nil {
		t.Error("BodyForUser crossed owners")
	}
	if _, err := s.BodyForUser(ctx, "", name); err == nil {
		t.Error("BodyForUser served with no identity")
	}
	// Path jail holds on the user route too.
	if _, err := s.FileForUser(ctx, alice, name, "../../"+name+"/SKILL.md"); err == nil {
		t.Error("FileForUser path jail let a traversal through")
	}

	// Library-level disable trumps the chat opt-in.
	if err := s.SetDisabled(ctx, pkg.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BodyForUser(ctx, alice, name); err == nil {
		t.Error("disabled skill still served for chat")
	}
	if on, _ := s.ListChatEnabled(ctx, alice); len(on) != 0 {
		t.Errorf("disabled skill still in the prompt-block set: %d", len(on))
	}
}

// ── USER-SKILLS-SPEC Phase C ─────────────────────────────────────

func TestComposeSkillMD_RoundTrip(t *testing.T) {
	fm := Frontmatter{
		Name:         "compose-check",
		Description:  "A skill with: colons, and — dashes.",
		License:      "MIT",
		Metadata:     map[string]string{"version": "3.0"},
		AllowedTools: "web_search read_page",
	}
	body := "# Title\n\nDo the thing.\n\n- step one\n- step two"
	out, err := ComposeSkillMD(fm, body)
	if err != nil {
		t.Fatalf("ComposeSkillMD: %v", err)
	}
	got, gotBody, err := ParseSkillMD(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got.Name != fm.Name || got.Description != fm.Description ||
		got.License != "MIT" || got.Metadata["version"] != "3.0" ||
		got.AllowedTools != fm.AllowedTools {
		t.Errorf("frontmatter mangled: %+v", got)
	}
	if gotBody != body {
		t.Errorf("body mangled: %q", gotBody)
	}
	// Invalid inputs refused before anything touches disk.
	if _, err := ComposeSkillMD(Frontmatter{Name: "Bad Name", Description: "d"}, "b"); err == nil {
		t.Error("bad name composed")
	}
	if _, err := ComposeSkillMD(Frontmatter{Name: "ok-name", Description: "   "}, "b"); err == nil {
		t.Error("empty description composed")
	}
}

// The authored lifecycle: create → edit → chat-enable → the imported
// read-only rule.
func TestStore_AuthoredLifecycle(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	alice := uniqueName("spk-auth")
	seedUserAndShard(t, s, alice, uniqueName("spk-auth-sh"))
	name := uniqueName("written")

	pkg, err := s.SaveAuthored(ctx, alice, name, "House grocery rules.", "# Rules\n\nFIRSTBODY", nil)
	if err != nil {
		t.Fatalf("SaveAuthored (create): %v", err)
	}
	if pkg.Origin != "authored" || pkg.OwnerID != alice {
		t.Errorf("pkg = %+v", pkg)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "users", alice, name, "SKILL.md")); err != nil {
		t.Fatalf("authored SKILL.md missing: %v", err)
	}

	// Edit: body + description change, digest moves, origin sticks.
	edited, err := s.SaveAuthored(ctx, alice, name, "Updated rules.", "# Rules\n\nSECONDBODY", nil)
	if err != nil {
		t.Fatalf("SaveAuthored (edit): %v", err)
	}
	if edited.ID != pkg.ID || edited.Digest == pkg.Digest || edited.Description != "Updated rules." || edited.Origin != "authored" {
		t.Errorf("edit: %+v (was digest %s)", edited, pkg.Digest)
	}
	loaded, _, err := s.ContentForOwner(ctx, alice, pkg.ID)
	if err != nil || !strings.Contains(loaded.Body, "SECONDBODY") {
		t.Fatalf("ContentForOwner: %v %q", err, loaded.Body)
	}

	// Authored + chat-enabled serves on the trusted path (Phase B
	// authz composes with Phase C content).
	if err := s.SetChatEnabled(ctx, pkg.ID, true); err != nil {
		t.Fatal(err)
	}
	body, err := s.BodyForUser(ctx, alice, name)
	if err != nil || !strings.Contains(body, "SECONDBODY") {
		t.Fatalf("BodyForUser on authored: %q %v", body, err)
	}

	// An imported skill is read-only under PUT semantics.
	impName := uniqueName("readonly")
	if _, err := s.ImportZipForUser(ctx, alice, zipSkill(t, impName, false), "https://example.test/ro.zip", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveAuthored(ctx, alice, impName, "d", "b", nil); err == nil {
		t.Error("SaveAuthored overwrote an imported skill")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Duplicate keeps the tree + provenance and becomes editable; export
// round-trips through import.
func TestStore_DuplicateAndExport(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	alice := uniqueName("spk-dup")
	bob := uniqueName("spk-dup-b")
	seedUserAndShard(t, s, alice, uniqueName("spk-dup-sh"))
	seedUserAndShard(t, s, bob, uniqueName("spk-dup-bsh"))
	name := uniqueName("original")

	src, err := s.ImportZipForUser(ctx, alice, zipSkill(t, name, false), "https://example.test/src.zip", nil)
	if err != nil {
		t.Fatal(err)
	}

	copyName := uniqueName("copy")
	dup, err := s.Duplicate(ctx, alice, src.ID, copyName, nil)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if dup.Origin != "authored" || dup.SourceURL != "https://example.test/src.zip" {
		t.Errorf("duplicate provenance: %+v", dup)
	}
	// References came along; frontmatter renamed; now editable.
	if _, err := os.Stat(filepath.Join(s.Root, "users", alice, copyName, "references", "tables.md")); err != nil {
		t.Errorf("references not copied: %v", err)
	}
	loaded, _, err := s.ContentForOwner(ctx, alice, dup.ID)
	if err != nil || loaded.Frontmatter.Name != copyName {
		t.Fatalf("duplicate content: %v %+v", err, loaded)
	}
	if _, err := s.SaveAuthored(ctx, alice, copyName, "tweaked", "new body", nil); err != nil {
		t.Errorf("duplicate not editable: %v", err)
	}
	// Cross-owner duplicate refused.
	if _, err := s.Duplicate(ctx, bob, src.ID, uniqueName("steal"), nil); err == nil {
		t.Error("cross-owner duplicate accepted")
	}

	// Export → bob imports the archive and gets the same body.
	fname, data, err := s.ExportZip(ctx, alice, src.ID)
	if err != nil || fname != name+".zip" {
		t.Fatalf("ExportZip: %v (%s)", err, fname)
	}
	reimported, err := s.ImportZipForUser(ctx, bob, data, "", nil)
	if err != nil {
		t.Fatalf("re-import of export: %v", err)
	}
	if reimported.Digest != src.Digest {
		t.Errorf("export round-trip digest mismatch: %s vs %s", reimported.Digest, src.Digest)
	}
}

// capSkillBody must pass a normal SKILL.md through untouched but bound
// a pathological (imported, multi-MB) one before it goes into the tool
// loop — with valid UTF-8 and a marker telling the model where to find
// the rest.
func TestCapSkillBody(t *testing.T) {
	small := "# My Skill\n\nDo the thing."
	if got := capSkillBody(small); got != small {
		t.Errorf("small body was altered: %q", got)
	}

	// Over-cap body, and put a multi-byte rune straddling the cut so
	// the ToValidUTF8 path is exercised.
	huge := strings.Repeat("a", maxSkillFileBytes-1) + "é" + strings.Repeat("b", 1000)
	got := capSkillBody(huge)
	if len(got) > maxSkillFileBytes+200 {
		t.Errorf("capped body too long: %d bytes", len(got))
	}
	if !strings.Contains(got, "truncated at") {
		t.Errorf("capped body missing truncation marker")
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped body is not valid UTF-8 (split a rune)")
	}
}
