package sidecar

import "testing"

// A decision names its candidate; the resolver matches by it.
func TestParseBatchExtractResult_CandidateIndex(t *testing.T) {
	out, err := parseBatchExtractResult(`{"decisions":[{"candidate":1,"action":"duplicate","target_id":"x"},{"action":"add"}],"relationships":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if c := out.Decisions[0].Candidate; c == nil || *c != 1 || out.Decisions[0].Action != "DUPLICATE" {
		t.Errorf("decision 0 = %+v, want candidate 1, DUPLICATE", out.Decisions[0])
	}
	if out.Decisions[1].Candidate != nil {
		t.Errorf("decision 1 has candidate %d, want none", *out.Decisions[1].Candidate)
	}
}
