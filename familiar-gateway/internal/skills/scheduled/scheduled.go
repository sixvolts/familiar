// Package scheduled exposes the user's scheduled-action run history to
// the LLM as a callable tool. It closes the recall half of
// SLACK-CONTEXT: a scheduled action (a daily digest, a paper review)
// delivers into one surface, and later — possibly on a different day or
// a different surface — the user asks "what did you send me?" or "are
// these the same papers as yesterday?". Conversation hydration carries
// the most recent delivery forward within a thread, but cross-day and
// cross-surface recall needs the durable ledger, which stores each
// run's verbatim output.
//
// The tool is read-only and strictly user-scoped: it reads the calling
// user's own actions (SessionContext.UserID) and their runs. It never
// triggers, edits, or deletes an action.
package scheduled

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/actions"
	"github.com/familiar/gateway/internal/skills"
)

// Store is the slice of the actions store this skill needs: list a
// user's actions, and read a single action's recent runs. Narrowed to
// an interface so tests can inject a fake and so the skill doesn't
// couple to the concrete store beyond these two reads.
type Store interface {
	List(ctx context.Context, ownerID string, isAdmin bool) ([]*actions.Action, error)
	ListRuns(ctx context.Context, actionID string, limit int) ([]*actions.Run, error)
}

// Skill exposes recent_scheduled_runs.
type Skill struct {
	store Store
}

// New builds the skill over an actions store. Caller registers it only
// when scheduled actions are wired (store non-nil).
func New(store Store) *Skill { return &Skill{store: store} }

func (s *Skill) Name() string { return "scheduled" }
func (s *Skill) Description() string {
	return "Read the user's scheduled-action run history (past digests, reports)"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

const (
	defaultLimit = 5
	maxLimit     = 20
	// maxOutputPerRun trims each run's stored output so a multi-run
	// recall doesn't blow the tool-result budget. The ledger itself
	// caps stored output at 16KB; this is the per-run slice the model
	// sees. Enough to compare "same as yesterday?" without dumping a
	// full report verbatim for every run.
	maxOutputPerRun = 1500
)

var paramsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "action_name": {
      "type": "string",
      "description": "Optional: only return runs of the scheduled action whose name contains this text (case-insensitive). Omit to see recent runs across all of the user's actions."
    },
    "limit": {
      "type": "integer",
      "description": "Max runs to return, newest first (default 5, max 20).",
      "minimum": 1,
      "maximum": 20
    }
  },
  "additionalProperties": false
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name: "recent_scheduled_runs",
			Description: "Return the user's recent scheduled-action runs with the text each one produced. " +
				"Call this when the user refers to something a scheduled job sent them — a daily digest, a " +
				"paper review, a report — and asks what it said, whether it differs from a previous run " +
				"(\"are these the same as yesterday?\"), or when it last ran. Each result has the action name, " +
				"when it finished, its status, and the generated output. Read-only; this never runs or changes an action.",
			Parameters: paramsSchema,
		},
	}
}

