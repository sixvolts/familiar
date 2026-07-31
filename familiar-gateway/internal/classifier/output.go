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
//	  "search_depth": "none" | "shallow" | "deep"
//	}
type Output struct {
	Thinking    ThinkingLevel `json:"thinking"`
	MemoryDepth MemoryDepth   `json:"memory_depth"`
	SearchDepth SearchDepth   `json:"search_depth"`

	// Source records where this verdict came from. Never decoded from
	// the model (json:"-") — the classifier stamps it. Two reasons it
	// exists: the turn log needs to distinguish a real verdict from a
	// fallback (otherwise the fallback rate is unknowable), and a
	// consumer must be able to tell "the model said don't search" from
	// "we never got an answer", which are very different facts.
	Source Source `json:"-"`
}

// Source is the provenance of a classifier verdict.
type Source string

const (
	// SourceModel — the classifier answered and the verdict parsed.
	SourceModel Source = "model"
	// SourceStatic — we learned nothing about this turn: no classifier
	// wired, its chain offline, or the request never completed. Carries
	// StaticDefault.
	SourceStatic Source = "static"
	// SourceUnparsed — the model answered but the output was unusable.
	// Distinct from SourceStatic because a responding-but-confused
	// classifier is evidence the turn may be unusual, so this one keeps
	// ConservativeFallback.
	SourceUnparsed Source = "unparsed"
)

// FromModel reports whether the verdict reflects an actual classifier
// decision. Consumers that would otherwise treat a fallback's level as
// an affirmative instruction should gate on this.
func (o Output) FromModel() bool { return o.Source == SourceModel }

// ConservativeFallback is the EXPENSIVE fallback: thinking and memory
// both at their highest non-search settings, search off.
//
// Its scope is now narrow. It applies only when the classifier actually
// responded and its levels failed Validate() — a model that answered but
// could not be read is weak evidence the turn is unusual, so paying more
// is defensible. Every other failure (no client, chain offline, transport
// error, timeout, unparseable body) uses StaticDefault instead: see the
// reasoning there for why "we could not classify" must not mean "spend
// maximum effort".
//
// Original rationale, from CHAT-REARCH §"Classifier — Behavior": prefer
// wasted tokens over a degraded answer.
func ConservativeFallback() Output {
	return Output{
		Thinking:    ThinkingHigh,
		MemoryDepth: MemoryDeep,
		SearchDepth: SearchNone,
		Source:      SourceUnparsed,
	}
}

// StaticDefault is what to use when we learned NOTHING about the turn —
// no classifier configured, its chain offline, or the request failed or
// timed out.
//
// ConservativeFallback is the wrong answer for those cases. It is the
// most expensive configuration in the system, so using it on a timeout
// means a saturated box responds to "we're too busy to classify" by
// demanding maximum work from itself: widest retrieval, largest token
// ceiling, heaviest prompt overlay. That is an amplifying loop, and it
// fires exactly when the box can least afford it. Reserve
// ConservativeFallback for the one case where it is justified — the
// model DID answer and we couldn't read it, which is weak evidence the
// turn is unusual.
//
// Search is Shallow, not None, and that is deliberate: SearchNone is a
// hard veto (it sets webSearchDisabled), so defaulting to None means a
// classifier outage silently makes web search impossible — and "look up
// the latest X" then gets answered from stale weights with full
// confidence. One permitted call lets the chat model decide for itself;
// it does not force a search. A wasted Brave call is a much better
// failure than a confidently stale answer.
func StaticDefault() Output {
	return Output{
		Thinking:    ThinkingMedium,
		MemoryDepth: MemoryShallow,
		SearchDepth: SearchShallow,
		Source:      SourceStatic,
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
