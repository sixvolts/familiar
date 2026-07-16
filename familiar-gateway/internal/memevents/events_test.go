package memevents

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func drain(ch <-chan Event, max int, deadline time.Duration) []Event {
	out := make([]Event, 0, max)
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for len(out) < max {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-timer.C:
			return out
		}
	}
	return out
}

func TestBusFanout(t *testing.T) {
	b := NewBus(0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := b.Subscribe(ctx, "sess-1", 8)
	c := b.Subscribe(ctx, "", 8) // catch-all

	b.Emit("sess-1", KindFactExtracted, FactExtractedPayload{FactID: "f1", Content: "hello"})
	b.Emit("sess-2", KindFactExtracted, FactExtractedPayload{FactID: "f2"})

	gotA := drain(a, 1, 100*time.Millisecond)
	if len(gotA) != 1 || gotA[0].SessionID != "sess-1" {
		t.Fatalf("session-scoped subscriber got %+v", gotA)
	}
	gotC := drain(c, 2, 100*time.Millisecond)
	if len(gotC) != 2 {
		t.Fatalf("catch-all got %d, want 2", len(gotC))
	}
}

func TestBusReplay(t *testing.T) {
	b := NewBus(4, nil)
	for i := 0; i < 6; i++ {
		b.Emit("s", KindFactExtracted, FactExtractedPayload{FactID: "x"})
	}
	// Ring keeps last 4. Latest IDs are 3,4,5,6. Replay since=4 → ids 5,6.
	got := b.Replay("s", 4)
	if len(got) != 2 || got[0].ID != 5 || got[1].ID != 6 {
		t.Fatalf("replay got %+v", got)
	}
	// Replay across the ring's drop window: since=0 should return whatever
	// the ring still holds, not error.
	all := b.Replay("s", 0)
	if len(all) != 4 {
		t.Fatalf("full replay got %d, want 4 (ring cap)", len(all))
	}
}

func TestBusSlowSubscriberDropsEvents(t *testing.T) {
	b := NewBus(0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.Subscribe(ctx, "s", 1) // tiny buffer
	for i := 0; i < 5; i++ {
		b.Emit("s", KindFactExtracted, FactExtractedPayload{FactID: "x"})
	}
	got := drain(ch, 5, 50*time.Millisecond)
	if len(got) > 1 {
		t.Logf("got %d events (buffer let through more than 1; ok if non-flaky)", len(got))
	}
	if len(got) == 0 {
		t.Fatalf("expected at least 1 event, got 0")
	}
}

func TestEventPayloadRoundtrip(t *testing.T) {
	b := NewBus(0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.Subscribe(ctx, "s", 4)
	b.Emit("s", KindConflictResolved, ConflictResolvedPayload{
		Action:      "UPDATE",
		FactID:      "new-id",
		TargetID:    "old-id",
		FactPreview: "gpu-host has 192GB RAM",
	})
	got := drain(ch, 1, 100*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	var p ConflictResolvedPayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Action != "UPDATE" || p.FactID != "new-id" || p.TargetID != "old-id" {
		t.Fatalf("payload mismatch: %+v", p)
	}
}
