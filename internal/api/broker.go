package api

import (
	"sync"

	"github.com/danielbecerra/corral/internal/session"
)

// Frame is what a subscriber's channel carries. A normal frame carries a
// persisted Event. A resync frame (Resync=true, zero Event) tells the
// client it missed >=1 event (its channel filled) and must refetch full
// state — the broker is best-effort, not a guaranteed-delivery log.
//
// DEVIATION from m5.md §9 (which specs `chan Event`): we carry a Frame, not
// a bare Event, so the resync signal never has to be encoded as an
// EventKind — an EventKind resync would risk someone persisting it via
// AppendEvent later.
//
// Ordering: PublishEvent runs after the store's insert returns, so two
// concurrent AppendEvent calls can deliver frames out of seq order. That's
// acceptable — the stream is best-effort-ordered; the dashboard sorts by
// seq. Likewise, a subscriber whose buffer overflowed can still receive an
// event that was already queued ahead of the resync marker before the
// overflow was noticed; the resync guarantee is only that no *future*
// publish delivers a fresh event to a still-stale subscriber ahead of its
// owed resync (see PublishEvent).
type Frame struct {
	Event  session.Event
	Resync bool
}

type sub struct {
	ch    chan Frame
	stale bool // dropped >=1 event; owes the client exactly one resync frame
}

// Broker fans every persisted event out to live SSE subscribers. It is
// corral's first server-push channel (m5.md §9). Drop-never-block: a
// subscriber that can't keep up drops events and is told to resync, but a
// slow subscriber can never slow the persist path or another subscriber.
type Broker struct {
	mu     sync.Mutex
	subs   map[int]*sub
	nextID int
	closed bool
	done   chan struct{}
}

// NewBroker returns a ready-to-use Broker with no subscribers.
func NewBroker() *Broker {
	return &Broker{subs: make(map[int]*sub), done: make(chan struct{})}
}

// Subscribe registers a new subscriber and returns its id, its frame
// channel, and the broker-wide done channel (closed by Close at shutdown).
// The handler selects on done so a live stream exits promptly when the
// daemon shuts down.
func (b *Broker) Subscribe() (id int, ch <-chan Frame, done <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id = b.nextID
	b.nextID++
	s := &sub{ch: make(chan Frame, 64)} // buffer: absorbs a burst; overflow -> resync
	b.subs[id] = s
	return id, s.ch, b.done
}

// Unsubscribe drops the subscriber. It deletes from the map under the lock
// and does NOT close the channel — the only reader is the handler that's
// already returning, and closing would invite a send-on-closed panic the
// instant the discipline slips.
func (b *Broker) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}

// PublishEvent implements store.EventPublisher. Non-blocking fan-out under
// the lock. Resync invariant: a stale subscriber is owed EXACTLY ONE
// resync frame, and never receives a post-drop event before that resync.
// So for a stale sub we try to send ONLY the resync (clearing stale on
// success) and skip this event entirely — the client will pick it up when
// it refetches.
func (b *Broker) PublishEvent(ev session.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for _, s := range b.subs {
		if s.stale {
			select {
			case s.ch <- Frame{Resync: true}:
				s.stale = false
			default:
				// resync still couldn't be delivered; stays stale, one owed
			}
			continue
		}
		select {
		case s.ch <- Frame{Event: ev}:
		default:
			s.stale = true // dropped; owes a resync on a future publish
		}
	}
}

// Close signals every live handler to exit (by closing done) and makes
// further PublishEvent calls no-ops. It NEVER closes per-subscriber
// channels. Idempotent.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.done)
}
