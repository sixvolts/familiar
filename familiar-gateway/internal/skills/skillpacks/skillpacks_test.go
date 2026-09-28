package skillpacks

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/familiar/gateway/internal/skills"
)

// fakeBackend serves "shard:<id>/<name>" and "user:<id>/<name>" bodies
// and records which path a call took.
type fakeBackend struct{ calls []string }

func (f *fakeBackend) BodyForShard(_ context.Context, shardID, name string) (string, error) {
	f.calls = append(f.calls, "shard:"+shardID+"/"+name)
	if name == "missing" {
		return "", errors.New("skill not bound to this shard")
	}
	return "SHARD BODY", nil
}
func (f *fakeBackend) FileForShard(_ context.Context, shardID, name, rel string) ([]byte, error) {
	f.calls = append(f.calls, "shardfile:"+shardID+"/"+name+"/"+rel)
	return []byte("SHARD FILE"), nil
}
func (f *fakeBackend) BodyForUser(_ context.Context, userID, name string) (string, error) {
	f.calls = append(f.calls, "user:"+userID+"/"+name)
	return "USER BODY", nil
}
func (f *fakeBackend) FileForUser(_ context.Context, userID, name, rel string) ([]byte, error) {
	f.calls = append(f.calls, "userfile:"+userID+"/"+name+"/"+rel)
	return []byte("USER FILE"), nil
}

// A shard turn reads through the shard's bindings, a trusted turn
// through the user's chat-enabled skills, and a turn with neither
// identity gets nothing (the authorization doesn't depend on the
// pipeline having filtered the tool).
func TestExecute_AuthorizationPaths(t *testing.T) {
	be := &fakeBackend{}
	s := New(func() Backend { return be })
	run := func(sc skills.SessionContext, tool, args string) skills.ToolResult {
		t.Helper()
		res, err := s.Execute(skills.WithContext(context.Background(), sc), tool, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run(skills.SessionContext{UserID: "alice", ShardID: "kiosk"}, "use_skill", `{"name":"recipes"}`); r.Content != "# Skill: recipes\n\nSHARD BODY" {
		t.Errorf("shard use_skill = %+v", r)
	}
	if r := run(skills.SessionContext{UserID: "alice"}, "use_skill", `{"name":"recipes"}`); r.Content != "# Skill: recipes\n\nUSER BODY" {
		t.Errorf("trusted use_skill = %+v", r)
	}
	if r := run(skills.SessionContext{UserID: "alice", ShardID: "kiosk"}, "read_skill_file", `{"skill":"recipes","path":"references/a.md"}`); r.Content != "SHARD FILE" {
		t.Errorf("shard read_skill_file = %+v", r)
	}
	if r := run(skills.SessionContext{}, "use_skill", `{"name":"recipes"}`); r.Error == "" {
		t.Errorf("no identity got %+v", r)
	}
	if r := run(skills.SessionContext{ShardID: "kiosk"}, "use_skill", `{"name":"missing"}`); r.Error == "" {
		t.Errorf("unbound skill got %+v", r)
	}
	want := []string{"shard:kiosk/recipes", "user:alice/recipes", "shardfile:kiosk/recipes/references/a.md", "shard:kiosk/missing"}
	if len(be.calls) != len(want) {
		t.Fatalf("backend calls = %v, want %v", be.calls, want)
	}
	for i := range want {
		if be.calls[i] != want[i] {
			t.Errorf("call %d = %s, want %s", i, be.calls[i], want[i])
		}
	}
	if r, _ := New(func() Backend { return nil }).Execute(context.Background(), "use_skill", json.RawMessage(`{"name":"x"}`)); r.Error == "" {
		t.Error("an unconfigured backend served something")
	}
}
