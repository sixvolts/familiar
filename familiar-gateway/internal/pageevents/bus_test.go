package pageevents

// The bus is the fanout point the notes/wiki cross-device sync rides
// on (SSE handler subscribes, every page mutation publishes). These
// pin its three load-bearing promises: every live subscriber sees
// every event in order, cancellation detaches a subscriber and closes
// its channel, and a slow subscriber drops events instead of blocking
// the publisher.

import (
	"context"
	"testing"
	"time"
)

func recvOne(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while expecting an event")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return Event{} // unreachable
}

func TestPublishFansOutToAllSubscribers(t *testing.T) {
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := b.Subscribe(ctx, 4)
	c := b.Subscribe(ctx, 4)

	b.Publish(KindPageSaved, "book-1", "page-1", PageSavedPayload{BookSlug: "b", PageSlug: "p"})

	for name, ch := range map[string]<-chan Event{"a": a, "c": c} {
		e := recvOne(t, ch)
		if e.Kind != KindPageSaved || e.BookID != "book-1" || e.PageID != "page-1" {
			t.Errorf("subscriber %s got %+v", name, e)
		}
		if len(e.Payload) == 0 {
			t.Errorf("subscriber %s: payload missing", name)
		}
	}
}

func TestEventIDsAreMonotonic(t *testing.T) {
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, 8)

	for i := 0; i < 5; i++ {
		b.Publish(KindPageSaved, "book", "page", nil)
	}
	var last uint64
	for i := 0; i < 5; i++ {
		e := recvOne(t, ch)
		if e.ID <= last {
			t.Fatalf("event %d: id %d not greater than previous %d", i, e.ID, last)
		}
		last = e.ID
	}
}

func TestCancelDetachesAndClosesChannel(t *testing.T) {
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx, 4)

	cancel()
	// The unsubscribe goroutine closes the channel; wait for it.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// Closed — publishing now must not panic (send on
				// closed channel) because the sub was removed first.
				b.Publish(KindPageDeleted, "book", "page", nil)
				return
			}
		case <-deadline:
			t.Fatal("channel never closed after cancel")
		}
	}
}

func TestSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slow := b.Subscribe(ctx, 1)  // fills after one event
	fast := b.Subscribe(ctx, 16) // must still see everything

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Publish(KindPageSaved, "book", "page", nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on a slow subscriber")
	}

	// Fast subscriber got all 10; slow got exactly its buffer (1).
	for i := 0; i < 10; i++ {
		recvOne(t, fast)
	}
	if got := len(slow); got != 1 {
		t.Errorf("slow subscriber buffered %d events, want 1", got)
	}
}

func TestNilBusPublishIsANoOp(t *testing.T) {
	var b *Bus
	b.Publish(KindPageSaved, "book", "page", nil) // must not panic
}

func TestUnmarshalablePayloadStillDelivers(t *testing.T) {
	b := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, 1)

	// channels can't JSON-marshal — the bus must fall back to "{}"
	// rather than swallowing the event.
	b.Publish(KindPageSaved, "book", "page", make(chan int))
	e := recvOne(t, ch)
	if string(e.Payload) != "{}" {
		t.Errorf("payload fallback = %q, want {}", e.Payload)
	}
}
