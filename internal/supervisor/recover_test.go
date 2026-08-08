package supervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// openRecoverTestStore returns a fresh sqlite-backed store.Store rooted in
// a per-test temp dir, mirroring internal/checkpoint/resume_test.go's own
// openTestStore (duplicated rather than shared to avoid a test-only cross-
// package dependency).
func openRecoverTestStore(t *testing.T) (*store.Store, *clocktest.FakeClock) {
	t.Helper()
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, fc
}

func createRecoverTestSession(t *testing.T, st *store.Store, id string, desired session.DesiredState, status session.Status, claudeSessionID string) *session.Session {
	t.Helper()
	rec, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:              id,
		Name:            id,
		Mode:            session.ModeInteractive,
		Cwd:             "/tmp/work-" + id,
		ClaudeBin:       "/usr/local/bin/claude",
		Argv:            []string{"/usr/local/bin/claude"},
		EnvKeys:         []string{"HOME"},
		SettingsPath:    "/tmp/state/sessions/" + id + "/settings.json",
		SettingSources:  "user,project,local",
		ClaudeSessionID: claudeSessionID,
		DesiredState:    desired,
		Status:          status,
		PID:             4242,
		PGID:            4242,
		ProcStartNs:     1,
		Rows:            40,
		Cols:            120,
	})
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
	return rec
}

// fakeProcChecker reports every pid it was told to as alive, per test case.
type fakeProcChecker struct {
	alive map[int]bool
}

func (f fakeProcChecker) Alive(pid int, startNs int64) bool { return f.alive[pid] }

// fakeCheckpointer is a Checkpointer double whose Restore/Resumable/
// Checkpoint behavior is fixed per test case; it never touches a real
// process or the filesystem.
type fakeCheckpointer struct {
	resumableOK  bool
	resumableWhy string
	restoreErr   error
	restoreSpec  session.Spec
	checkpoints  []string // session IDs Checkpoint was called with
}

func (f *fakeCheckpointer) Checkpoint(ctx context.Context, s *LiveSession, reason string) error {
	f.checkpoints = append(f.checkpoints, s.SessionID)
	return nil
}

func (f *fakeCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	if f.restoreErr != nil {
		return session.Spec{}, f.restoreErr
	}
	spec := f.restoreSpec
	spec.ID = rec.ID
	return spec, nil
}

func (f *fakeCheckpointer) Resumable(rec session.Session) (bool, string) {
	return f.resumableOK, f.resumableWhy
}

// newTestRegistry builds a Registry against a real store but with a fake
// checkpointer and no live sessions — enough to exercise recover()'s store
// reads/writes and event log without spawning any real process. Spawn
// itself is only reached in the "resumable" cases below, where it will
// fail (no real claude_bin) — recoverOne treats a failed Spawn as a logged
// no-op (design doc §3.5 does not require recovery to retry), so tests
// assert on the store row's state before Spawn is attempted, not after.
func newTestRegistry(t *testing.T, st *store.Store, clk clock.Clock, cp Checkpointer) *Registry {
	t.Helper()
	return New(st, state.New(st), cp, clk, Config{}, nil)
}

func TestRecover_SkipsSessionsNotDesiredRunning(t *testing.T) {
	st, fc := openRecoverTestStore(t)
	createRecoverTestSession(t, st, "sess-stopped", session.DesiredStopped, session.StatusExited, "claude-1")

	cp := &fakeCheckpointer{}
	r := newTestRegistry(t, st, fc, cp)

	if err := r.recover(context.Background(), fakeProcChecker{}, 5*time.Millisecond); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := st.GetSession(context.Background(), "sess-stopped")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Status != session.StatusExited {
		t.Fatalf("Status = %q, want unchanged %q", got.Status, session.StatusExited)
	}
	if len(cp.checkpoints) != 0 {
		t.Fatalf("Checkpoint called %d times, want 0 (desired_state=stopped must never be touched)", len(cp.checkpoints))
	}
}

func TestRecover_UnresumableMarksFailedAndStopped(t *testing.T) {
	st, fc := openRecoverTestStore(t)
	createRecoverTestSession(t, st, "sess-1", session.DesiredRunning, session.StatusRunning, "")

	cp := &fakeCheckpointer{resumableOK: false, resumableWhy: "no claude_session_id recorded"}
	r := newTestRegistry(t, st, fc, cp)

	if err := r.recover(context.Background(), fakeProcChecker{}, 5*time.Millisecond); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := st.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Status != session.StatusFailed {
		t.Fatalf("Status = %q, want %q", got.Status, session.StatusFailed)
	}
	if got.DesiredState != session.DesiredStopped {
		t.Fatalf("DesiredState = %q, want %q (never retry an unresumable session)", got.DesiredState, session.DesiredStopped)
	}

	events, err := st.ListEvents(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionUnresumable) {
		t.Fatalf("events = %v, want a %q event", eventKinds(events), session.EventSessionUnresumable)
	}
}

func TestRecover_RestoreErrorMarksFailedAndStopped(t *testing.T) {
	st, fc := openRecoverTestStore(t)
	createRecoverTestSession(t, st, "sess-1", session.DesiredRunning, session.StatusRunning, "claude-1")

	cp := &fakeCheckpointer{resumableOK: true, restoreErr: errRestoreBoom}
	r := newTestRegistry(t, st, fc, cp)

	if err := r.recover(context.Background(), fakeProcChecker{}, 5*time.Millisecond); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := st.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Status != session.StatusFailed || got.DesiredState != session.DesiredStopped {
		t.Fatalf("Status/DesiredState = %q/%q, want failed/stopped", got.Status, got.DesiredState)
	}
}

func TestRecover_LiveOrphanIsReapedNeverReadopted(t *testing.T) {
	st, _ := openRecoverTestStore(t)
	createRecoverTestSession(t, st, "sess-1", session.DesiredRunning, session.StatusRunning, "claude-1")

	cp := &fakeCheckpointer{resumableOK: false, resumableWhy: "resume attempted below by Spawn failing; irrelevant here"}
	// A real Clock here (not the FakeClock the other cases use): grace is
	// only 1ms of real wall time, and reapOrphan's After(grace) wait would
	// never fire against a FakeClock that nothing in this test advances.
	r := newTestRegistry(t, st, clock.Real(), cp)

	pc := fakeProcChecker{alive: map[int]bool{4242: true}}
	if err := r.recover(context.Background(), pc, 1*time.Millisecond); err != nil {
		t.Fatalf("recover: %v", err)
	}

	events, err := st.ListEvents(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionOrphanReaped) {
		t.Fatalf("events = %v, want a %q event (recovery must never re-adopt a live child)", eventKinds(events), session.EventSessionOrphanReaped)
	}
}

var errRestoreBoom = fmtError("restore: boom")

type fmtError string

func (e fmtError) Error() string { return string(e) }

func hasEventKind(events []*session.Event, kind session.EventKind) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func eventKinds(events []*session.Event) []session.EventKind {
	out := make([]session.EventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}
