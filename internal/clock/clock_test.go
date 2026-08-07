package clock

import (
	"testing"
	"time"
)

func TestReal_NowAdvancesWithWallClock(t *testing.T) {
	c := Real()
	t1 := c.Now()
	time.Sleep(time.Millisecond)
	t2 := c.Now()
	if !t2.After(t1) {
		t.Fatalf("Real().Now() did not advance: t1=%v t2=%v", t1, t2)
	}
}

func TestReal_AfterFires(t *testing.T) {
	c := Real()
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("Real().After did not fire within 1s")
	}
}

func TestReal_NewTickerTicks(t *testing.T) {
	c := Real()
	tk := c.NewTicker(time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(time.Second):
		t.Fatal("Real().NewTicker did not tick within 1s")
	}
}
