package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// panicCheckpointer is a Checkpointer double whose every method panics —
// used by TestWake_AlreadyLiveIsNoop to prove Wake's already-live branch
// never touches the checkpointer at all.
type panicCheckpointer struct{}

func (panicCheckpointer) Checkpoint(ctx context.Context, s *LiveSession, reason string) (bool, error) {
	panic("Checkpoint should not be called for an already-live Wake")
}

func (panicCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	panic("Restore should not be called for an already-live Wake")
}

func (panicCheckpointer) Resumable(rec session.Session) (bool, string) {
	panic("Resumable should not be called for an already-live Wake")
}

// killPGID sends SIGKILL directly to a live session's process group,
// bypassing the checkpointer entirely — used to clean up a real fakeclaude
// child a test spawned, without exercising (or depending on) Checkpoint.
func killPGID(t *testing.T, r *Registry, idOrName string) {
	t.Helper()
	if ls, ok := r.Get(idOrName); ok && ls.PGID > 1 {
		_ = syscall.Kill(-ls.PGID, syscall.SIGKILL)
	}
}

func newWakeTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestWake_AlreadyLiveIsNoop confirms Wake's fast path for a session the
// registry already tracks as live: it returns the current store row without
// touching the checkpointer at all (panicCheckpointer would fail the test
// otherwise) or bumping resume_count.
func TestWake_AlreadyLiveIsNoop(t *testing.T) {
	claudeBin := buildFakeClaude(t)
	st := newWakeTestStore(t)
	cwd := t.TempDir()

	spec := session.Spec{
		ID:             newTestUUID(),
		Name:           "wake-live-" + newTestUUID(),
		Mode:           session.ModeInteractive,
		Cwd:            cwd,
		ClaudeBin:      claudeBin,
		SettingSources: "user,project,local",
		Rows:           24,
		Cols:           80,
	}
	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             spec.ID,
		Name:           spec.Name,
		Mode:           spec.Mode,
		Cwd:            spec.Cwd,
		ClaudeBin:      spec.ClaudeBin,
		SettingSources: spec.SettingSources,
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusStarting,
		Rows:           24,
		Cols:           80,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	r := New(st, state.New(st), panicCheckpointer{}, clock.Real(), Config{
		StateDir:      t.TempDir(),
		EnvSnapshot:   map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion: "test",
		APIVersion:    1,
	}, nil)

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { killPGID(t, r, spec.ID) })

	updated, err := r.Wake(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("Wake(already-live): %v", err)
	}
	if updated.Status != session.StatusRunning {
		t.Fatalf("Status = %q, want %q", updated.Status, session.StatusRunning)
	}
	if updated.ResumeCount != 0 {
		t.Fatalf("ResumeCount = %d, want 0 (already-live wake must not resume-spawn)", updated.ResumeCount)
	}
}

// TestWake_UnresumableReturnsErrNotResumable confirms Wake rejects a
// stopped row the checkpointer reports as unresumable, and never persists
// desired_state=running for it (nothing was actually attempted).
func TestWake_UnresumableReturnsErrNotResumable(t *testing.T) {
	st := newWakeTestStore(t)
	createRecoverTestSession(t, st, "sess-1", session.DesiredStopped, session.StatusExited, "")

	cp := &fakeCheckpointer{resumableOK: false, resumableWhy: "no claude_session_id recorded"}
	r := New(st, state.New(st), cp, clock.Real(), Config{}, nil)

	_, err := r.Wake(context.Background(), "sess-1")
	if !errors.Is(err, ErrNotResumable) {
		t.Fatalf("Wake err = %v, want wrapping ErrNotResumable", err)
	}

	got, gerr := st.GetSession(context.Background(), "sess-1")
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if got.DesiredState != session.DesiredStopped {
		t.Fatalf("DesiredState = %q, want unchanged %q", got.DesiredState, session.DesiredStopped)
	}
}

