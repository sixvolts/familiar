package sidecar

// CHAT-REARCH §"Memory Write Pipeline" — single-call conflict
// resolution + relationship extraction on the medium slot. Replaces
// the per-fact ClassifyConflict loop. See the spec for why these
// two passes are batched: same slot, same context, similar prompt,
// and conflict classification benefits from seeing relationship
// context.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BatchCandidate pairs a freshly-extracted fact with its nearest
// existing neighbors (looked up via pgvector before the batch call).
// The medium-slot model uses the neighbors to decide ADD vs UPDATE
// vs DUPLICATE.
type BatchCandidate struct {
	Fact      ExtractedFact
	Neighbors []FactNeighbor
}

// FactNeighbor is one nearest-neighbor lookup result. ID is the
// existing memory's UUID — required for the UPDATE action so the
// gateway can chain Supersedes correctly.
type FactNeighbor struct {
	ID         string
	Content    string
	Similarity float64
}

// BatchExtractInput is the full payload for one BatchClassifyAndRelate
// call. Spec inputs:
//   - candidates from step 1 (fact extraction)
//   - nearest-neighbor existing facts per candidate
//   - current turn text (user + assistant)
//   - recently extracted facts (this session)
//   - raw retrieved relationships used in the assembled context
type BatchExtractInput struct {
	UserMessage      string
	AssistantMessage string
	Candidates       []BatchCandidate
	RecentFacts      []string
	RetrievedRels    []ExtractedRelationship
}

// BatchDecision is the per-candidate action emitted by the model.
// Action ∈ {"ADD", "UPDATE", "DUPLICATE"}. TargetID is the existing
// memory ID this candidate updates or duplicates — empty for ADD.
type BatchDecision struct {
	Action   string `json:"action"`
	TargetID string `json:"target_id,omitempty"`
}

// BatchExtractResult is the parsed output of one batched call.
// Decisions[i] applies 1:1 to Input.Candidates[i]; the model is
// instructed to preserve order. Callers must defensively check
// len(Decisions) before indexing — a misbehaving model can return
// fewer.
type BatchExtractResult struct {
	Decisions     []BatchDecision         `json:"decisions"`
	Relationships []ExtractedRelationship `json:"relationships"`
}

const batchExtractSystemPrompt = `You resolve fact conflicts AND extract relationship triples in a single pass.

Output a single JSON object with two keys — "decisions" and "relationships" — and nothing else.
No markdown fences. No prose. No commentary.

INPUT SHAPE:
- Current turn text (user + assistant messages).
- Candidate facts extracted from this turn, each paired with its nearest existing neighbors (with ids).
- Recently extracted facts from earlier in this session (no ids — context only).
- Raw retrieved relationships that were available to the assistant when it composed its response.

DECISIONS — one per candidate, IN INPUT ORDER:
{"action": "ADD" | "UPDATE" | "DUPLICATE", "target_id": "<existing-id>"}

  ADD       — candidate is new information. Neighbors don't cover it. Omit target_id.
  UPDATE    — candidate refines or replaces a neighbor (same subject + predicate, new value;
              corrected typo; clarified detail). Set target_id to the neighbor being superseded.
  DUPLICATE — candidate restates a neighbor with no new information. Set target_id.

If candidates list is empty, return {"decisions": [], ...}.

RELATIONSHIPS — array of {"subject", "predicate", "object"} triples drawn from the current turn.
Use snake_case predicates (has_ip, runs_os, located_at, owned_by, part_of, depends_on,
has_gpu, runs_service, prefers, works_at, ...).

CRITICAL RULES for subjects/objects:
- NEVER use pronouns (he/she/they/it). Replace with the actual entity name. If the entity
  cannot be determined, drop the triple.
- Prefer specific proper names (hostnames, codenames) over generic descriptors.
- Lowercase. Pick one name per entity and use it consistently.
- Bare entity names, not sentences.
- Only emit triples that are CLEARLY STATED in the current turn, not implied.

If no relationships are present, return "relationships": [].

EXAMPLE:
Current turn:
  user: actually gpu-host has 192GB ram, not 256GB
  assistant: Got it — updating that.
Candidates:
  [0] {"content":"gpu-host has 192GB RAM","category":"technical_fact"}
      neighbors: [{"id":"abc-123","content":"gpu-host has 256GB of RAM","similarity":0.91}]
Output:
{"decisions":[{"action":"UPDATE","target_id":"abc-123"}],
 "relationships":[{"subject":"gpu-host","predicate":"has_ram","object":"192GB"}]}`

