package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// killingCheckpointer is a Checkpointer double that actually terminates
// the process group (SIGKILL, no grace/escalation) instead of recording a
// call and returning — unlike fakeCheckpointer in recover_test.go, which
// deliberately never touches a real process. TestGraceEscalation (the
// integration suite) exercises the real SIGTERM-then-grace-then-SIGKILL
// path; this test only needs Kill's store/registry bookkeeping to run
// against a process that actually goes away.
type killingCheckpointer struct{}

func (killingCheckpointer) Checkpoint(ctx context.Context, s *LiveSession, reason string) (bool, error) {
	if s.PGID > 1 {
		_ = syscall.Kill(-s.PGID, syscall.SIGKILL)
	}
	return false, nil
}

func (killingCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	return session.Spec{}, nil
}

func (killingCheckpointer) Resumable(rec session.Session) (bool, string) { return false, "" }

// --- Get/register/deregister: id-vs-name resolution --------------------

func TestRegistry_GetResolvesByIDOrName(t *testing.T) {
	r := New(nil, nil, nil, clock.Real(), Config{}, nil)
	ls := &LiveSession{SessionID: "sess-1"}
	r.register(ls, "work-1")

	got, ok := r.Get("sess-1")
	if !ok || got != ls {
		t.Fatalf("Get(id) = %v, %v, want %v, true", got, ok, ls)
	}
	got, ok = r.Get("work-1")
	if !ok || got != ls {
		t.Fatalf("Get(name) = %v, %v, want %v, true", got, ok, ls)
	}
	if _, ok := r.Get("no-such-thing"); ok {
		t.Fatal("Get(unknown) = true, want false")
	}
}

func TestRegistry_DeregisterRemovesBothKeys(t *testing.T) {
	r := New(nil, nil, nil, clock.Real(), Config{}, nil)
	ls := &LiveSession{SessionID: "sess-1"}
	r.register(ls, "work-1")
	r.deregister("sess-1", "work-1")

	if _, ok := r.Get("sess-1"); ok {
		t.Fatal("Get(id) after deregister = true, want false")
	}
	if _, ok := r.Get("work-1"); ok {
		t.Fatal("Get(name) after deregister = true, want false")
	}
	if len(r.ListLive()) != 0 {
		t.Fatalf("ListLive() after deregister = %d entries, want 0", len(r.ListLive()))
	}
}

// TestRegistry_RegisterNameCollisionLastWriteWins documents current
// behavior rather than asserting a policy the registry doesn't implement:
// the registry itself never rejects a duplicate name (that guarantee is
// store.CreateSession's unique index, enforced before Spawn is ever
// called — see handlers_sessions.go's pre-create collision check). If two
// LiveSessions are nonetheless registered under the same name, the name
// index simply points at whichever was registered most recently; the
// earlier one is still reachable (and still torn down normally) by its own
// session ID.
func TestRegistry_RegisterNameCollisionLastWriteWins(t *testing.T) {
	r := New(nil, nil, nil, clock.Real(), Config{}, nil)
	first := &LiveSession{SessionID: "sess-1"}
	second := &LiveSession{SessionID: "sess-2"}
	r.register(first, "work-1")
	r.register(second, "work-1")

	got, ok := r.Get("work-1")
	if !ok || got != second {
		t.Fatalf("Get(name) after collision = %v, %v, want the second-registered %v", got, ok, second)
	}
	got, ok = r.Get("sess-1")
	if !ok || got != first {
		t.Fatalf("Get(sess-1) after name collision = %v, %v, want %v still reachable by ID", got, ok, first)
	}
	if len(r.ListLive()) != 2 {
		t.Fatalf("ListLive() = %d entries, want 2 (both remain tracked)", len(r.ListLive()))
	}
}

// --- Spawn: end-to-end registration through a real (fake) process ------

func openSupervisorTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// writeFakeClaudeScript writes a tiny shebang script standing in for
// spec.ClaudeBin (same technique as spawn_test.go's
// TestSpawnStartsRealProcessOverPTY): it ignores whatever argv BuildArgv
// appends and just sleeps, giving Spawn a real child to register without
// needing the full test/fakeclaude binary.
func writeFakeClaudeScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing fake script: %v", err)
	}
	return path
}

func TestRegistry_SpawnRegistersByIDAndNameThenKillDeregisters(t *testing.T) {
	st := openSupervisorTestStore(t)
	cwd := t.TempDir()
	claudeBin := writeFakeClaudeScript(t, cwd)
	stateDir := t.TempDir()

	spec := session.Spec{
		ID:             "22222222-2222-2222-2222-222222222222",
		Name:           "work-1",
		Mode:           session.ModeInteractive,
		Cwd:            cwd,
		ClaudeBin:      claudeBin,
		SettingSources: "user,project,local",
		Rows:           40,
		Cols:           120,
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
		Rows:           40,
		Cols:           120,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cp := killingCheckpointer{}
	r := New(st, state.New(st), cp, clock.Real(), Config{
		StateDir:       stateDir,
		EnvPassthrough: nil,
		EnvSnapshot:    map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion:  "test",
		APIVersion:     1,
	}, nil)

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if _, ok := r.Get(spec.ID); !ok {
		t.Fatal("Get(id) after Spawn = false, want true")
	}
	if _, ok := r.Get(spec.Name); !ok {
		t.Fatal("Get(name) after Spawn = false, want true")
	}

	got, err := st.GetSession(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Status != session.StatusRunning {
		t.Fatalf("Status after Spawn = %q, want %q", got.Status, session.StatusRunning)
	}
	if got.PID == 0 {
		t.Fatal("PID after Spawn = 0, want the real child's pid")
	}
	if len(got.Argv) == 0 {
		t.Fatal("Argv after Spawn = empty, want the persisted BuildArgv() result")
	}
	if len(got.EnvKeys) == 0 {
		t.Fatal("EnvKeys after Spawn = empty, want the persisted env key names")
	}

	if _, err := r.Kill(context.Background(), spec.Name, nil); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.Get(spec.ID); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := r.Get(spec.ID); ok {
		t.Fatal("Get(id) after Kill did not become false within 5s (reaper never deregistered)")
	}
}