// TestWake_HappyPathResumesStoppedSession exercises the full resume path:
// a stopped-but-resumable row is relaunched, resume_count increments, and a
// session.resumed event carries reason="wake" (distinguishing it from
// recoverOne's own session.resumed, which never sets a reason).
func TestWake_HappyPathResumesStoppedSession(t *testing.T) {
	claudeBin := buildFakeClaude(t)
	st := newWakeTestStore(t)
	cwd := t.TempDir()

	createRecoverTestSession(t, st, "sess-1", session.DesiredStopped, session.StatusExited, "claude-1")
	// createRecoverTestSession hardcodes Cwd/ClaudeBin to fake values;
	// overwrite them to the real fakeclaude binary/cwd this test needs
	// Restore's returned spec to actually launch.
	if _, err := st.UpdateSession(context.Background(), "sess-1", func(s *session.Session) {
		s.Cwd = cwd
		s.ClaudeBin = claudeBin
	}); err != nil {
		t.Fatalf("UpdateSession (fixing cwd/claude_bin): %v", err)
	}

	cp := &fakeCheckpointer{
		resumableOK: true,
		restoreSpec: session.Spec{
			Name:           "sess-1",
			Mode:           session.ModeInteractive,
			Cwd:            cwd,
			ClaudeBin:      claudeBin,
			SettingSources: "user,project,local",
			Rows:           40,
			Cols:           120,
		},
	}
	r := New(st, state.New(st), cp, clock.Real(), Config{
		StateDir:      t.TempDir(),
		EnvSnapshot:   map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion: "test",
		APIVersion:    1,
	}, nil)
	t.Cleanup(func() { killPGID(t, r, "sess-1") })

	updated, err := r.Wake(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if updated.Status != session.StatusRunning {
		t.Fatalf("Status = %q, want %q", updated.Status, session.StatusRunning)
	}
	if updated.DesiredState != session.DesiredRunning {
		t.Fatalf("DesiredState = %q, want %q", updated.DesiredState, session.DesiredRunning)
	}
	if updated.ResumeCount != 1 {
		t.Fatalf("ResumeCount = %d, want 1", updated.ResumeCount)
	}
	if _, ok := r.Get("sess-1"); !ok {
		t.Fatal("Get(sess-1) after Wake = false, want the resumed session live in the registry")
	}

	events, err := st.ListEvents(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind != session.EventSessionResumed {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(e.DataJSON), &data); err != nil {
			t.Fatalf("unmarshaling session.resumed data %q: %v", e.DataJSON, err)
		}
		if data["reason"] != "wake" {
			t.Fatalf("session.resumed data = %v, want reason=\"wake\"", data)
		}
		if rc, ok := data["resume_count"].(float64); !ok || rc != 1 {
			t.Fatalf("session.resumed data = %v, want resume_count=1", data)
		}
		found = true
	}
	if !found {
		t.Fatalf("events = %v, want a %q event with reason=wake", eventKinds(events), session.EventSessionResumed)
	}
}

// TestWake_NameCollisionErrors confirms Wake refuses to resume a
// terminal-state row whose name has since been reused by a different,
// active session (store's partial unique index frees a terminal row's name
// for reuse — see migrations/0002's sessions_name_active) rather than
// silently colliding with it in the live registry.
func TestWake_NameCollisionErrors(t *testing.T) {
	st := newWakeTestStore(t)

	// Insert the terminal row FIRST, then the active one reusing its name
	// SECOND — this is the realistic production ordering (session sess-1
	// ran and went terminal, freeing "work-1" under the partial unique
	// index sessions_name_active, then a new session sess-active claimed
	// it). Deliberately in this order rather than the reverse: sess-1 has
	// the lower rowid, so an unordered `WHERE name = ?` lookup (as
	// store.GetSessionByName does) returns sess-1 itself, not
	// sess-active — which is exactly the case that defeats a name-collision
	// check written as "does GetSessionByName return a different ID".
	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:              "sess-1",
		Name:            "work-1",
		Mode:            session.ModeInteractive,
		Cwd:             "/tmp/work-1",
		ClaudeBin:       "/usr/local/bin/claude",
		SettingSources:  "user,project,local",
		ClaudeSessionID: "claude-1",
		DesiredState:    session.DesiredStopped,
		Status:          session.StatusExited,
		Rows:            40,
		Cols:            120,
	}); err != nil {
		t.Fatalf("CreateSession(sess-1): %v", err)
	}
	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             "sess-active",
		Name:           "work-1",
		Mode:           session.ModeInteractive,
		Cwd:            "/tmp/work-active",
		ClaudeBin:      "/usr/local/bin/claude",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusRunning,
		Rows:           40,
		Cols:           120,
	}); err != nil {
		t.Fatalf("CreateSession(sess-active): %v", err)
	}

	cp := &fakeCheckpointer{resumableOK: true}
	r := New(st, state.New(st), cp, clock.Real(), Config{}, nil)

	_, err := r.Wake(context.Background(), "sess-1")
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("Wake err = %v, want wrapping ErrNameTaken", err)
	}

	got, gerr := st.GetSession(context.Background(), "sess-1")
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if got.DesiredState != session.DesiredStopped {
		t.Fatalf("DesiredState = %q, want unchanged %q (collision must be rejected before persisting intent)", got.DesiredState, session.DesiredStopped)
	}
}
