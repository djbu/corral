package quota

import (
	"github.com/djbu/corral/internal/clock/clocktest"
	"testing"
	"time"
)

func TestControllerReservePauseAndWindowReset(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(0, 0))
	c, err := New(clk, Config{Window: time.Hour, Limit: 10, InteractiveReserve: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admit(Headless, 7).Allowed {
		t.Fatal("headless should receive non-reserved capacity")
	}
	if got := c.Admit(Headless, 1); got.Allowed || got.RetryAfter != time.Hour {
		t.Fatalf("headless decision = %+v", got)
	}
	if !c.Admit(Interactive, 3).Allowed {
		t.Fatal("interactive reserve was consumed by headless")
	}
	c.PauseUntil(clk.Now().Add(5 * time.Minute))
	if got := c.Admit(Interactive, 1); got.Allowed || got.RetryAfter != 5*time.Minute {
		t.Fatalf("pause decision = %+v", got)
	}
	clk.Advance(time.Hour)
	if !c.Admit(Headless, 7).Allowed {
		t.Fatal("window did not reset deterministically")
	}
}
