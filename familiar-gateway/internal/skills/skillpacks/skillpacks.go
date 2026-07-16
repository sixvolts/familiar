// Package skillpacks exposes imported Agent Skills to the model
// (SKILL-PACKAGES-SPEC Phase 2). Two tools implement the standard's
// progressive-disclosure layers above the always-visible metadata:
//
//	use_skill        — activation: returns a skill's SKILL.md body
//	read_skill_file  — resources: returns one referenced file
//
// Authorization rides in on the skills.SessionContext and follows two
// paths (USER-SKILLS-SPEC Phase B):
//
//   - shard turns (ShardID set): the calling shard's bound-skill set;
//   - trusted turns (UserID set, no shard): the calling user's OWN
//     chat-enabled skills. The pipeline only advertises the tools on
//     trusted turns when the user has chat-enabled skills, but the
//     backend authorizes independently of that wiring.
//
// With neither identity every call is refused. The skill itself is
// registered natively; what it serves is imported content.
package skillpacks

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/familiar/gateway/internal/skills"
)

// Backend is the narrow surface the tools need — implemented by
// *skillpkg.Store, declared here so this package doesn't import it.
type Backend interface {
	BodyForShard(ctx context.Context, shardID, name string) (string, error)
	FileForShard(ctx context.Context, shardID, name, relPath string) ([]byte, error)
	BodyForUser(ctx context.Context, userID, name string) (string, error)
	FileForUser(ctx context.Context, userID, name, relPath string) ([]byte, error)
}

// Skill implements skills.Skill.
type Skill struct {
	resolve func() Backend // lazy — the store wires up after registration
}

// New constructs the skill with a lazy backend resolver, matching
// the wiki skill's pattern (main.go registers skills before the DB
// pool exists).
func New(resolve func() Backend) *Skill {
	return &Skill{resolve: resolve}
}

func (s *Skill) Name() string        { return "skillpacks" }
func (s *Skill) Description() string { return "Imported Agent Skills (shard-scoped)" }
func (s *Skill) Version() string     { return "1.0.0" }

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name: "use_skill",
			Description: "Activate one of the skills listed in your Skills section: returns the skill's " +
				"full instructions. Call this before attempting a task a skill covers.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "The skill name exactly as listed."}
				},
				"required": ["name"]
			}`),
		},
		{
			Name: "read_skill_file",
			Description: "Read a file that a skill's instructions reference (e.g. references/REFERENCE.md). " +
				"Paths are relative to the skill's root.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"skill": {"type": "string", "description": "The skill name."},
					"path":  {"type": "string", "description": "Relative path inside the skill, e.g. references/forms.md."}
				},
				"required": ["skill", "path"]
			}`),
		},
	}
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	backend := s.resolve()
	if backend == nil {
		return skills.ToolResult{Error: "imported skills are not configured on this gateway"}, nil
	}
	sc, _ := skills.ContextFrom(ctx)
	// Two authorization paths: shard binding (shard turns) or
	// owner + chat_enabled (trusted turns). Defense in depth: the
	// pipeline gates advertising/dispatch, but the authorization here
	// is independent of that wiring — no identity, no content.
	if sc.ShardID == "" && sc.UserID == "" {
		return skills.ToolResult{Error: "imported skills need a shard or user identity on this turn"}, nil
	}
	body := func(name string) (string, error) {
		if sc.ShardID != "" {
			return backend.BodyForShard(ctx, sc.ShardID, name)
		}
		return backend.BodyForUser(ctx, sc.UserID, name)
	}
	file := func(name, path string) ([]byte, error) {
		if sc.ShardID != "" {
			return backend.FileForShard(ctx, sc.ShardID, name, path)
		}
		return backend.FileForUser(ctx, sc.UserID, name, path)
	}

	switch toolName {
	case "use_skill":
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
			return skills.ToolResult{Error: "use_skill requires {\"name\": \"<skill>\"}"}, nil
		}
		text, err := body(p.Name)
		if err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		content := "# Skill: " + p.Name + "\n\n" + text
		return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil

	case "read_skill_file":
		var p struct {
			Skill string `json:"skill"`
			Path  string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Skill == "" || p.Path == "" {
			return skills.ToolResult{Error: "read_skill_file requires {\"skill\": ..., \"path\": ...}"}, nil
		}
		content, err := file(p.Skill, p.Path)
		if err != nil {
			return skills.ToolResult{Error: err.Error()}, nil
		}
		text := string(content)
		return skills.ToolResult{Content: text, Tokens: len(text) / 4}, nil

	default:
		return skills.ToolResult{}, fmt.Errorf("skillpacks: unknown tool %q", toolName)
	}
}

func (s *Skill) Init(cfg json.RawMessage) error { return nil }
func (s *Skill) Close() error                   { return nil }