// runDTO is the per-run shape returned to the model (and to programmatic
// consumers via Data).
type runDTO struct {
	Action     string `json:"action"`
	FinishedAt string `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	Output     string `json:"output,omitempty"`
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	if toolName != "recent_scheduled_runs" {
		return skills.ToolResult{Error: "unknown tool: " + toolName}, nil
	}
	sc, _ := skills.ContextFrom(ctx)
	userID := sc.UserID
	if userID == "" {
		// No resolved identity — refuse rather than leak another
		// user's history. Matches the memory skills' posture.
		return skills.ToolResult{Error: "no user identity in context — cannot read scheduled-action history"}, nil
	}

	var in struct {
		ActionName string `json:"action_name"`
		Limit      int    `json:"limit"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return skills.ToolResult{Error: "invalid arguments: " + err.Error()}, nil
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	acts, err := s.store.List(ctx, userID, false)
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("list actions: %w", err)
	}

	nameFilter := strings.TrimSpace(strings.ToLower(in.ActionName))
	matched := make([]*actions.Action, 0, len(acts))
	for _, a := range acts {
		if nameFilter == "" || strings.Contains(strings.ToLower(a.Name), nameFilter) {
			matched = append(matched, a)
		}
	}
	if len(matched) == 0 {
		if nameFilter != "" {
			avail := actionNames(acts)
			msg := fmt.Sprintf("No scheduled action matches %q.", in.ActionName)
			if avail != "" {
				msg += " Your scheduled actions: " + avail + "."
			} else {
				msg += " You have no scheduled actions."
			}
			return skills.ToolResult{Content: msg, Tokens: len(msg) / 4}, nil
		}
		msg := "You have no scheduled actions."
		return skills.ToolResult{Content: msg, Tokens: len(msg) / 4}, nil
	}

	// Gather runs across all matched actions, then keep the newest
	// `limit` overall. Pull `limit` from each so a single very-active
	// action can't starve the merge before the sort.
	type runWithName struct {
		name string
		run  *actions.Run
	}
	var all []runWithName
	for _, a := range matched {
		runs, rerr := s.store.ListRuns(ctx, a.ID, limit)
		if rerr != nil {
			return skills.ToolResult{}, fmt.Errorf("list runs for %s: %w", a.Name, rerr)
		}
		for _, r := range runs {
			all = append(all, runWithName{name: a.Name, run: r})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		return runTime(all[i].run).After(runTime(all[j].run))
	})
	if len(all) > limit {
		all = all[:limit]
	}

	if len(all) == 0 {
		msg := "No runs recorded yet for " + describeScope(in.ActionName, matched) + "."
		return skills.ToolResult{Content: msg, Tokens: len(msg) / 4}, nil
	}

	now := time.Now()
	dtos := make([]runDTO, 0, len(all))
	var b strings.Builder
	fmt.Fprintf(&b, "Recent scheduled-action runs for %s:\n", describeScope(in.ActionName, matched))
	for _, rw := range all {
		r := rw.run
		out := strings.TrimSpace(r.Output)
		if len(out) > maxOutputPerRun {
			out = out[:maxOutputPerRun] + "\n…[truncated]"
		}
		when := ""
		if t := runTime(r); !t.IsZero() {
			when = t.UTC().Format("2006-01-02 15:04 MST") + " (" + humanizeSince(now.Sub(t)) + ")"
		}
		dtos = append(dtos, runDTO{Action: rw.name, FinishedAt: when, Status: r.Status, Output: out})

		b.WriteString("\n— ")
		b.WriteString(rw.name)
		if when != "" {
			b.WriteString(" · ")
			b.WriteString(when)
		}
		b.WriteString(" · ")
		b.WriteString(r.Status)
		b.WriteString("\n")
		if out != "" {
			b.WriteString(out)
			b.WriteString("\n")
		} else {
			b.WriteString("(no output)\n")
		}
	}

	content := strings.TrimRight(b.String(), "\n")
	data, _ := json.Marshal(dtos)
	return skills.ToolResult{
		Content: content,
		Data:    data,
		Tokens:  len(content) / 4,
	}, nil
}

// runTime returns the moment a run is best ordered by: its finish time
// when set, else its start time.
func runTime(r *actions.Run) time.Time {
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return r.StartedAt
}

func actionNames(acts []*actions.Action) string {
	names := make([]string, 0, len(acts))
	for _, a := range acts {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

func describeScope(filter string, matched []*actions.Action) string {
	if strings.TrimSpace(filter) != "" && len(matched) == 1 {
		return matched[0].Name
	}
	if strings.TrimSpace(filter) != "" {
		return fmt.Sprintf("%d actions matching %q", len(matched), filter)
	}
	return "all your scheduled actions"
}

// humanizeSince renders a coarse "how long ago" so the model can reason
// about "yesterday" / "last week" without parsing timestamps.
func humanizeSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