// BatchClassifyAndRelate runs the medium-slot conflict +
// relationship pass for one turn's worth of candidates. Returns an
// empty result on parse failure so callers can degrade gracefully —
// the spec accepts occasional duplicates over a blocked write
// pipeline.
func (r *HTTPRouter) BatchClassifyAndRelate(ctx context.Context, in BatchExtractInput) (BatchExtractResult, error) {
	user := buildBatchExtractPrompt(in)
	raw, err := r.chatComplete(ctx, batchExtractSystemPrompt, user, 1500, 0.2)
	if err != nil {
		return BatchExtractResult{}, err
	}
	return parseBatchExtractResult(raw)
}

// buildBatchExtractPrompt formats the structured input into a single
// user-role prompt. Kept separate for testability.
func buildBatchExtractPrompt(in BatchExtractInput) string {
	var b strings.Builder

	b.WriteString("<current_turn>\n")
	if in.UserMessage != "" {
		fmt.Fprintf(&b, "user: %s\n", in.UserMessage)
	}
	if in.AssistantMessage != "" {
		fmt.Fprintf(&b, "assistant: %s\n", in.AssistantMessage)
	}
	b.WriteString("</current_turn>\n\n")

	b.WriteString("<candidates>\n")
	if len(in.Candidates) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i, c := range in.Candidates {
			fmt.Fprintf(&b, "[%d] content=%q category=%q\n", i, c.Fact.Content, c.Fact.Category)
			if len(c.Neighbors) == 0 {
				b.WriteString("    neighbors: (none)\n")
				continue
			}
			b.WriteString("    neighbors:\n")
			for _, n := range c.Neighbors {
				fmt.Fprintf(&b, "      - id=%q content=%q similarity=%.2f\n", n.ID, n.Content, n.Similarity)
			}
		}
	}
	b.WriteString("</candidates>\n\n")

	if len(in.RecentFacts) > 0 {
		b.WriteString("<recent_session_facts>\n")
		for _, f := range in.RecentFacts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("</recent_session_facts>\n\n")
	}

	if len(in.RetrievedRels) > 0 {
		b.WriteString("<retrieved_relationships>\n")
		for _, r := range in.RetrievedRels {
			fmt.Fprintf(&b, "- (%s, %s, %s)\n", r.Subject, r.Predicate, r.Object)
		}
		b.WriteString("</retrieved_relationships>\n\n")
	}

	b.WriteString("Emit decisions (one per candidate, in order) and relationships as a single JSON object.")
	return b.String()
}

// parseBatchExtractResult tolerates the same wrappings as the other
// sidecar parsers (markdown fences, thinking tags, preambles) by
// scanning for the first balanced JSON object.
func parseBatchExtractResult(raw string) (BatchExtractResult, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return BatchExtractResult{}, fmt.Errorf("no JSON object in batch extract response: %q", truncate(raw, 200))
	}
	var out BatchExtractResult
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return BatchExtractResult{}, fmt.Errorf("parse batch envelope: %w (raw: %q)", err, truncate(raw, 200))
	}
	for i := range out.Decisions {
		out.Decisions[i].Action = strings.ToUpper(strings.TrimSpace(out.Decisions[i].Action))
	}
	return out, nil
}
