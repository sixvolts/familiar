package admin

// Skills catalog endpoint tests (FAMILIAR-CONSOLE-SPEC Phase C).
//
//   GET /console/api/skills
//
// Any authenticated user can read this — no role gating. Returns
// structured per-skill info including each tool's parameter schema.
// An unwired catalog (no AttachSkillCatalog) returns an empty list
// (not 503) so the frontend can render "no skills" without a 404
// detour.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// fakeCatalog is the minimal SkillCatalog for the endpoint tests.
// Only Skills() is exercised by listSkillCatalog; ToolNames /
// KnownToolNames return empty so we don't accidentally hit the
// shard-creation paths.
type fakeCatalog struct {
	skills []SkillInfo
}

func (f *fakeCatalog) ToolNames() []string             { return nil }
func (f *fakeCatalog) KnownToolNames() map[string]bool { return map[string]bool{} }
func (f *fakeCatalog) Skills() []SkillInfo             { return f.skills }

func TestSkillCatalog_ReturnsStructuredSkills(t *testing.T) {
	h := &Handler{}
	h.AttachSkillCatalog(&fakeCatalog{skills: []SkillInfo{
		{
			Name:        "memory",
			Description: "Persistent semantic memory",
			Version:     "1.0.0",
			Tools: []SkillToolInfo{
				{
					Name:        "save_fact",
					Description: "Persist a fact",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}}}`),
				},
				{
					Name:        "search_memory",
					Description: "Semantic search",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			},
		},
		{
			Name:        "search",
			Description: "Web search",
			Version:     "0.9.0",
			Tools: []SkillToolInfo{
				{Name: "web_search", Description: "Search the web",
					Parameters: json.RawMessage(`{"type":"object"}`)},
			},
		},
	}})

	req := httptest.NewRequest("GET", "/console/api/skills", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listSkillCatalog(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	var body struct {
		Skills []SkillInfo `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(body.Skills))
	}
	if body.Skills[0].Name != "memory" {
		t.Errorf("first skill = %q, want memory", body.Skills[0].Name)
	}
	if len(body.Skills[0].Tools) != 2 {
		t.Errorf("memory tool count = %d, want 2", len(body.Skills[0].Tools))
	}
	// Parameter schema round-trips as raw JSON, not re-serialised noise.
	var params map[string]any
	if err := json.Unmarshal(body.Skills[0].Tools[0].Parameters, &params); err != nil {
		t.Errorf("tool params didn't round-trip as JSON: %v", err)
	}
	if params["type"] != "object" {
		t.Errorf("tool params lost shape: %+v", params)
	}
}

func TestSkillCatalog_UnwiredReturnsEmpty(t *testing.T) {
	h := &Handler{} // no AttachSkillCatalog
	req := httptest.NewRequest("GET", "/console/api/skills", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listSkillCatalog(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (unwired should not 503 here)", rec.Code)
	}
	var body struct {
		Skills []SkillInfo `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Skills == nil {
		t.Errorf("skills == nil; empty JSON array expected so frontend doesn't NPE")
	}
	if len(body.Skills) != 0 {
		t.Errorf("len = %d, want 0", len(body.Skills))
	}
}

// TestSkillCatalog_NotRoleGated documents the spec guarantee:
// "The endpoint requires authentication but is not role-gated."
// We cover both roles and expect identical 200 responses.
func TestSkillCatalog_NotRoleGated(t *testing.T) {
	h := &Handler{}
	h.AttachSkillCatalog(&fakeCatalog{skills: []SkillInfo{
		{Name: "memory", Tools: []SkillToolInfo{{Name: "save_fact"}}},
	}})

	for _, au := range []AuthUser{alisonUser(), operatorAdmin()} {
		req := httptest.NewRequest("GET", "/console/api/skills", nil).WithContext(
			ctxWithAuth(context.Background(), au))
		rec := httptest.NewRecorder()
		h.listSkillCatalog(rec, req)
		if rec.Code != 200 {
			t.Errorf("role %q got status %d, want 200", au.Role, rec.Code)
		}
	}
}

// NewSkillCatalog copies the inputs defensively so caller mutation
// after construction does not bleed into the catalog. This test
// guards against a regression where any of the three fields (names,
// known, skills) gets shared with the caller's backing slice/map.
func TestNewSkillCatalog_CopiesInputsDefensively(t *testing.T) {
	tools := []string{"save_fact"}
	known := map[string]bool{"save_fact": true}
	skills := []SkillInfo{{Name: "memory", Tools: []SkillToolInfo{{Name: "save_fact"}}}}

	cat := NewSkillCatalog(tools, known, skills)

	// Mutate callers' inputs.
	tools[0] = "injected"
	known["injected"] = true
	skills[0].Name = "injected"

	if cat.ToolNames()[0] != "save_fact" {
		t.Errorf("ToolNames leaked caller mutation: %v", cat.ToolNames())
	}
	if cat.KnownToolNames()["injected"] {
		t.Error("KnownToolNames leaked caller mutation")
	}
	if cat.Skills()[0].Name != "memory" {
		t.Errorf("Skills leaked caller mutation: %v", cat.Skills())
	}
}
