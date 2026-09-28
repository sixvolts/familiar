package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// ExtractedFact is one fact extracted from a conversation by the sidecar.
type ExtractedFact struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

// ExtractedRelationship is one (subject, predicate, object) triple
// returned alongside the facts. Used by the lightweight graph layer
// to capture structured edges that vector similarity can't express,
// e.g. {"server-1", "has_ip", "10.0.0.21"}.
type ExtractedRelationship struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}

// ExtractionResult is the combined output of the sidecar extraction
// pass: discrete facts plus the entity-relationship triples mined from
// the same turns in a single LLM call.
type ExtractionResult struct {
	Facts         []ExtractedFact
	Relationships []ExtractedRelationship
}

// Turn is a single user/assistant exchange, used for summarization input.
type Turn struct {
	Role    string
	Content string
}

// chatComplete sends a non-streaming chat completion to the sidecar llama-server
// and returns the first choice's message content. Thinking is disabled.
func (r *HTTPRouter) chatComplete(ctx context.Context, systemPrompt, userPrompt string, maxTokens int, temperature float64) (string, error) {
	content, _, err := r.chatCompleteFinish(ctx, systemPrompt, userPrompt, maxTokens, temperature)
	return content, err
}

// chatCompleteFinish is chatComplete plus the choice's finish_reason
// ("length" when max_tokens cut it off).
func (r *HTTPRouter) chatCompleteFinish(ctx context.Context, systemPrompt, userPrompt string, maxTokens int, temperature float64) (content, finish string, err error) {
	type chatMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type chatReq struct {
		Model              string         `json:"model,omitempty"`
		Messages           []chatMsg      `json:"messages"`
		MaxTokens          int            `json:"max_tokens"`
		Temperature        float64        `json:"temperature"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	}
	type chatChoice struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	type chatResp struct {
		Choices []chatChoice `json:"choices"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	msgs := []chatMsg{}
	if systemPrompt != "" {
		msgs = append(msgs, chatMsg{Role: "system", Content: systemPrompt})
	}
	msgs = append(msgs, chatMsg{Role: "user", Content: userPrompt})

	reqBody, err := json.Marshal(chatReq{
		Model:              r.model,
		Messages:           msgs,
		MaxTokens:          maxTokens,
		Temperature:        temperature,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.endpoint+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", "", fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("chat request to %s: %w", r.endpoint, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read chat response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("chat HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var cr chatResp
	if err := json.Unmarshal(respBytes, &cr); err != nil {
		return "", "", fmt.Errorf("parse chat response: %w", err)
	}
	if cr.Error != nil {
		return "", "", fmt.Errorf("chat API error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", "", fmt.Errorf("empty chat response")
	}
	return cr.Choices[0].Message.Content, cr.Choices[0].FinishReason, nil
}

const summarizeSystemPrompt = `You produce concise narrative summaries of conversations.
Focus on: topics discussed, decisions made, the user's goal, and specific technical details
that matter for follow-up questions. Do NOT include greetings, pleasantries, or
meta-commentary. Output only the summary text, no formatting, no preamble. Target 200-300
tokens.`

// Summarize produces a rolling summary of a conversation. If prevSummary is
// non-empty, the new turns are folded into it. Returns the new summary text.
func (r *HTTPRouter) Summarize(ctx context.Context, prevSummary string, turns []Turn) (string, error) {
	if len(turns) == 0 {
		return prevSummary, nil
	}

	var user strings.Builder
	if prevSummary != "" {
		user.WriteString("Previous summary:\n")
		user.WriteString(prevSummary)
		user.WriteString("\n\nNew turns to incorporate:\n")
	} else {
		user.WriteString("Summarize this conversation:\n")
	}
	user.WriteString("<conversation>\n")
	for _, t := range turns {
		fmt.Fprintf(&user, "%s: %s\n", t.Role, t.Content)
	}
	user.WriteString("</conversation>\n")
	if prevSummary != "" {
		user.WriteString("\nUpdate the summary to incorporate the new turns. Keep it under 300 tokens.")
	}

	out, finish, err := r.chatCompleteFinish(ctx, summarizeSystemPrompt, user.String(), 450, 0.3)
	if err != nil {
		return prevSummary, err
	}
	summary, err := checkSummary(out, finish)
	if err != nil {
		return prevSummary, err
	}
	return summary, nil
}

// minSummaryChars is the shortest text accepted as a rolling summary.
const minSummaryChars = 40

// checkSummary vets a summarizer reply before it replaces the rolling
// summary, which the caller then saves while dropping the turns it
// covers. Summarize returned whatever came back: an empty reply (a
// reasoning model that spent its tokens thinking) or a refusal ("I can't
// help with that.") replaced the summary, and the turns it had folded
// in were lost. A <think> block is stripped; what is left must be a
// plausible summary.
func checkSummary(out, finish string) (string, error) {
	s := strings.TrimSpace(stripThink(out))
	if len(s) < minSummaryChars {
		return "", fmt.Errorf("summarizer returned %d characters (finish=%q); keeping the previous summary", len(s), finish)
	}
	if looksLikeRefusal(s) {
		return "", fmt.Errorf("summarizer declined (%q); keeping the previous summary", truncate(s, 80))
	}
	return s, nil
}

// stripThink drops a <think>…</think> block a reasoning model inlined
// despite enable_thinking=false.
func stripThink(s string) string {
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		return s[i+len("</think>"):]
	}
	return s
}

// looksLikeRefusal reports a short reply that opens by declining.
func looksLikeRefusal(s string) bool {
	if len(s) > 400 {
		return false
	}
	l := strings.ToLower(s)
	for _, p := range []string{"i can't", "i cannot", "i can’t", "i won't", "i'm sorry", "i am sorry", "sorry,", "i'm unable", "i am unable", "as an ai"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

const extractSystemPrompt = `You extract specific, actionable facts AND entity-relationship triples from conversations.
Output a single JSON object with two keys — "facts" and "relationships" — and nothing else.
No markdown fences, no prose, no commentary.

"facts" is an array of objects with "content" and "category" fields.
Categories: "decision", "technical_fact", "preference", "project_context",
"configuration", "issue", "todo".

Only extract facts that are SPECIFIC and stand alone without needing the conversation
for context. Skip vague generalizations and meta-commentary.

A <context> block of earlier conversation may precede the <conversation>. Use it ONLY
to resolve pronouns and back-references in the <conversation> ("it", "that server",
"the other one") into concrete entity names — so "bump it to 64GB" becomes a standalone
fact about the named machine. Do NOT extract any facts or triples FROM the <context>;
extract only from the <conversation>.

"relationships" is an array of {"subject","predicate","object"} triples that capture
structured edges between entities mentioned in the turns. Use snake_case predicates
(has_ip, runs_os, located_at, owned_by, part_of, depends_on, has_gpu, ...). Emit a
triple only when it is clearly stated, not when it is merely implied.

CRITICAL RULES for subjects and objects:
- NEVER use pronouns (he, she, they, it, his, her, their). Replace with the actual
  entity name from context. If the entity cannot be determined, drop the triple.
- Use the most specific proper name available. Prefer hostnames and project
  codenames ("machine-alpha", "project-zephyr") over generic descriptors
  ("the gateway", "the big server", "the workstation").
- Normalize to lowercase ("machine-alpha" not "Machine-Alpha").
- Pick ONE name per entity and use it consistently across every triple. If a fact
  mentions "the gaming desktop (machine-alpha)", the subject is "machine-alpha"
  in every triple that refers to that machine.
- Subjects and objects are bare entity names, not full sentences.

Example:
Input: "server-1 is at 10.0.0.21 and runs Ubuntu 22.04 with 6x GPUs"
Output:
{"facts":[
  {"content":"server-1 IP address is 10.0.0.21","category":"configuration"},
  {"content":"server-1 runs Ubuntu 22.04","category":"technical_fact"},
  {"content":"server-1 has 6 GPUs","category":"technical_fact"}
],"relationships":[
  {"subject":"server-1","predicate":"has_ip","object":"10.0.0.21"},
  {"subject":"server-1","predicate":"runs_os","object":"Ubuntu 22.04"},
  {"subject":"server-1","predicate":"has_gpu","object":"6 GPUs"}
]}

If no facts or no relationships are present, return the corresponding key as an empty array.`

// ExtractFacts asks the sidecar to extract discrete facts and relationship
// triples from a set of turns in a single LLM call. Returns an empty
// result on a parse failure so the caller can proceed — extraction is
// best-effort and the pipeline is expected to handle partial output.
func (r *HTTPRouter) ExtractFacts(ctx context.Context, turns []Turn) (ExtractionResult, error) {
	return r.ExtractFactsWithContext(ctx, turns, nil)
}

// ExtractFactsWithContext is ExtractFacts with a read-only block of prior
// turns the extractor may use to resolve pronouns and back-references in the
// conversation ("bump it to 64GB") into standalone facts — without extracting
// anything FROM the context itself. context may be nil (document extraction,
// or no prior turns), which is exactly ExtractFacts.
func (r *HTTPRouter) ExtractFactsWithContext(ctx context.Context, turns, context []Turn) (ExtractionResult, error) {
	return r.extractFacts(ctx, turns, context, extractMaxTokens)
}

// extractMaxTokens is the output budget for extracting from a chat turn.
// extractLargeMaxTokens is the budget for a large document: a 12K-char
// write-up yields ~40 facts plus triples, ~1500 tokens, and at the chat
// budget of 900 the reply was cut mid-array and the whole of it lost.
const (
	extractMaxTokens      = 900
	extractLargeMaxTokens = 4096
)

// ExtractFactsLarge is ExtractFacts for a large document, with the large
// output budget.
func (r *HTTPRouter) ExtractFactsLarge(ctx context.Context, turns []Turn) (ExtractionResult, error) {
	return r.extractFacts(ctx, turns, nil, extractLargeMaxTokens)
}

func (r *HTTPRouter) extractFacts(ctx context.Context, turns, context []Turn, maxTokens int) (ExtractionResult, error) {
	if len(turns) == 0 {
		return ExtractionResult{}, nil
	}

	var user strings.Builder
	if len(context) > 0 {
		user.WriteString("<context>\n")
		for _, t := range context {
			fmt.Fprintf(&user, "%s: %s\n", t.Role, t.Content)
		}
		user.WriteString("</context>\n\n")
	}
	user.WriteString("<conversation>\n")
	for _, t := range turns {
		fmt.Fprintf(&user, "%s: %s\n", t.Role, t.Content)
	}
	user.WriteString("</conversation>\n\nExtract facts and relationships as a single JSON object.")

	raw, err := r.chatComplete(ctx, extractSystemPrompt, user.String(), maxTokens, 0.2)
	if err != nil {
		return ExtractionResult{}, err
	}
	return parseExtractionResult(raw)
}

// parseExtractionResult is the pure-function parser for the sidecar's
// facts+relationships JSON envelope. Tolerates whatever wrapping the
// model insists on adding (markdown fences, thinking tags, preambles)
// by scanning for the first balanced JSON object.
func parseExtractionResult(raw string) (ExtractionResult, error) {
	// Some llama-server builds still leak a bare JSON array when the
	// model reverts to the old extraction shape; accept that as a
	// facts-only response so an old prompt cache doesn't break us.
	if arrStart := strings.Index(raw, "["); arrStart >= 0 {
		if objStart := strings.Index(raw, "{"); objStart < 0 || arrStart < objStart {
			arrEnd := strings.LastIndex(raw, "]")
			if arrEnd > arrStart {
				var facts []ExtractedFact
				if err := json.Unmarshal([]byte(raw[arrStart:arrEnd+1]), &facts); err == nil {
					return ExtractionResult{Facts: facts}, nil
				}
			}
		}
	}

	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return ExtractionResult{}, fmt.Errorf("no JSON object in extract response: %q", truncate(raw, 200))
	}
	var env struct {
		Facts         []ExtractedFact         `json:"facts"`
		Relationships []ExtractedRelationship `json:"relationships"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &env); err != nil {
		// A reply cut off by max_tokens: keep the complete elements
		// before the cut rather than discarding every fact.
		if got, ok := salvageExtraction(raw[start:]); ok {
			log.Printf("[sidecar] extraction reply truncated; kept %d facts and %d triples before the cut", len(got.Facts), len(got.Relationships))
			return got, nil
		}
		return ExtractionResult{}, fmt.Errorf("parse extraction envelope: %w (raw: %q)", err, truncate(raw, 200))
	}
	return ExtractionResult{Facts: env.Facts, Relationships: env.Relationships}, nil
}

// salvageExtraction decodes the complete facts and relationships at the
// front of a truncated envelope, element by element, stopping at the
// first incomplete one. ok is false when nothing was recovered.
func salvageExtraction(raw string) (ExtractionResult, bool) {
	var out ExtractionResult
	dec := json.NewDecoder(strings.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return out, false
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := k.(string)
		if t, err := dec.Token(); err != nil || t != json.Delim('[') {
			break
		}
		for dec.More() {
			var err error
			switch key {
			case "facts":
				var f ExtractedFact
				if err = dec.Decode(&f); err == nil {
					out.Facts = append(out.Facts, f)
				}
			case "relationships":
				var r ExtractedRelationship
				if err = dec.Decode(&r); err == nil {
					out.Relationships = append(out.Relationships, r)
				}
			default:
				var skip json.RawMessage
				err = dec.Decode(&skip)
			}
			if err != nil {
				return out, len(out.Facts)+len(out.Relationships) > 0
			}
		}
		if _, err := dec.Token(); err != nil { // the array's ']'
			break
		}
	}
	return out, len(out.Facts)+len(out.Relationships) > 0
}

const relationshipExtractionPrompt = `You extract entity-relationship triples from a list of facts.
Output ONLY a JSON object with key "relationships" containing an array of
{"subject","predicate","object"} triples. No markdown fences. No prose.

Use snake_case predicates: has_ip, runs_os, located_at, owned_by, part_of,
depends_on, has_gpu, has_cpu, has_ram, has_storage, runs_service, connects_to,
uses_tool, built_with, prefers, has_role, works_at, lives_in, drives,
has_capacity, configured_with, member_of, etc.

Only emit triples that are CLEARLY STATED in the facts, not implied.
If no relationships can be extracted, return {"relationships": []}.

CRITICAL RULES for subjects and objects:
- NEVER use pronouns (he, she, they, it, his, her, their). Replace with the actual
  entity name from context. If the entity cannot be determined from the facts,
  drop the triple entirely — a triple with "he" as a subject is worse than no
  triple.
- Use the most specific proper name available. Prefer hostnames and project
  codenames ("machine-alpha", "project-zephyr") over generic descriptors
  ("the gateway", "the workstation"). Use the user's actual name (not "the
  user", not "the owner") if it appears anywhere in the facts.
- Normalize to lowercase ("machine-alpha" not "Machine-Alpha").
- Pick ONE name per entity and use it consistently across every triple. If the
  facts mention "the gaming desktop (machine-alpha)", the subject is
  "machine-alpha" in every triple about that machine — not "desktop", not
  "workstation", not "gaming pc".
- Subjects and objects are bare entity names, not full sentences.`

// ExtractRelationshipsFromFacts mines entity-relationship triples from
// a batch of pre-existing memory facts. Unlike ExtractFacts (which also
// pulls discrete facts from raw conversation turns), this path assumes
// the facts are already curated and only needs the structured edges.
// Facts longer than 300 characters are truncated before the prompt is
// built so a single outlier doesn't blow the context window; the
// extractor still sees the informative leading sentence.
func (r *HTTPRouter) ExtractRelationshipsFromFacts(ctx context.Context, facts []string) ([]ExtractedRelationship, error) {
	if len(facts) == 0 {
		return nil, nil
	}

	var user strings.Builder
	user.WriteString("Extract relationships from these facts:\n\n")
	for _, f := range facts {
		if len(f) > 300 {
			f = f[:300]
		}
		fmt.Fprintf(&user, "- %s\n", f)
	}

	raw, err := r.chatComplete(ctx, relationshipExtractionPrompt, user.String(), 2000, 0.1)
	if err != nil {
		return nil, err
	}
	return parseRelationshipResult(raw)
}

// parseRelationshipResult is the pure-function parser for the
// relationship-only extraction envelope. Tolerates markdown fences and
// leading preambles the same way parseExtractionResult does; returns
// an empty slice (not nil, not an error) when the envelope exists but
// carries no triples, so callers can treat "no relationships" and
// "successful extraction" as the same happy path.
func parseRelationshipResult(raw string) ([]ExtractedRelationship, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in relationship response: %q", truncate(raw, 200))
	}
	var env struct {
		Relationships []ExtractedRelationship `json:"relationships"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &env); err != nil {
		return nil, fmt.Errorf("parse relationship envelope: %w (raw: %q)", err, truncate(raw, 200))
	}
	if env.Relationships == nil {
		env.Relationships = []ExtractedRelationship{}
	}
	return env.Relationships, nil
}

const entityGroupingPrompt = `You group entity names that refer to the same real-world thing.

Input is a list of lowercase entity names from a knowledge graph about a user's
homelab, personal systems, projects, and people. Many names are variants of the
same entity — "machine-alpha" / "the desktop" / "workstation" might all be one
machine; "server-1" / "the inference server" / "the gpu box" might all be
another.

Output ONLY a JSON object of this shape — no markdown fences, no prose:
{"groups":[{"canonical":"machine-alpha","aliases":["the desktop","workstation","gaming pc"]}]}

Rules:
- The canonical name is the clearest, most specific proper name in the group.
  Prefer short, memorable names over long descriptive ones.
- Only group entities you are confident refer to the same thing. When in doubt,
  leave an entity out of every group — a missed merge is fine, a wrong merge
  corrupts the graph.
- Do NOT emit groups with only the canonical and no aliases (those are no-ops).
- Do NOT include the canonical name inside its own aliases array.
- Leave generic nouns ("server", "desktop", "machine") out of groups unless
  context makes the reference unambiguous.
- IP addresses, predicates, and bare numbers are never entities — skip them.`

// EntityGroup is one alias cluster returned by GroupEntities: a
// canonical name plus the noisy variants the caller should rewrite
// into that canonical.
type EntityGroup struct {
	Canonical string   `json:"canonical"`
	Aliases   []string `json:"aliases"`
}

// GroupEntities asks the sidecar to cluster a list of noisy entity
// names into alias groups keyed by a canonical name. Used by the
// entity-resolution backfill pass. The caller is expected to chunk
// large vocabularies before calling — the prompt scales linearly in
// the number of names and wide batches blow the context window.
func (r *HTTPRouter) GroupEntities(ctx context.Context, names []string) ([]EntityGroup, error) {
	if len(names) == 0 {
		return nil, nil
	}

	var user strings.Builder
	user.WriteString("Group these entity names:\n\n")
	for _, n := range names {
		fmt.Fprintf(&user, "- %s\n", n)
	}
	user.WriteString("\nReturn the JSON groups object now.")

	raw, err := r.chatComplete(ctx, entityGroupingPrompt, user.String(), 2000, 0.1)
	if err != nil {
		return nil, err
	}
	return parseEntityGroups(raw)
}

// parseEntityGroups is the pure-function parser for the grouping
// envelope. Tolerates markdown fences and preambles the same way
// parseRelationshipResult does.
func parseEntityGroups(raw string) ([]EntityGroup, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in grouping response: %q", truncate(raw, 200))
	}
	var env struct {
		Groups []EntityGroup `json:"groups"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &env); err != nil {
		return nil, fmt.Errorf("parse grouping envelope: %w (raw: %q)", err, truncate(raw, 200))
	}
	// Drop degenerate groups: no aliases, or aliases that contain the
	// canonical. This protects the MergeEntity call from no-op rewrites
	// and from the pathological case where the model echoes the
	// canonical back as its own alias.
	out := make([]EntityGroup, 0, len(env.Groups))
	for _, g := range env.Groups {
		g.Canonical = strings.ToLower(strings.TrimSpace(g.Canonical))
		if g.Canonical == "" {
			continue
		}
		cleaned := make([]string, 0, len(g.Aliases))
		for _, a := range g.Aliases {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" || a == g.Canonical {
				continue
			}
			cleaned = append(cleaned, a)
		}
		if len(cleaned) == 0 {
			continue
		}
		g.Aliases = cleaned
		out = append(out, g)
	}
	return out, nil
}
