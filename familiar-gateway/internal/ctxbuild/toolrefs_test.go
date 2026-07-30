package ctxbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func names(issues []ToolRefIssue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Name)
	}
	return out
}

func has(issues []ToolRefIssue, name string) bool {
	for _, i := range issues {
		if i.Name == name {
			return true
		}
	}
	return false
}

// The two names that actually shipped broken must be caught in every form
// the real prompts used them in.
func TestAuditToolRefs_CatchesTheRealPhantoms(t *testing.T) {
	known := map[string]bool{"search_memory": true, "save_fact": true}
	cases := []struct {
		name string
		text string
		want string
	}{
		{"bold documentation entry",
			"- **core_memory_update** — Update your working knowledge of the user.",
			"core_memory_update"},
		{"MUST call", "  stated in a [MEMORY] block, you MUST call memory_search.", "memory_search"},
		{"call with args prose", "- call memory_search with a targeted query", "memory_search"},
		{"use before responding", "If not, use memory_search before responding.", "memory_search"},
		{"routing arrow", "   - Factual recall about the owner/systems → memory_search", "memory_search"},
		{"backticked after call", "call `memory_search` again", "memory_search"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AuditToolRefs(c.text, known)
			if !has(got, c.want) {
				t.Fatalf("did not flag %q in %q; got %v", c.want, c.text, names(got))
			}
		})
	}
}

// Parameter and field names appear all over these prompts and are not
// tools. Flagging them would make the warning noise and get it ignored.
func TestAuditToolRefs_IgnoresParametersAndFields(t *testing.T) {
	known := map[string]bool{"read_page": true, "recent_scheduled_runs": true, "update_page": true}
	// Every line is real text from the shipped prompts.
	text := strings.Join([]string{
		"1. `read_page(book_slug, page_slug)` — get the current content",
		"  `action_name` to narrow).",
		`- "when did that last run?" → call it and read ` + "`finished_at`" + ".",
		"**recent_scheduled_runs** — list recent runs",
	}, "\n")

	got := AuditToolRefs(text, known)
	if len(got) != 0 {
		t.Fatalf("expected no issues, got %v", names(got))
	}
}

// A registered tool referenced correctly must never be reported, and the
// audit must not fire on an unrelated snake_case word in prose.
func TestAuditToolRefs_CleanTextIsSilent(t *testing.T) {
	known := map[string]bool{"search_memory": true, "web_search": true}
	text := strings.Join([]string{
		"- **search_memory** — Search your long-term memory.",
		"4. Prefer search_memory for questions about the user's stuff.",
		"Prefer web_search for general knowledge.",
		"The auto_retrieved block is not a tool reference in a call position.",
	}, "\n")
	if got := AuditToolRefs(text, known); len(got) != 0 {
		t.Fatalf("expected silence, got %v", names(got))
	}
}

// One name referenced many times is one issue, with bounded context.
func TestAuditToolRefs_DedupesAndBoundsContexts(t *testing.T) {
	known := map[string]bool{"search_memory": true}
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("you MUST call memory_search now.\n")
	}
	got := AuditToolRefs(b.String(), known)
	if len(got) != 1 {
		t.Fatalf("expected 1 deduped issue, got %v", names(got))
	}
	if n := len(got[0].Contexts); n == 0 || n > 3 {
		t.Errorf("contexts = %d, want 1..3 (bounded, non-empty)", n)
	}
	for _, c := range got[0].Contexts {
		if strings.ContainsAny(c, "\n\r") {
			t.Errorf("context must be single-line for logging: %q", c)
		}
	}
}

// Degenerate inputs must be silent, not panic and not flood.
func TestAuditToolRefs_DegenerateInputs(t *testing.T) {
	if got := AuditToolRefs("", map[string]bool{"a_b": true}); got != nil {
		t.Errorf("empty text should yield nil, got %v", names(got))
	}
	if got := AuditToolRefs("   \n\t ", map[string]bool{"a_b": true}); got != nil {
		t.Errorf("blank text should yield nil, got %v", names(got))
	}
	// No registry to check against: disable rather than report everything.
	if got := AuditToolRefs("call memory_search", nil); got != nil {
		t.Errorf("nil known-set should disable the audit, got %v", names(got))
	}
	if got := AuditToolRefs("call memory_search", map[string]bool{}); got != nil {
		t.Errorf("empty known-set should disable the audit, got %v", names(got))
	}
}

