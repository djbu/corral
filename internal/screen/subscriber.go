package screen

import "sync"

// subscriberChanCap is the fixed channel capacity from design doc §5.4.
const subscriberChanCap = 256

// Subscriber is one attached client's non-blocking channel handoff (§5.4).
// Screen.Feed never blocks on a slow subscriber: on overflow it removes the
// subscriber from its broadcast set and closes Done instead of waiting.
// Every []byte sent on Ch is a copy — Feed's caller reuses its read buffer,
// so nothing referencing it may be retained past the send.
type Subscriber struct {
	Ch   chan []byte
	Done chan struct{}

	mu      sync.Mutex
	dropped bool
	closed  bool
}

// NewSubscriber allocates a Subscriber ready to pass to Screen.Attach.
func NewSubscriber() *Subscriber {
	return &Subscriber{
		Ch:   make(chan []byte, subscriberChanCap),
		Done: make(chan struct{}),
	}
}

// Dropped reports whether Screen ever overflow-dropped this subscriber
// (as opposed to a clean Detach).
func (s *Subscriber) Dropped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// markDropped records an overflow drop and closes Done. Safe to call more
// than once (e.g. concurrently with a Detach) — Done is closed exactly
// once regardless of how many of markDropped/close race.
func (s *Subscriber) markDropped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped = true
	s.closeLocked()
}

// close closes Done if it hasn't been already. Used by Screen.Detach for a
// clean detach, and by markDropped for an overflow drop — either path may
// run first, and only the first one actually closes the channel.
func (s *Subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *Subscriber) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.Done)
}
