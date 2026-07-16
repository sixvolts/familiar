// Package instance provides a Familiar skill that surfaces
// deployment-specific operational data: admin console URL,
// registration URL, docs, admin contact. The LLM calls
// `get_instance_info` when a user asks how to register, where the
// admin console lives, who to contact for account issues, or where
// to find documentation.
//
// Stored entirely in gateway config ([instance] block). No API
// dependencies, no network calls, no caching needed — the skill's
// entire state is frozen at startup from the loaded config.
//
// When every field of InstanceConfig is empty the skill is not
// registered at all (see main.go wiring), so the tool never appears
// in the LLM's toolbox on an unconfigured fresh deployment. Operators
// fill in `[instance]` during setup (interactive section of
// setup.sh) or by copying the block out of config.example.toml.
package instance

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/skills"
)

// Skill exposes a single tool, get_instance_info, returning the
// configured deployment metadata. All fields are optional; the tool
// omits empty ones from its response.
type Skill struct {
	cfg config.InstanceConfig
}

// New builds the skill from an InstanceConfig. The caller is
// responsible for only registering the skill when at least one field
// is set (see HasAnyField) so we don't pollute the tool registry with
// a no-op on unconfigured deployments.
func New(cfg config.InstanceConfig) *Skill {
	return &Skill{cfg: cfg}
}

// HasAnyField reports whether the config has any meaningful value. The
// caller uses this to decide whether to register the skill at all.
func HasAnyField(cfg config.InstanceConfig) bool {
	return cfg.Name != "" ||
		cfg.AdminURL != "" ||
		cfg.RegisterURL != "" ||
		cfg.AdminContact != "" ||
		cfg.DocsURL != "" ||
		cfg.HelpNotes != ""
}

func (s *Skill) Name() string { return "instance" }
func (s *Skill) Description() string {
	return "Deployment metadata (admin URL, registration URL, admin contact, docs)"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

// No parameters — the tool returns all configured fields and lets the
// LLM pick what to surface. A per-field tool surface would proliferate
// entries (one per field) with no real benefit since the payload is
// tiny.
var emptyParams = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name: "get_instance_info",
			Description: "Return this deployment's operational metadata: admin console URL, " +
				"user registration URL, admin contact, and documentation link. Call this " +
				"when a user asks how to register, where the admin console is, who to " +
				"contact for account or access issues, or where to find docs. All fields " +
				"are optional; omitted fields are not configured on this deployment.",
			Parameters: emptyParams,
		},
	}
}

// instanceInfoDTO is the JSON shape returned to the LLM. Empty fields
// are omitted via omitempty so the model doesn't waste reasoning on
// "(not configured)" placeholders.
type instanceInfoDTO struct {
	Name         string `json:"name,omitempty"`
	AdminURL     string `json:"admin_url,omitempty"`
	RegisterURL  string `json:"register_url,omitempty"`
	AdminContact string `json:"admin_contact,omitempty"`
	DocsURL      string `json:"docs_url,omitempty"`
	HelpNotes    string `json:"help_notes,omitempty"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if toolName != "get_instance_info" {
		return skills.ToolResult{Error: "unknown tool: " + toolName}, nil
	}

	info := instanceInfoDTO{
		Name:         s.cfg.Name,
		AdminURL:     s.cfg.AdminURL,
		RegisterURL:  s.cfg.RegisterURL,
		AdminContact: s.cfg.AdminContact,
		DocsURL:      s.cfg.DocsURL,
		HelpNotes:    s.cfg.HelpNotes,
	}

	// Render a compact human/LLM-readable summary alongside the JSON
	// so the model has something reason-about-able without parsing
	// the Data payload.
	var b strings.Builder
	if info.Name != "" {
		b.WriteString("Deployment: ")
		b.WriteString(info.Name)
		b.WriteString("\n")
	}
	if info.AdminURL != "" {
		b.WriteString("Admin console: ")
		b.WriteString(info.AdminURL)
		b.WriteString("\n")
	}
	if info.RegisterURL != "" && info.RegisterURL != info.AdminURL {
		b.WriteString("Registration: ")
		b.WriteString(info.RegisterURL)
		b.WriteString("\n")
	}
	if info.AdminContact != "" {
		b.WriteString("Admin contact: ")
		b.WriteString(info.AdminContact)
		b.WriteString("\n")
	}
	if info.DocsURL != "" {
		b.WriteString("Docs: ")
		b.WriteString(info.DocsURL)
		b.WriteString("\n")
	}
	if info.HelpNotes != "" {
		b.WriteString("\n")
		b.WriteString(info.HelpNotes)
		b.WriteString("\n")
	}
	content := strings.TrimRight(b.String(), "\n")
	if content == "" {
		content = "No deployment info is configured for this instance."
	}

	data, _ := json.Marshal(info)
	return skills.ToolResult{
		Content: content,
		Data:    data,
		Tokens:  len(content) / 4,
	}, nil
}
