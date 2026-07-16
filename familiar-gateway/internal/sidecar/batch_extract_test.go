package sidecar

import (
	"strings"
	"testing"
)

func TestParseBatchExtractResult(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantDecisions int
		wantRels      int
		firstAction   string
		firstTargetID string
		wantErr       bool
	}{
		{
			name: "clean JSON",
			raw: `{"decisions":[{"action":"UPDATE","target_id":"abc-123"},{"action":"ADD"}],` +
				`"relationships":[{"subject":"gpu-host","predicate":"has_ram","object":"192GB"}]}`,
			wantDecisions: 2,
			wantRels:      1,
			firstAction:   "UPDATE",
			firstTargetID: "abc-123",
		},
		{
			name:          "lowercase action gets normalized",
			raw:           `{"decisions":[{"action":"duplicate","target_id":"x"}],"relationships":[]}`,
			wantDecisions: 1,
			firstAction:   "DUPLICATE",
			firstTargetID: "x",
		},
		{
			name:          "JSON wrapped in thinking tags",
			raw:           "<think>analysis...</think>\n{\"decisions\":[{\"action\":\"ADD\"}],\"relationships\":[]}",
			wantDecisions: 1,
			firstAction:   "ADD",
		},
		{
			name:          "markdown fence wrapping",
			raw:           "```json\n{\"decisions\":[],\"relationships\":[]}\n```",
			wantDecisions: 0,
			wantRels:      0,
		},
		{
			name:    "no JSON",
			raw:     "I'm not sure how to classify these.",
			wantErr: true,
		},
		{
			name:    "malformed JSON",
			raw:     "{decisions: [...",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBatchExtractResult(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got.Decisions) != tt.wantDecisions {
				t.Errorf("decisions: got %d, want %d", len(got.Decisions), tt.wantDecisions)
			}
			if len(got.Relationships) != tt.wantRels {
				t.Errorf("relationships: got %d, want %d", len(got.Relationships), tt.wantRels)
			}
			if tt.firstAction != "" && len(got.Decisions) > 0 {
				if got.Decisions[0].Action != tt.firstAction {
					t.Errorf("first action: got %q, want %q", got.Decisions[0].Action, tt.firstAction)
				}
				if got.Decisions[0].TargetID != tt.firstTargetID {
					t.Errorf("first target_id: got %q, want %q", got.Decisions[0].TargetID, tt.firstTargetID)
				}
			}
		})
	}
}

func TestBuildBatchExtractPrompt(t *testing.T) {
	in := BatchExtractInput{
		UserMessage:      "actually gpu-host has 192GB ram",
		AssistantMessage: "Got it.",
		Candidates: []BatchCandidate{
			{
				Fact: ExtractedFact{Content: "gpu-host has 192GB RAM", Category: "technical_fact"},
				Neighbors: []FactNeighbor{
					{ID: "abc-123", Content: "gpu-host has 256GB of RAM", Similarity: 0.91},
				},
			},
		},
		RecentFacts: []string{"gpu-host runs Ubuntu 24.04"},
		RetrievedRels: []ExtractedRelationship{
			{Subject: "gpu-host", Predicate: "has_gpu", Object: "6x GPUs"},
		},
	}
	prompt := buildBatchExtractPrompt(in)
	for _, want := range []string{
		"<current_turn>",
		"actually gpu-host has 192GB ram",
		"gpu-host has 192GB RAM",
		"abc-123",
		"<recent_session_facts>",
		"<retrieved_relationships>",
		"gpu-host, has_gpu, 6x GPUs",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nfull prompt:\n%s", want, prompt)
		}
	}
}
