package clocktest

import (
	"testing"
	"time"
)

func TestFakeClock_NowIsDeterministic(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := NewFake(start)

	if got := fc.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}

	fc.Advance(5 * time.Second)
	want := start.Add(5 * time.Second)
	if got := fc.Now(); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
}

func TestFakeClock_AfterFiresOnlyOnAdvance(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := NewFake(start)

	ch := fc.After(10 * time.Second)

	select {
	case <-ch:
		t.Fatal("After channel fired before any Advance")
	default:
		// expected: After never delivers without an Advance call. This check
		// is deterministic (no real-time wait needed): delivery only ever
		// happens synchronously inside Advance, which we have not called yet.
	}

	fc.Advance(5 * time.Second)
	select {
	case <-ch:
		t.Fatal("After channel fired before deadline reached")
	default:
	}

	fc.Advance(5 * time.Second)
	select {
	case got := <-ch:
		want := start.Add(10 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("After delivered %v, want %v", got, want)
		}
	default:
		t.Fatal("After channel did not fire once deadline was reached")
	}
}

func TestFakeClock_AfterNonPositiveFiresImmediately(t *testing.T) {
	fc := NewFake(time.Now())
	ch := fc.After(0)
	select {
	case <-ch:
	default:
		t.Fatal("After(0) did not fire immediately")
	}
}

func TestFakeClock_AfterOvershootStillFires(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := NewFake(start)
	ch := fc.After(10 * time.Second)

	// Advancing past (not exactly to) the deadline must still fire.
	fc.Advance(30 * time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("After channel did not fire when Advance overshot the deadline")
	}
}

func TestFakeClock_TickerTicksOnlyOnAdvance(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := NewFake(start)
	ticker := fc.NewTicker(10 * time.Second)
	defer ticker.Stop()

	select {
	case <-ticker.C():
		t.Fatal("ticker fired before any Advance")
	default:
	}

	fc.Advance(10 * time.Second)
	select {
	case got := <-ticker.C():
		want := start.Add(10 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("tick = %v, want %v", got, want)
		}
	default:
		t.Fatal("ticker did not fire at its interval")
	}

	// Advancing by three intervals worth in one call must still only leave
	// one buffered tick (capacity 1), matching time.Ticker's drop-on-slow-
	// receiver behavior, and must not block Advance.
	fc.Advance(30 * time.Second)
	select {
	case <-ticker.C():
	default:
		t.Fatal("ticker did not fire after a multi-interval Advance")
	}
	select {
	case <-ticker.C():
		t.Fatal("ticker channel should be drained to a single buffered tick")
	default:
	}
}

func TestFakeClock_TickerStopStopsDelivery(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := NewFake(start)
	ticker := fc.NewTicker(5 * time.Second)
	ticker.Stop()

	fc.Advance(20 * time.Second)
	select {
	case <-ticker.C():
		t.Fatal("stopped ticker delivered a tick")
	default:
	}
}

func TestFakeClock_NewTickerPanicsOnNonPositiveDuration(t *testing.T) {
	fc := NewFake(time.Now())
	defer func() {
		if recover() == nil {
			t.Fatal("NewTicker(0) did not panic")
		}
	}()
	fc.NewTicker(0)
}
