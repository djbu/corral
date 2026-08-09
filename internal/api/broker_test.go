package api

import (
	"sync"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/session"
)

func mustRecvFrame(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame")
		return Frame{}
	}
}

func assertNoFrame(t *testing.T, ch <-chan Frame) {
	t.Helper()
	select {
	case f := <-ch:
		t.Fatalf("expected no frame, got %+v", f)
	default:
	}
}

func TestBroker_BasicDelivery(t *testing.T) {
	b := NewBroker()
	id, ch, _ := b.Subscribe()
	defer b.Unsubscribe(id)

	ev := session.Event{Seq: 1, SessionID: "s1", TsMs: 100, Kind: session.EventSessionCreated, DataJSON: "{}"}
	b.PublishEvent(ev)

	got := mustRecvFrame(t, ch)
	if got.Resync {
		t.Fatal("expected a normal frame, got resync")
	}
	if got.Event != ev {
		t.Fatalf("got %+v, want %+v", got.Event, ev)
	}
}

func TestBroker_MultiSubscriberIsolation(t *testing.T) {
	b := NewBroker()
	id1, ch1, _ := b.Subscribe()
	defer b.Unsubscribe(id1)
	id2, ch2, _ := b.Subscribe()
	defer b.Unsubscribe(id2)

	ev := session.Event{Seq: 1, SessionID: "s1", Kind: session.EventSessionCreated, DataJSON: "{}"}
	b.PublishEvent(ev)

	f1 := mustRecvFrame(t, ch1)
	f2 := mustRecvFrame(t, ch2)
	if f1.Event != ev || f2.Event != ev {
		t.Fatalf("both subscribers should see the same event: %+v %+v", f1, f2)
	}
}

// TestBroker_SlowConsumerGetsResync exercises the stale-flag path
// (broker.go's PublishEvent): once a subscriber's 64-frame buffer is full,
// the next publish is dropped and the subscriber is marked stale instead of
// delivered. A stale subscriber then receives nothing but a single resync
// frame on the next publish that finds room for it — never a fresh event
// ahead of that resync — and reverts to normal delivery immediately after.
func TestBroker_SlowConsumerGetsResync(t *testing.T) {
	b := NewBroker()
	id, ch, _ := b.Subscribe()
	defer b.Unsubscribe(id)

	// Fill the subscriber's buffer completely (64 frames = subBufferSize).
	const bufSize = 64
	for i := int64(0); i < bufSize; i++ {
		b.PublishEvent(session.Event{Seq: i, Kind: session.EventSessionCreated, DataJSON: "{}"})
	}
	// Buffer is now full; this publish can't be delivered, so the
	// subscriber is marked stale and the event itself is dropped.
	b.PublishEvent(session.Event{Seq: bufSize, Kind: session.EventSessionCreated, DataJSON: "{}"})

	// Drain one frame to free a slot, then publish again: because the
	// subscriber is stale, PublishEvent must deliver ONLY a resync frame
	// into that freed slot — never the fresh event — and clear stale.
	first := mustRecvFrame(t, ch)
	if first.Resync {
		t.Fatal("first drained frame should be a pre-overflow normal event, not a resync")
	}
	b.PublishEvent(session.Event{Seq: bufSize + 1, Kind: session.EventSessionCreated, DataJSON: "{}"})

	// Drain the rest of the pre-overflow buffer (bufSize-1 more normal
	// frames) before reaching the resync marker queued behind them.
	for i := 0; i < bufSize-1; i++ {
		f := mustRecvFrame(t, ch)
		if f.Resync {
			t.Fatalf("unexpected resync while draining the pre-overflow buffer (frame %d)", i)
		}
	}
	f := mustRecvFrame(t, ch)
	if !f.Resync {
		t.Fatalf("expected the queued resync frame, got %+v", f)
	}
	assertNoFrame(t, ch)

	// Normal delivery resumes immediately after the resync.
	nextEv := session.Event{Seq: bufSize + 2, Kind: session.EventSessionCreated, DataJSON: "{}"}
	b.PublishEvent(nextEv)
	f = mustRecvFrame(t, ch)
	if f.Resync || f.Event != nextEv {
		t.Fatalf("expected normal frame %+v, got %+v", nextEv, f)
	}
}

func TestBroker_UnsubscribeNoPanic(t *testing.T) {
	b := NewBroker()
	id, _, _ := b.Subscribe()
	b.Unsubscribe(id)
	b.Unsubscribe(id) // idempotent-in-practice: deleting an absent map key is a no-op, not a panic

	// A publish after unsubscribe must not panic or block.
	b.PublishEvent(session.Event{Seq: 1, Kind: session.EventSessionCreated, DataJSON: "{}"})
}

func TestBroker_CloseIdempotent(t *testing.T) {
	b := NewBroker()
	_, _, done := b.Subscribe()
	b.Close()
	b.Close() // must not panic

	select {
	case <-done:
	default:
		t.Fatal("done channel should be closed")
	}

	// PublishEvent after Close is a documented no-op, not a panic.
	b.PublishEvent(session.Event{Seq: 1, Kind: session.EventSessionCreated, DataJSON: "{}"})
}

// TestBroker_ConcurrentStress exercises Subscribe/Unsubscribe/PublishEvent/
// Close concurrently under -race: correctness here means "the race
// detector finds nothing and nothing panics/deadlocks," not a specific
// delivery count (concurrent slow readers are expected to drop frames by
// design).
func TestBroker_ConcurrentStress(t *testing.T) {
	b := NewBroker()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			var seq int64
			for {
				select {
				case <-stop:
					return
				default:
					b.PublishEvent(session.Event{Seq: seq, Kind: session.EventSessionCreated, DataJSON: "{}"})
					seq++
				}
			}
		}(p)
	}

	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id, ch, _ := b.Subscribe()
				select {
				case <-ch:
				default:
				}
				b.Unsubscribe(id)
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	b.Close()
}