// AuditedPromptText must cover every layer a model could be shown —
// including the tool policy and each tier overlay, since the original bug
// was a name that was correct ONLY in tool_policy.md (the one file the
// trivial tier omits) and wrong in every overlay.
func TestAuditedPromptText_CoversEveryLayer(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"base.md":           "BASE_TEXT",
		"tool_policy.md":    "POLICY_TEXT",
		"tier_trivial.md":   "TRIVIAL_TEXT",
		"tier_knowledge.md": "KNOWLEDGE_TEXT",
		"tier_reasoning.md": "REASONING_TEXT",
		"tier_deep.md":      "DEEP_TEXT",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	ps, err := NewPromptStore(dir, "FALLBACK_TEXT")
	if err != nil {
		t.Fatalf("NewPromptStore: %v", err)
	}
	got := ps.AuditedPromptText()
	for _, want := range []string{
		"BASE_TEXT", "POLICY_TEXT", "TRIVIAL_TEXT",
		"KNOWLEDGE_TEXT", "REASONING_TEXT", "DEEP_TEXT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("AuditedPromptText missing %s — a phantom in that layer would go unreported", want)
		}
	}

	// An admin base override replaces base.md and must itself be audited.
	ps.SetBaseOverride("OVERRIDE_TEXT")
	got = ps.AuditedPromptText()
	if !strings.Contains(got, "OVERRIDE_TEXT") {
		t.Error("base override must be audited")
	}
	if strings.Contains(got, "BASE_TEXT") {
		t.Error("overridden base should not also be audited")
	}

	// A store with no dir serves the monolithic fallback; audit it too.
	bare, err := NewPromptStore("", "MONOLITHIC_FALLBACK")
	if err != nil {
		t.Fatalf("NewPromptStore(bare): %v", err)
	}
	if !strings.Contains(bare.AuditedPromptText(), "MONOLITHIC_FALLBACK") {
		t.Error("fallback prompt must be audited")
	}
	// Nil receiver must not panic (the store is optional at some sites).
	var nilStore *PromptStore
	if nilStore.AuditedPromptText() != "" {
		t.Error("nil store should yield empty text")
	}
}

// The SHIPPED prompts must not reference a phantom tool.
//
// Caveat worth knowing: prompts/ lives outside this Go module, so the test
// cache does not key on those files — a prompt-only edit can replay a
// stale PASS locally. Run with -count=1 (as `make test-integration` does)
// or in a cold CI checkout. The authoritative guard is the boot-time audit
// in cmd/gateway, which uses the live registry.
func TestShippedPromptsReferenceOnlyRealTools(t *testing.T) {
	const promptDir = "../../../prompts"
	if _, err := os.Stat(promptDir); err != nil {
		t.Skipf("shipped prompts not found at %s: %v", promptDir, err)
	}
	var b strings.Builder
	err := filepath.WalkDir(promptDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		b.Write(data)
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("reading shipped prompts: %v", err)
	}
	if strings.TrimSpace(b.String()) == "" {
		t.Fatal("read no prompt text — the guard would pass vacuously")
	}

	for _, is := range AuditToolRefs(b.String(), shippedToolNames()) {
		t.Errorf("shipped prompts instruct the model to call %q, which no skill registers.\n  referenced near: %s",
			is.Name, strings.Join(is.Contexts, " | "))
	}
}

// shippedToolNames is the tool set the first-party skills register.
//
// Duplicated from the skills packages on purpose: importing them here
// would make internal/ctxbuild depend on every skill (and on their
// constructors' dependencies), and research's tool set is config-dependent
// so a reconstruction would drift anyway. The cost of this list going
// stale is a false failure on a REAL tool, which is loud, obvious, and
// fixed by adding one line — not a silent miss. The boot-time audit uses
// the live registry and has no such list.
func shippedToolNames() map[string]bool {
	known := map[string]bool{}
	for _, n := range []string{
		// memory
		"save_fact", "remember", "search_memory", "list_my_memories",
		"forget_fact", "correct_fact",
		// notes
		"create_note", "read_note", "update_note", "append_to_note",
		"patch_note", "search_notes", "list_recent_notes",
		// wiki
		"list_books", "list_pages", "read_page", "create_page",
		"update_page", "append_to_page", "patch_page", "pin_page",
		"search_pages",
		// web / news / weather / datetime / instance
		"web_search", "fetch_page", "get_news", "search_news",
		"get_current_weather", "get_forecast", "get_current_datetime",
		"get_instance_info",
		// profile
		"update_my_email",
		// skill packages (shard-only, still nameable in prompts)
		"use_skill", "read_skill_file",
		// scheduled actions + research (registered late, in httpadapter)
		"recent_scheduled_runs", "spawn_research_workers",
		"compose_research_note",
	} {
		known[n] = true
	}
	return known
}
