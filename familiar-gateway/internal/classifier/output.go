// Package classifier defines the gateway's classification output —
// the ordinal effort levels the classifier emits per turn, plus the
// helpers consumers use to translate those levels into concrete
// budgets at request time. CHAT-REARCH §"Classifier".
//
// The schema is intentionally categorical. The classifier picks the
// LEVEL (low / medium / high, etc.); config decides what each level
// MEANS in tokens, top-k, max-searches, and so on. Decoupling lets
// the budgets be tuned without retraining or re-prompting.
package classifier

// ThinkingLevel is the requested thinking budget for the chat model.
// "off" disables thinking entirely; the others scale to a token
// budget at request time via [effort.thinking.<level>].
type ThinkingLevel string

const (
	ThinkingOff    ThinkingLevel = "off"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
)

// MemoryDepth controls how aggressively the pipeline retrieves
// memories for the turn. "none" skips retrieval entirely; "shallow"
// is the default-safe path; "deep" widens both top-k and threshold.
type MemoryDepth string

const (
	MemoryNone    MemoryDepth = "none"
	MemoryShallow MemoryDepth = "shallow"
	MemoryDeep    MemoryDepth = "deep"
)

// SearchDepth controls how many web-search calls the chat model is
// allowed to make during the tool loop for this turn. "none" disables
// search entirely; "shallow" caps at one call; "deep" allows several.
type SearchDepth string

const (
	SearchNone    SearchDepth = "none"
	SearchShallow SearchDepth = "shallow"
	SearchDeep    SearchDepth = "deep"
)

// Output is the per-turn classifier verdict. The pipeline reads
// this once before context assembly and threads each field into
// the appropriate consumer.
//
// Wire shape (JSON, from the classifier model):
//
//	{
//	  "thinking":     "off" | "low" | "medium" | "high",
//	  "memory_depth": "none" | "shallow" | "deep",
//	  "search_depth": "none" | "shallow" | "deep",
//	  "tools":        ["search", "notes_read", ...]
//	}
type Output struct {
	Thinking    ThinkingLevel `json:"thinking"`
	MemoryDepth MemoryDepth   `json:"memory_depth"`
	SearchDepth SearchDepth   `json:"search_depth"`
	Tools       []string      `json:"tools"`
}

// ConservativeFallback is what the pipeline uses when the classifier
// is unreachable, returns malformed JSON, or otherwise fails. Per
// the spec: prefer wasted tokens over a degraded answer (so thinking
// + memory both go to their highest non-search settings; search
// stays off because a hallucinated query is worse than no search).
//
// CHAT-REARCH §"Classifier — Behavior".
func ConservativeFallback() Output {
	return Output{
		Thinking:    ThinkingHigh,
		MemoryDepth: MemoryDeep,
		SearchDepth: SearchNone,
		Tools:       nil,
	}
}

// Validate reports whether every field carries a known ordinal.
// Used by the classifier-output parser to decide between accepting
// the model's output and falling back to ConservativeFallback().
func (o Output) Validate() bool {
	switch o.Thinking {
	case ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh:
	default:
		return false
	}
	switch o.MemoryDepth {
	case MemoryNone, MemoryShallow, MemoryDeep:
	default:
		return false
	}
	switch o.SearchDepth {
	case SearchNone, SearchShallow, SearchDeep:
	default:
		return false
	}
	return true
}
