package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
)

// newLifecycleTestRegistry wires a Registry against a real sqlite store
// and a real fakeclaude binary, with EnvSnapshot/EnvPassthrough built
// directly by the test rather than through daemon.go's fixed
// envSnapshotWhitelist — that whitelist deliberately excludes anything
// outside a small, security-relevant set of names (design doc §7.2), and
// CORRAL_FAKE_* is test-only plumbing that has no business being added to
// it. Since supervisor.Config.EnvSnapshot/EnvPassthrough are just data,
// the test can hand BuildEnv exactly the fakeclaude knobs it needs
// without touching any production allowlist.
func newLifecycleTestRegistry(t *testing.T, cp Checkpointer, extraEnv map[string]string) (*Registry, *store.Store, session.Spec) {
	t.Helper()
	claudeBin := buildFakeClaude(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cwd := t.TempDir()
	snapshot := map[string]string{"PATH": os.Getenv("PATH")}
	passthrough := make([]string, 0, len(extraEnv))
	for k, v := range extraEnv {
		snapshot[k] = v
		passthrough = append(passthrough, k)
	}

	spec := session.Spec{
		ID:             newTestUUID(),
		Name:           "lifecycle-" + newTestUUID(),
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

	r := New(st, state.New(st), cp, clock.Real(), Config{
		StateDir:       t.TempDir(),
		EnvSnapshot:    snapshot,
		EnvPassthrough: passthrough,
		CorralVersion:  "test",
		APIVersion:     1,
	}, nil)
	return r, st, spec
}

// newTestUUID is not a real uuidv4 generator; it only needs to be unique
// enough within one test run to satisfy the store's session-ID/name
// columns, which do not validate uuid format.
func newTestUUID() string {
	return time.Now().Format("20060102150405.000000000")
}

// TestChildExitIsObserved verifies the reaper path (Registry.reap): a
// self-exiting child (fakeclaude's CORRAL_FAKE_EXIT_AFTER/CORRAL_FAKE_EXIT_CODE
// knobs) is observed via cmd.Wait(), and the session row ends up with
// Status=exited, the real exit code, and — critically — DesiredState
// flipped to stopped, since a self-exit must never be resumed on the next
// daemon restart (unlike a user-initiated Kill, which sets DesiredState
// before signalling for a different reason).
func TestChildExitIsObserved(t *testing.T) {
	r, st, spec := newLifecycleTestRegistry(t, killingCheckpointer{}, map[string]string{
		"CORRAL_FAKE_EXIT_AFTER": "300ms",
		"CORRAL_FAKE_EXIT_CODE":  "3",
	})

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got *session.Session
	for time.Now().Before(deadline) {
		var err error
		got, err = st.GetSession(context.Background(), spec.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.Status == session.StatusExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got.Status != session.StatusExited {
		t.Fatalf("Status = %q after 5s, want %q", got.Status, session.StatusExited)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("ExitCode = %v, want 3", got.ExitCode)
	}
	if got.ExitSignal != "" {
		t.Fatalf("ExitSignal = %q, want \"\" (normal exit, not signaled)", got.ExitSignal)
	}
	if got.DesiredState != session.DesiredStopped {
		t.Fatalf("DesiredState = %q, want %q (a self-exit must never be resumed)", got.DesiredState, session.DesiredStopped)
	}

	events, err := st.ListEvents(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionExited) {
		t.Fatalf("events = %v, want a %q event", eventKinds(events), session.EventSessionExited)
	}
}

// escalatingCheckpointer is a Checkpointer double that performs the same
// SIGTERM -> wait grace -> SIGKILL escalation as
// checkpoint.ResumeCheckpointer.Checkpoint (internal/checkpoint/resume.go)
// but is declared locally to avoid the import cycle (checkpoint already
// imports supervisor for *supervisor.LiveSession). It exists so this
// package can test Registry.Kill's grace-override plumbing — the
// `cp.(interface{ WithGrace(time.Duration) Checkpointer })` type
// assertion in Kill — end to end against a real process, without
// depending on internal/checkpoint's concrete type. The real escalation
// logic itself is already covered by
// TestResumeCheckpointer_CheckpointStopsProcessGroupAndPersists in that
// package.
type escalatingCheckpointer struct {
	clk   clock.Clock
	grace time.Duration
}

func (c escalatingCheckpointer) Checkpoint(ctx context.Context, s *LiveSession, reason string) (bool, error) {
	if s.PGID > 1 {
		_ = killGroupTolerant(s.PGID, syscall.SIGTERM)
	}
	select {
	case <-ctx.Done():
	case <-c.clk.After(c.grace):
		if s.PGID > 1 {
			_ = killGroupTolerant(s.PGID, syscall.SIGKILL)
		}
	}
	return false, nil
}

func (c escalatingCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	return session.Spec{}, nil
}

func (c escalatingCheckpointer) Resumable(rec session.Session) (bool, string) { return false, "" }

func (c escalatingCheckpointer) WithGrace(grace time.Duration) Checkpointer {
	return escalatingCheckpointer{clk: c.clk, grace: grace}
}

// TestGraceEscalation verifies that killing a session whose child ignores
// SIGTERM (fakeclaude's CORRAL_FAKE_IGNORE_SIGTERM=1) actually waits out
// the grace period and finishes it off with SIGKILL: Kill must not return
// (and the process must not die) before grace has elapsed, and the
// session's recorded exit_signal must be SIGKILL, not SIGTERM.
func TestGraceEscalation(t *testing.T) {
	const grace = 300 * time.Millisecond
	r, st, spec := newLifecycleTestRegistry(t, escalatingCheckpointer{clk: clock.Real(), grace: grace}, map[string]string{
		"CORRAL_FAKE_IGNORE_SIGTERM": "1",
	})

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Wait for fakeclaude to actually be running before sending any
	// signal: exec()/dynamic-load/Go-runtime bootstrap all happen before
	// a single line of its main() runs, so a SIGTERM sent the instant
	// Spawn returns can race the process into existence and hit the OS's
	// default (terminate) disposition regardless of where in main()
	// fakeclaude installs its signal.Notify handler — no amount of
	// reordering inside main() closes that window. Alt-screen entry is
	// evidence the process is alive and has gotten well past program
	// start (same readiness signal TestScreenBridge polls for).
	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatal("Get(id) after Spawn = false, want true")
	}
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) && !ls.Screen.DebugGrid().AltScreen {
		time.Sleep(5 * time.Millisecond)
	}
	if !ls.Screen.DebugGrid().AltScreen {
		t.Fatal("fakeclaude never reached alt-screen within 5s; can't trust a Kill sent this early")
	}

	start := time.Now()
	if _, err := r.Kill(context.Background(), spec.ID, nil); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < grace {
		t.Fatalf("Kill returned after %v, want >= grace (%v): SIGTERM must not have been enough to stop it early", elapsed, grace)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got *session.Session
	for time.Now().Before(deadline) {
		var err error
		got, err = st.GetSession(context.Background(), spec.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.Status == session.StatusExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != session.StatusExited {
		t.Fatalf("Status = %q after 5s, want %q", got.Status, session.StatusExited)
	}
	if got.ExitSignal != "SIGKILL" {
		t.Fatalf("ExitSignal = %q, want %q (SIGTERM was ignored)", got.ExitSignal, "SIGKILL")
	}
}
