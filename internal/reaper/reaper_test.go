package reaper

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
)

// checkpointCall records one CheckpointIdle invocation for assertion.
type checkpointCall struct {
	id      string
	idleFor time.Duration
}

// fakeSupervisor is an in-memory Supervisor double. Safe for concurrent use
// (TestReaper_StartClose_Deadlock drives it from a real background
// goroutine), guarded by mu.
type fakeSupervisor struct {
	mu       sync.Mutex
	liveIDs  []string
	attached map[string]bool
	calls    []checkpointCall
	err      error
}

func (f *fakeSupervisor) LiveIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.liveIDs))
	copy(out, f.liveIDs)
	return out
}

func (f *fakeSupervisor) Attached(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached[id]
}

func (f *fakeSupervisor) CheckpointIdle(ctx context.Context, id string, idleFor time.Duration) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, checkpointCall{id: id, idleFor: idleFor})
	return &session.Session{ID: id}, nil
}

func (f *fakeSupervisor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSupervisor) lastCall() checkpointCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

// fakeStore is an in-memory Store double keyed by session ID.
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[string]*session.Session{}}
}

func (f *fakeStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[id]
	if !ok {
		return nil, fmt.Errorf("fakeStore: no such session: %s", id)
	}
	return sess, nil
}

// --- reapOnce policy: called directly, no ticker/goroutine involved -----

func TestReaper_ReapOnce_IdleUnattachedPastTimeout_Reaps(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)

	st := newFakeStore()
	st.sessions["s1"] = &session.Session{
		ID:             "s1",
		AgentState:     session.AgentIdle,
		LastActivityMs: start.UnixMilli(),
	}
	sup := &fakeSupervisor{liveIDs: []string{"s1"}}

	r := New(sup, st, clk, nil, 10*time.Minute)
	clk.Advance(11 * time.Minute)
	r.reapOnce(context.Background())

	if got := sup.callCount(); got != 1 {
		t.Fatalf("CheckpointIdle called %d times, want 1", got)
	}
	call := sup.lastCall()
	if call.id != "s1" {
		t.Fatalf("CheckpointIdle id = %q, want s1", call.id)
	}
	if call.idleFor != 11*time.Minute {
		t.Fatalf("CheckpointIdle idleFor = %v, want %v", call.idleFor, 11*time.Minute)
	}
}

func TestReaper_ReapOnce_WithinTimeout_NotReaped(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)

	st := newFakeStore()
	st.sessions["s1"] = &session.Session{
		ID:             "s1",
		AgentState:     session.AgentIdle,
		LastActivityMs: start.UnixMilli(),
	}
	sup := &fakeSupervisor{liveIDs: []string{"s1"}}

	r := New(sup, st, clk, nil, 10*time.Minute)
	clk.Advance(5 * time.Minute) // idle 5m < 10m timeout
	r.reapOnce(context.Background())

	if got := sup.callCount(); got != 0 {
		t.Fatalf("CheckpointIdle called %d times, want 0", got)
	}
}

func TestReaper_ReapOnce_TemplateMayShortenButNotLengthenGlobalTimeout(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)
	st := newFakeStore()
	st.sessions["short"] = &session.Session{ID: "short", Template: "fast", AgentState: session.AgentIdle, LastActivityMs: start.UnixMilli()}
	st.sessions["long"] = &session.Session{ID: "long", Template: "slow", AgentState: session.AgentIdle, LastActivityMs: start.UnixMilli()}
	sup := &fakeSupervisor{liveIDs: []string{"short", "long"}}
	r := New(sup, st, clk, nil, 10*time.Minute, map[string]time.Duration{"fast": 2 * time.Minute, "slow": time.Hour})

	clk.Advance(3 * time.Minute)
	r.reapOnce(context.Background())
	if got := sup.callCount(); got != 1 || sup.lastCall().id != "short" {
		t.Fatalf("calls after 3m = %+v, want only short template session", sup.calls)
	}

	clk.Advance(8 * time.Minute)
	r.reapOnce(context.Background())
	if got := sup.callCount(); got != 3 {
		t.Fatalf("calls after 11m = %d, want 3 (global cap reaps both)", got)
	}
}

