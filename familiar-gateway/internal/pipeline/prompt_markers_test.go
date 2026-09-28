package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/ctxbuild"
)

// The shipped prompts describe the context the pipeline actually builds.
// They sent the model looking for "[MEMORY] blocks" (memories arrive
// under "Relevant context"), a "working context" and a "user profile"
// with the user's name and role (both removed), and told trivial turns
// no tools were needed while giving them the memory tools. A literal
// model then called search_memory for facts it already had, or invented
// a profile.
//
// prompts/ is outside this module, so the test cache doesn't key on it:
// run with -count=1 after a prompt-only edit.
func TestShippedPromptsMatchTheContextFormat(t *testing.T) {
	const dir = "../../../prompts"
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("shipped prompts not found at %s: %v", dir, err)
	}
	texts := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			if b, rerr := os.ReadFile(p); rerr == nil {
				texts[p] = string(b)
			}
		}
		return nil
	})
	if len(texts) == 0 {
		t.Fatal("read no prompts")
	}
	mentionsRelevant := false
	for p, s := range texts {
		lower := strings.ToLower(s)
		for _, gone := range []string{"[memory]", "working context", "user profile"} {
			if strings.Contains(lower, gone) {
				t.Errorf("%s refers to %q, which the pipeline no longer produces", p, gone)
			}
		}
		if strings.Contains(s, "No tools needed") {
			t.Errorf("%s says no tools are needed; trivial turns get the memory tools", p)
		}
		mentionsRelevant = mentionsRelevant || strings.Contains(s, "Relevant context")
	}
	// What they call the memories is what flattenAssembled labels them.
	sys := flattenAssembled(ctxbuild.AssembledContext{Memories: []ctxbuild.Memory{{Content: "- x"}}}, "q")[0].Content
	if mentionsRelevant && !strings.Contains(sys, "Relevant context") {
		t.Errorf("prompts refer to \"Relevant context\" but the memories are labelled otherwise:\n%s", sys)
	}
}
