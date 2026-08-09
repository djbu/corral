// Package clocktest provides a deterministic clock.Clock implementation for
// tests. Time only moves when Advance is called explicitly; nothing in this
// package ever reads the wall clock or blocks on a real timer, so tests never
// need time.Sleep to synchronize against it.
package clocktest

import (
	"sync"
	"time"

	"github.com/djbu/corral/internal/clock"
)

// FakeClock is a deterministic clock.Clock for tests. All methods are safe
// for concurrent use.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
	tickers []*fakeTicker
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
	fired    bool
}

// NewFake returns a FakeClock whose initial time is start.
func NewFake(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// Now returns the fake clock's current time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel (buffered, capacity 1) that receives the fake
// clock's current time once a call to Advance moves now to or past the
// deadline now()+d. It fires only as a direct result of Advance, never on
// its own; d <= 0 fires immediately (matching time.After).
func (f *FakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	w := &fakeWaiter{
		deadline: f.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	if d <= 0 {
		w.fired = true
		w.ch <- f.now
		return w.ch
	}
	f.waiters = append(f.waiters, w)
	return w.ch
}

// NewTicker returns a clock.Ticker that delivers a tick (buffered, capacity
// 1 — a slow receiver drops intervening ticks, matching time.Ticker) each
// time Advance crosses a multiple of d. It panics if d <= 0, matching
// time.NewTicker's behavior for a real ticker.
func (f *FakeClock) NewTicker(d time.Duration) clock.Ticker {
	if d <= 0 {
		panic("clocktest: non-positive ticker duration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	t := &fakeTicker{
		parent:   f,
		interval: d,
		next:     f.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	f.tickers = append(f.tickers, t)
	return t
}

// Advance moves the fake clock's time forward by d, then fires (in
// unspecified order) every After channel and Ticker tick whose deadline is
// now due. It never blocks: all deliveries are non-blocking sends on
// buffered, capacity-1 channels, so a test that never reads a channel cannot
// wedge Advance.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)

	live := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.fired && !w.deadline.After(f.now) {
			w.fired = true
			select {
			case w.ch <- f.now:
			default:
			}
			continue
		}
		live = append(live, w)
	}
	f.waiters = live

	for _, t := range f.tickers {
		if t.stopped {
			continue
		}
		for !t.next.After(f.now) {
			select {
			case t.ch <- t.next:
			default:
			}
			t.next = t.next.Add(t.interval)
		}
	}
}

type fakeTicker struct {
	parent   *FakeClock
	interval time.Duration
	next     time.Time
	ch       chan time.Time
	stopped  bool
}

func (t *fakeTicker) C() <-chan time.Time { return t.ch }

// Stop marks the ticker as stopped; Advance will no longer deliver ticks to
// it. It does not close the channel.
func (t *fakeTicker) Stop() {
	t.parent.mu.Lock()
	defer t.parent.mu.Unlock()
	t.stopped = true
}