func TestReaper_ReapOnce_AttachedPastTimeout_NotReaped(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)

	st := newFakeStore()
	st.sessions["s1"] = &session.Session{
		ID:             "s1",
		AgentState:     session.AgentIdle,
		LastActivityMs: start.UnixMilli(),
	}
	sup := &fakeSupervisor{
		liveIDs:  []string{"s1"},
		attached: map[string]bool{"s1": true},
	}

	r := New(sup, st, clk, nil, 10*time.Minute)
	clk.Advance(11 * time.Minute)
	r.reapOnce(context.Background())

	if got := sup.callCount(); got != 0 {
		t.Fatalf("CheckpointIdle called %d times for an attached session, want 0", got)
	}
}

func TestReaper_ReapOnce_NonIdleStates_NotReaped(t *testing.T) {
	states := []session.AgentState{
		session.AgentWorking,
		session.AgentBlocked,
		session.AgentUnknown,
	}
	for _, agentState := range states {
		t.Run(string(agentState), func(t *testing.T) {
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clk := clocktest.NewFake(start)

			st := newFakeStore()
			st.sessions["s1"] = &session.Session{
				ID:             "s1",
				AgentState:     agentState,
				LastActivityMs: start.UnixMilli(),
			}
			sup := &fakeSupervisor{liveIDs: []string{"s1"}}

			r := New(sup, st, clk, nil, 10*time.Minute)
			clk.Advance(11 * time.Minute)
			r.reapOnce(context.Background())

			if got := sup.callCount(); got != 0 {
				t.Fatalf("agent_state=%s: CheckpointIdle called %d times, want 0", agentState, got)
			}
		})
	}
}

// TestReaper_ReapOnce_HeadlessIdlePastTimeout_NotReaped is m4.md §8.2's
// reaper exemption: a headless task-owned session that is idle, unattached,
// and past idle_timeout — every condition that would reap an interactive
// session — must still never be reaped. Its lifecycle authority is the
// orchestrator's per-task timeout, not this idle checkpoint; the exemption
// is keyed on Mode, checked before any of the idle/attached/timeout
// predicates below it.
func TestReaper_ReapOnce_HeadlessIdlePastTimeout_NotReaped(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)

	st := newFakeStore()
	st.sessions["s1"] = &session.Session{
		ID:             "s1",
		Mode:           session.ModeHeadless,
		AgentState:     session.AgentIdle,
		LastActivityMs: start.UnixMilli(),
	}
	sup := &fakeSupervisor{liveIDs: []string{"s1"}}

	r := New(sup, st, clk, nil, 10*time.Minute)
	clk.Advance(11 * time.Minute)
	r.reapOnce(context.Background())

	if got := sup.callCount(); got != 0 {
		t.Fatalf("CheckpointIdle called %d times for a headless session, want 0", got)
	}
}

// --- disabled reaper (idle_timeout=0): Start/Close are safe no-ops --------

func TestReaper_Disabled_StartAndCloseAreNoops(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)

	st := newFakeStore()
	st.sessions["s1"] = &session.Session{
		ID:             "s1",
		AgentState:     session.AgentIdle,
		LastActivityMs: start.UnixMilli(),
	}
	sup := &fakeSupervisor{liveIDs: []string{"s1"}}

	r := New(sup, st, clk, nil, 0)
	r.Start()
	if r.cancel != nil {
		t.Fatal("Start with idleTimeout=0 set r.cancel; want no-op (nil)")
	}

	// Advance the clock well past what would be a reap-worthy idle duration;
	// with no loop running, reapOnce is never driven.
	clk.Advance(time.Hour)

	r.Close() // must not panic/deadlock even though Start never launched a goroutine.

	if got := sup.callCount(); got != 0 {
		t.Fatalf("CheckpointIdle called %d times with reaping disabled, want 0", got)
	}
}

// --- lifecycle: Start/Close join the goroutine under -race -----------------

func TestReaper_StartClose_JoinsGoroutine(t *testing.T) {
	sup := &fakeSupervisor{}
	st := newFakeStore()

	r := New(sup, st, clock.Real(), nil, time.Second)
	r.Start()

	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — reap-loop goroutine failed to join")
	}
}
