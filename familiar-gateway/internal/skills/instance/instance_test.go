package instance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/config"
)

func TestHasAnyField(t *testing.T) {
	if HasAnyField(config.InstanceConfig{}) {
		t.Errorf("empty config should report no fields")
	}
	if !HasAnyField(config.InstanceConfig{AdminContact: "@roo"}) {
		t.Errorf("config with one field should report true")
	}
}

func TestExecute_RendersAllFields(t *testing.T) {
	cfg := config.InstanceConfig{
		Name:         "Familiar (Host-a)",
		AdminURL:     "https://host-a.example/admin/",
		RegisterURL:  "https://host-a.example/admin/",
		AdminContact: "@roo on Slack",
		DocsURL:      "https://docs.example",
		HelpNotes:    "Ask early, ask often.",
	}
	s := New(cfg)

	res, err := s.Execute(context.Background(), "get_instance_info", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	// Content summary should surface each populated field.
	for _, want := range []string{
		"Familiar (Host-a)",
		"https://host-a.example/admin/",
		"@roo on Slack",
		"https://docs.example",
		"Ask early, ask often.",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q\nfull:\n%s", want, res.Content)
		}
	}

	// Register URL equal to admin URL should collapse (no duplicate
	// "Registration:" line when the two point at the same endpoint).
	if strings.Count(res.Content, "https://host-a.example/admin/") != 1 {
		t.Errorf("duplicate admin URL rendered; expected collapse when register_url == admin_url")
	}

	// Data payload should round-trip through JSON cleanly.
	var dto instanceInfoDTO
	if err := json.Unmarshal(res.Data, &dto); err != nil {
		t.Fatalf("Data is not valid JSON: %v", err)
	}
	if dto.AdminURL != cfg.AdminURL {
		t.Errorf("Data.admin_url = %q, want %q", dto.AdminURL, cfg.AdminURL)
	}
}

func TestExecute_EmptyConfigFallsThrough(t *testing.T) {
	s := New(config.InstanceConfig{})
	res, err := s.Execute(context.Background(), "get_instance_info", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "No deployment info") {
		t.Errorf("expected no-info fallback, got %q", res.Content)
	}
}

func TestExecute_UnknownToolErrors(t *testing.T) {
	s := New(config.InstanceConfig{Name: "x"})
	res, _ := s.Execute(context.Background(), "not_a_tool", nil)
	if res.Error == "" {
		t.Errorf("expected error for unknown tool")
	}
}

func TestExecute_OmitsEmptyFieldsInJSON(t *testing.T) {
	// Only one field populated — JSON payload must not carry the
	// others as empty strings; omitempty keeps the response compact.
	s := New(config.InstanceConfig{AdminContact: "@roo"})
	res, _ := s.Execute(context.Background(), "get_instance_info", nil)
	if strings.Contains(string(res.Data), "admin_url") {
		t.Errorf("empty admin_url should be omitted from JSON: %s", res.Data)
	}
	if !strings.Contains(string(res.Data), "admin_contact") {
		t.Errorf("admin_contact should be present in JSON: %s", res.Data)
	}
}
