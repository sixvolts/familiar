// Package datetime provides a Familiar skill that answers "what is the
// current date/time". The model has no wall-clock of its own — its
// training-cutoff notion of "now" is both stale and unknowable at
// runtime — so this gives it a first-class way to check, rather than
// guessing or asking the user.
//
// Pure Go: no config, no network, no external dependencies. Registered
// unconditionally at startup like the instance skill.
package datetime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/familiar/gateway/internal/skills"
)

// Skill exposes get_current_datetime.
type Skill struct{}

// New constructs the datetime skill. No dependencies.
func New() *Skill { return &Skill{} }

func (s *Skill) Name() string { return "datetime" }
func (s *Skill) Description() string {
	return "Current date and time, in UTC or a requested IANA timezone"
}
func (s *Skill) Version() string { return "1.0.0" }

func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

var currentDatetimeParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "timezone": {
      "type": "string",
      "description": "Optional IANA timezone name, e.g. \"America/Chicago\", \"Europe/London\", \"Asia/Tokyo\". Defaults to UTC when omitted or unrecognized."
    }
  }
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "get_current_datetime",
			Description: "Get the current date and time. Optionally pass an IANA timezone; defaults to UTC. Use this whenever the current date, time, day of week, or 'now' matters — do not guess or rely on training data.",
			Parameters:  currentDatetimeParams,
		},
	}
}

type datetimeArgs struct {
	Timezone string `json:"timezone,omitempty"`
}

func (s *Skill) Execute(_ context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	switch toolName {
	case "get_current_datetime":
		var args datetimeArgs
		if len(params) > 0 {
			if err := json.Unmarshal(params, &args); err != nil {
				return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
			}
		}
		content, data := currentDatetime(time.Now(), args.Timezone)
		return skills.ToolResult{Content: content, Data: data, Tokens: len(content) / 4}, nil
	default:
		return skills.ToolResult{}, fmt.Errorf("datetime: unknown tool %q", toolName)
	}
}

// currentDatetime renders `now` in the requested IANA timezone (UTC on
// empty/unknown). Split out from Execute so tests can pin the instant.
//
// An unrecognized zone falls back to UTC with a note rather than
// erroring: the model asked for the time, and a correct UTC answer it
// can convert beats a hard failure. UTC is always included in the
// structured data so the model has an anchor regardless of the zone.
func currentDatetime(now time.Time, tz string) (string, json.RawMessage) {
	loc := time.UTC
	tzNote := ""
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		} else {
			tzNote = fmt.Sprintf(" (unrecognized timezone %q — showing UTC instead)", tz)
		}
	}
	t := now.In(loc)
	zoneAbbrev, _ := t.Zone()

	content := fmt.Sprintf(
		"Current date and time: %s%s. ISO 8601: %s. Unix timestamp: %d.",
		t.Format("Monday, January 2, 2006 at 3:04 PM MST"),
		tzNote,
		t.Format(time.RFC3339),
		t.Unix(),
	)

	data, _ := json.Marshal(map[string]any{
		"iso8601":     t.Format(time.RFC3339),
		"unix":        t.Unix(),
		"timezone":    loc.String(),
		"zone_abbrev": zoneAbbrev,
		"weekday":     t.Weekday().String(),
		"date":        t.Format("2006-01-02"),
		"time":        t.Format("15:04:05"),
		"utc":         now.UTC().Format(time.RFC3339),
	})
	return content, data
}
