package sidecar

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/classifier"
)

// Classifier eval harness.
//
// Answers the standing question "does the classifier earn its cost?" on the
// QUALITY axis (the mechanism trace already settled that all four fields are
// load-bearing). It runs the REAL classify path over a curated labeled
// fixture and reports per-field accuracy, the search-veto confusion (the
// safety-critical field — a false SearchNone answers a recency turn from
// stale weights), and how much of the trivial set the deterministic
// fast-path already covers.
//
// Two parts:
//   - The fast-path consistency check is pure Go and ALWAYS runs: the
//     deterministic gate must never fire on a fixture turn whose reference
//     verdict isn't off/none/none. That's a hard correctness guard on the
//     curated set vs. the gate.
//   - The model eval is gated on FAMILIAR_CLASSIFIER_EVAL_URL (the classify
//     endpoint, e.g. http://127.0.0.1:8080). It reports; it fails only if the
//     endpoint is unusable, so normal model variance doesn't flake CI.
//
// Point it at the deployed classify model to measure gemma; pointing it at a
// stronger model (the local 122B) measures the prompt's ceiling instead.

const evalEnvURL = "FAMILIAR_CLASSIFIER_EVAL_URL"

type evalCase struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	History []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"history"`
	Want struct {
		Thinking string `json:"thinking"`
		Memory   string `json:"memory"`
		Search   string `json:"search"`
	} `json:"want"`
	Note string `json:"note"`
}

func loadEvalFixture(t *testing.T) []evalCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/classifier_eval.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		Cases []evalCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return fx.Cases
}

var thinkingRank = map[string]int{"off": 0, "low": 1, "medium": 2, "high": 3}

func isTrivialVerdict(want struct {
	Thinking string `json:"thinking"`
	Memory   string `json:"memory"`
	Search   string `json:"search"`
}) bool {
	return want.Thinking == "off" && want.Memory == "none" && want.Search == "none"
}

// The deterministic fast-path must never fire on a turn whose reference
// verdict isn't trivial (off/none/none) — a false trivial would strip
// reasoning, memory, and search from a real turn. Pure Go; always runs.
func TestClassifierFastPathAgreesWithFixture(t *testing.T) {
	cases := loadEvalFixture(t)
	fired, trivialTotal, trivialCovered := 0, 0, 0
	for _, c := range cases {
		_, ok := classifier.TrivialFastPath(c.Message)
		if isTrivialVerdict(c.Want) {
			trivialTotal++
			if ok {
				trivialCovered++
			}
		}
		if ok {
			fired++
			if !isTrivialVerdict(c.Want) {
				t.Errorf("case %q: fast-path fired but reference verdict is %s/%s/%s, not off/none/none — the gate is too aggressive",
					c.ID, c.Want.Thinking, c.Want.Memory, c.Want.Search)
			}
		}
	}
	t.Logf("fast-path: fired on %d/%d cases; covers %d/%d trivial cases (recall %.0f%%) — the rest of the trivial turns rely on the classifier",
		fired, len(cases), trivialCovered, trivialTotal, pct(trivialCovered, trivialTotal))
}

// The model eval: run the real classify path over the fixture and report.
func TestClassifierEvalAgainstModel(t *testing.T) {
	url := os.Getenv(evalEnvURL)
	if url == "" {
		t.Skipf("skipping model eval: set %s to the classify endpoint (e.g. http://127.0.0.1:8080)", evalEnvURL)
	}
	cases := loadEvalFixture(t)
	r := NewHTTPRouter(url)

	var (
		total, valid             int
		thinkExact, thinkWithin1 int
		memExact, searchExact    int
		// search veto confusion
		needSearch, missedSearch int // want != none; got none = dangerous miss
		noSearch, falseSearch    int // want none; got != none = wasted-call
	)
	type disagreement struct {
		id, got, want string
	}
	var worst []disagreement

	for _, c := range cases {
		total++
		history := make([]Turn, 0, len(c.History))
		for _, h := range c.History {
			history = append(history, Turn{Role: h.Role, Content: h.Content})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, _, err := r.classifyEffortWithUsage(ctx, history, c.Message)
		cancel()
		if err != nil || !out.Validate() {
			t.Logf("case %q: no valid verdict (err=%v out=%+v)", c.ID, err, out)
			continue
		}
		valid++

		got := struct{ th, mem, se string }{
			string(out.Thinking), string(out.MemoryDepth), string(out.SearchDepth),
		}
		if got.th == c.Want.Thinking {
			thinkExact++
		}
		if abs(thinkingRank[got.th]-thinkingRank[c.Want.Thinking]) <= 1 {
			thinkWithin1++
		}
		if got.mem == c.Want.Memory {
			memExact++
		}
		if got.se == c.Want.Search {
			searchExact++
		}

		// Search veto — the field where a wrong answer silently degrades.
		if c.Want.Search != "none" {
			needSearch++
			if got.se == "none" {
				missedSearch++
				worst = append(worst, disagreement{c.ID, "search=none", "search=" + c.Want.Search + " (MISSED — stale-answer risk)"})
			}
		} else {
			noSearch++
			if got.se != "none" {
				falseSearch++
			}
		}
		// Log any full-verdict disagreement for eyeballing.
		if got.th != c.Want.Thinking || got.mem != c.Want.Memory || got.se != c.Want.Search {
			t.Logf("  ~ %-24s got %s/%s/%s  want %s/%s/%s  (%s)",
				c.ID, got.th, got.mem, got.se, c.Want.Thinking, c.Want.Memory, c.Want.Search, c.Note)
		}
	}

	t.Logf("=== classifier eval vs %s ===", url)
	t.Logf("valid verdicts:        %d/%d", valid, total)
	if valid == 0 {
		t.Fatalf("endpoint returned no valid verdicts — check %s", evalEnvURL)
	}
	t.Logf("thinking exact:        %d/%d (%.0f%%)   within-1-band: %d/%d (%.0f%%)",
		thinkExact, valid, pct(thinkExact, valid), thinkWithin1, valid, pct(thinkWithin1, valid))
	t.Logf("memory  exact:         %d/%d (%.0f%%)", memExact, valid, pct(memExact, valid))
	t.Logf("search  exact:         %d/%d (%.0f%%)", searchExact, valid, pct(searchExact, valid))
	t.Logf("search VETO (critical): needed=%d missed=%d (%.0f%% of search-needed turns wrongly vetoed)",
		needSearch, missedSearch, pct(missedSearch, needSearch))
	t.Logf("search waste:          none-needed=%d false-search=%d (%.0f%% wasted-call rate)",
		noSearch, falseSearch, pct(falseSearch, noSearch))
	for _, d := range worst {
		t.Logf("  !! %-24s %s vs want %s", d.id, d.got, d.want)
	}

	// Smoke floor only — a broken prompt/endpoint, not normal variance.
	if pct(thinkWithin1, valid) < 40 {
		t.Errorf("thinking within-1-band accuracy %.0f%% is implausibly low — prompt or endpoint likely broken", pct(thinkWithin1, valid))
	}
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
