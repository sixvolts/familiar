package datetime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A fixed instant: 2026-07-05T18:30:00Z (a Sunday).
var fixed = time.Date(2026, 7, 5, 18, 30, 0, 0, time.UTC)

func decodeData(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("data decode: %v", err)
	}
	return m
}

func TestCurrentDatetime_UTCDefault(t *testing.T) {
	content, raw := currentDatetime(fixed, "")
	if !strings.Contains(content, "2026") || !strings.Contains(content, "Sunday") {
		t.Errorf("content missing date/weekday: %q", content)
	}
	if !strings.Contains(content, "2026-07-05T18:30:00Z") {
		t.Errorf("content missing ISO8601 UTC: %q", content)
	}
	d := decodeData(t, raw)
	if d["timezone"] != "UTC" {
		t.Errorf("timezone = %v, want UTC", d["timezone"])
	}
	if d["weekday"] != "Sunday" {
		t.Errorf("weekday = %v, want Sunday", d["weekday"])
	}
	if d["date"] != "2026-07-05" {
		t.Errorf("date = %v, want 2026-07-05", d["date"])
	}
	if int64(d["unix"].(float64)) != fixed.Unix() {
		t.Errorf("unix = %v, want %d", d["unix"], fixed.Unix())
	}
}

func TestCurrentDatetime_TimezoneApplied(t *testing.T) {
	// 18:30 UTC is 13:30 the same day in America/Chicago (CDT, -05:00).
	content, raw := currentDatetime(fixed, "America/Chicago")
	d := decodeData(t, raw)
	if d["timezone"] != "America/Chicago" {
		t.Errorf("timezone = %v, want America/Chicago", d["timezone"])
	}
	if d["time"] != "13:30:00" {
		t.Errorf("local time = %v, want 13:30:00", d["time"])
	}
	if !strings.Contains(content, "1:30 PM") {
		t.Errorf("content missing localized clock: %q", content)
	}
	// UTC anchor always present regardless of the requested zone.
	if d["utc"] != "2026-07-05T18:30:00Z" {
		t.Errorf("utc anchor = %v", d["utc"])
	}
}

func TestCurrentDatetime_UnknownZoneFallsBackToUTC(t *testing.T) {
	content, raw := currentDatetime(fixed, "Mars/Olympus_Mons")
	if !strings.Contains(content, "unrecognized timezone") {
		t.Errorf("expected fallback note, got: %q", content)
	}
	d := decodeData(t, raw)
	if d["timezone"] != "UTC" {
		t.Errorf("fallback timezone = %v, want UTC", d["timezone"])
	}
}

func TestExecute_ToolRouting(t *testing.T) {
	s := New()
	res, err := s.Execute(context.Background(), "get_current_datetime", json.RawMessage(`{"timezone":"UTC"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != "" || res.Content == "" {
		t.Errorf("unexpected result: %+v", res)
	}

	if _, err := s.Execute(context.Background(), "nope", nil); err == nil {
		t.Error("unknown tool should return an error")
	}

	bad, err := s.Execute(context.Background(), "get_current_datetime", json.RawMessage(`{not json`))
	if err != nil {
		t.Fatalf("malformed params should be a soft error, got hard error: %v", err)
	}
	if bad.Error == "" {
		t.Error("malformed params should set ToolResult.Error")
	}
}

func TestTools_ExposesSingleTool(t *testing.T) {
	tools := New().Tools()
	if len(tools) != 1 || tools[0].Name != "get_current_datetime" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	// Parameters must be valid JSON so the registry/LLM can consume it.
	var schema map[string]any
	if err := json.Unmarshal(tools[0].Parameters, &schema); err != nil {
		t.Errorf("tool params not valid JSON: %v", err)
	}
}
