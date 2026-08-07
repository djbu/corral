// Package clock provides an injectable time source. Production code must
// never call time.Now(), time.After, or time.NewTicker directly outside this
// package and its real implementation below; everything that needs the
// current time or a timer takes a Clock so tests can drive time
// deterministically via clocktest.FakeClock instead of sleeping.
package clock

import "time"

// Ticker mirrors the subset of *time.Ticker that callers need, so it can be
// faked in tests.
type Ticker interface {
	// C returns the channel on which ticks are delivered.
	C() <-chan time.Time
	// Stop turns off the ticker. It does not close C.
	Stop()
}

// Clock is the injectable time source. Real() returns the wall-clock
// implementation; tests should use clocktest.NewFake instead.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after the
	// duration d has elapsed.
	After(d time.Duration) <-chan time.Time
	// NewTicker returns a Ticker that ticks every d.
	NewTicker(d time.Duration) Ticker
}

// Real returns a Clock backed by the actual system clock and real timers.
func Real() Clock {
	return realClock{}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) NewTicker(d time.Duration) Ticker {
	return realTicker{t: time.NewTicker(d)}
}

type realTicker struct {
	t *time.Ticker
}

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
