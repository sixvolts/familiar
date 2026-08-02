package classifier

import "testing"

func TestTrivialFastPath_FiresOnPleasantries(t *testing.T) {
	// Each must produce the off/none/none verdict stamped SourceFastPath.
	cases := []string{
		"thanks", "Thanks!", "THANKS", "thank you", "Thank you so much!!",
		"ty", "thx", "much appreciated", "appreciate it", "cheers",
		"hi", "Hello", "hey there", "good morning", "yo",
		"bye", "see you later", "ttyl", "take care", "good night",
		"lol", "haha", "cool", "nice", "awesome", "perfect", "well done",
		"  thanks  ", "thanks 🙏", "🙏 thanks",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			v, ok := TrivialFastPath(msg)
			if !ok {
				t.Fatalf("expected fast-path to fire on %q", msg)
			}
			if v.Thinking != ThinkingOff || v.MemoryDepth != MemoryNone || v.SearchDepth != SearchNone {
				t.Errorf("%q verdict = %+v, want off/none/none", msg, v)
			}
			if v.Source != SourceFastPath {
				t.Errorf("%q source = %q, want %q", msg, v.Source, SourceFastPath)
			}
			if v.Validate() != true {
				t.Errorf("%q trivial verdict must pass Validate()", msg)
			}
		})
	}
}

func TestTrivialFastPath_FallsThroughWhenAmbiguousOrReal(t *testing.T) {
	// The dangerous cases: bare affirmations/negations that CAN carry an
	// instruction in context (deliberately excluded), questions, and any
	// real request. All must fall through to the classifier (ok=false).
	cases := []string{
		// excluded affirmations/negations — could be "yes, save it"
		"yes", "no", "yeah", "nope", "sure", "ok", "okay", "k", "got it",
		"do it", "go ahead", "sounds good", "no problem",
		// questions are never trivial
		"thanks?", "what's up?", "hi, what's the weather?",
		// real requests that merely start social
		"hi, can you help me debug this?", "thanks for that, now what about X",
		"hello world program in go",
		// substantive
		"what is the capital of France", "reboot gpu-host",
		"remember my api key is sk-123",
		// empty / punctuation-only
		"", "   ", "!!!", "???",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			if _, ok := TrivialFastPath(msg); ok {
				t.Errorf("fast-path must NOT fire on %q (should reach the classifier)", msg)
			}
		})
	}
}

func TestNormalizeTrivial(t *testing.T) {
	cases := map[string]string{
		"Thanks!!":          "thanks",
		"  good   morning ": "good morning",
		"HELLO":             "hello",
		"thanks?":           "", // question → not trivial
		"🙏":                 "", // no letters
		"":                  "",
	}
	for in, want := range cases {
		if got := normalizeTrivial(in); got != want {
			t.Errorf("normalizeTrivial(%q) = %q, want %q", in, got, want)
		}
	}
}

// The gate must not inspect long messages at all (cheap guard + it can't be
// a bare pleasantry).
func TestTrivialFastPath_LongMessageSkipped(t *testing.T) {
	long := "thanks so much for all of the incredibly detailed help today"
	if _, ok := TrivialFastPath(long); ok {
		t.Errorf("long message should fall through, got fast-path hit")
	}
}
