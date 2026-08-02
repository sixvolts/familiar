package classifier

import (
	"strings"
	"unicode"
)

// Deterministic trivial fast-path.
//
// The classifier is load-bearing on real turns — it sets the thinking
// budget, the memory retrieval breadth (including "skip retrieval"), and
// the web-search gate. But it runs on EVERY trusted turn, including a
// one-word "thanks", where its ideal verdict (no thinking, no retrieval, no
// search) is deterministically knowable without a model call. This gate
// recognizes that closed set and short-circuits, saving both the classify
// round-trip and the retrieval it would otherwise gate.
//
// Design contract — the risk is ASYMMETRIC:
//   - A missed trivial (false negative) just pays the classify cost we
//     already pay today. Harmless.
//   - A false trivial (false positive) degrades a real turn: the trivial
//     verdict is off/none/none, so a turn wrongly gated here loses reasoning,
//     memory retrieval, AND web search.
//
// So the gate is deliberately HIGH-PRECISION / LOW-RECALL: it fires ONLY on
// an exact match of the normalized whole message to a curated set of pure
// social pleasantries (greetings, thanks, farewells, brief reactions) that
// can never carry an instruction, answer a pending question, or confirm an
// action. Bare affirmations/negations ("yes", "ok", "sure", "do it") are
// EXCLUDED on purpose: in context they can mean "yes, save that", which
// needs history and tools the trivial verdict would skip — so those fall
// through to the classifier, which sees the conversation and decides.

// maxTrivialLen bounds the message length the gate will even inspect. The
// longest curated phrase is well under this; anything longer is not a bare
// pleasantry, so skip normalization entirely.
const maxTrivialLen = 40

// trivialPhrases is the closed set of normalized whole-message strings the
// fast-path treats as trivial. Membership rule (see file header): a pure
// social pleasantry that NEVER requires acting on prior context. Keep this
// conservative — when in doubt, leave it out and let the classifier decide.
// The eval harness measures the classifier's agreement on these, which is
// the check on whether any entry here is too aggressive.
var trivialPhrases = map[string]struct{}{
	// greetings
	"hi": {}, "hello": {}, "hey": {}, "heya": {}, "hiya": {}, "yo": {},
	"howdy": {}, "hi there": {}, "hello there": {}, "hey there": {},
	"good morning": {}, "good afternoon": {}, "good evening": {},
	"morning": {}, "gm": {},
	// thanks / appreciation
	"thanks": {}, "thank you": {}, "thankyou": {}, "thanks so much": {},
	"thank you so much": {}, "thanks a lot": {}, "thanks a ton": {},
	"many thanks": {}, "thanks again": {}, "ty": {}, "tysm": {}, "thx": {},
	"much appreciated": {}, "appreciate it": {}, "appreciated": {},
	"cheers": {},
	// farewells
	"bye": {}, "goodbye": {}, "bye bye": {}, "cya": {}, "see you": {},
	"see ya": {}, "see you later": {}, "talk later": {},
	"talk to you later": {}, "ttyl": {}, "later": {}, "take care": {},
	"good night": {}, "goodnight": {}, "gn": {},
	// brief reactions (no instruction, no confirmation)
	"lol": {}, "lmao": {}, "rofl": {}, "haha": {}, "hahaha": {}, "hehe": {},
	"cool": {}, "nice": {}, "nice one": {}, "awesome": {}, "great": {},
	"perfect": {}, "sweet": {}, "wonderful": {}, "amazing": {},
	"excellent": {}, "brilliant": {}, "well done": {}, "good bot": {},
}

// TrivialVerdict is the verdict assigned to a fast-pathed trivial turn: no
// reasoning, no retrieval, no web search — the same shape the classifier
// would ideally emit for "thanks", stamped SourceFastPath.
func TrivialVerdict() Output {
	return Output{
		Thinking:    ThinkingOff,
		MemoryDepth: MemoryNone,
		SearchDepth: SearchNone,
		Source:      SourceFastPath,
	}
}

// TrivialFastPath reports whether msg is an unambiguous trivial turn that
// can skip the classifier, returning the trivial verdict when so. Only an
// exact match of the normalized whole message to the curated set qualifies;
// any question mark or extra content forces a fall-through (ok=false).
func TrivialFastPath(msg string) (Output, bool) {
	if len(msg) > maxTrivialLen {
		return Output{}, false
	}
	n := normalizeTrivial(msg)
	if n == "" {
		return Output{}, false
	}
	if _, ok := trivialPhrases[n]; ok {
		return TrivialVerdict(), true
	}
	return Output{}, false
}

// normalizeTrivial lowercases the message, strips leading/trailing
// non-letter runes (punctuation, emoji, whitespace — so "Thanks!!" and
// "🙏 thanks" both reduce to "thanks"), and collapses internal whitespace to
// single spaces so "good   morning" matches "good morning". A message
// containing a '?' is never trivial and returns "" — a question always
// deserves the classifier.
func normalizeTrivial(msg string) string {
	if strings.ContainsRune(msg, '?') {
		return ""
	}
	lower := strings.ToLower(msg)
	// Trim any leading/trailing run of non-letters (keeps internal spaces
	// and letters; drops edge punctuation/emoji/whitespace/digits).
	trimmed := strings.TrimFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	if trimmed == "" {
		return ""
	}
	return strings.Join(strings.Fields(trimmed), " ")
}
