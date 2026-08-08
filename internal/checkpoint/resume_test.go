package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

func openTestStore(t *testing.T) (*store.Store, *clocktest.FakeClock) {
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

func createTestSession(t *testing.T, st *store.Store, id, cwd, claudeSessionID string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:              id,
		Name:            id,
		Mode:            session.ModeInteractive,
		Cwd:             cwd,
		ClaudeBin:       "/usr/local/bin/claude",
		Argv:            []string{"/usr/local/bin/claude"},
		EnvKeys:         []string{"HOME"},
		SettingsPath:    "/tmp/state/sessions/" + id + "/settings.json",
		SettingSources:  "user,project,local",
		ClaudeSessionID: claudeSessionID,
		DesiredState:    session.DesiredRunning,
		Status:          session.StatusRunning,
		PID:             1234,
		PGID:            1234,
		ProcStartNs:     1,
		Rows:            40,
		Cols:            120,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestResumable(t *testing.T) {
	claudeHome := t.TempDir()

	tests := []struct {
		name        string
		claudeSess  string
		writeFile   bool
		wantOK      bool
		wantWhySubs string
	}{
		{name: "no claude_session_id", claudeSess: "", writeFile: false, wantOK: false, wantWhySubs: "no claude_session_id"},
		{name: "session id but no transcript file", claudeSess: "sess-abc", writeFile: false, wantOK: false, wantWhySubs: "not found"},
		{name: "session id with transcript file", claudeSess: "sess-present", writeFile: true, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := "/tmp/work-" + tt.name
			rec := session.Session{ID: "id-1", Cwd: cwd, ClaudeSessionID: tt.claudeSess}

			if tt.writeFile {
				dir := filepath.Join(claudeHome, "projects", pathSlug(cwd))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				f := filepath.Join(dir, tt.claudeSess+".jsonl")
				if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}

			c := &ResumeCheckpointer{ClaudeHome: claudeHome}
			ok, why := c.Resumable(rec)
			if ok != tt.wantOK {
				t.Fatalf("Resumable() ok = %v, want %v (why=%q)", ok, tt.wantOK, why)
			}
			if tt.wantWhySubs != "" && !containsSubstring(why, tt.wantWhySubs) {
				t.Fatalf("Resumable() why = %q, want substring %q", why, tt.wantWhySubs)
			}
		})
	}
}

// pathSlug mirrors internal/claude/sessions.Slug without importing it
// twice in the test (avoids a second dependency edge just for the test);
// it must stay consistent with that package's rule (literal '/' -> '-').
func pathSlug(cwd string) string {
	out := make([]byte, 0, len(cwd))
	for i := 0; i < len(cwd); i++ {
		if cwd[i] == '/' {
			out = append(out, '-')
		} else {
			out = append(out, cwd[i])
		}
	}
	return string(out)
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// spawnGroupLeader starts a long-sleeping child as its own session/process
// group leader (mirroring what supervisor.Spawn's pty.StartWithSize gives
// us in production) so Checkpoint has a real pgid to signal.
func spawnGroupLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting test child: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func TestResumeCheckpointer_CheckpointStopsProcessGroupAndPersists(t *testing.T) {
	st, _ := openTestStore(t)
	createTestSession(t, st, "sess-1", "/tmp/work", "")

	cmd := spawnGroupLeader(t)
	pgid := cmd.Process.Pid

	// A real Clock with a short grace: /bin/sleep has no SIGTERM handler,
	// so the OS default (terminate) reaps it well inside the grace
	// window, exercising the same "already gone by the time we'd
	// escalate" ESRCH-tolerant path SIGKILL takes in production when
	// SIGTERM alone was enough.
	c := NewResumeCheckpointer(st, clock.Real(), 200*time.Millisecond, t.TempDir(), map[string]string{"PATH": "/bin"}, nil, "xterm-256color", "/tmp/corral.sock")

	tok, err := c.Checkpoint(context.Background(), &supervisor.LiveSession{SessionID: "sess-1", PGID: pgid}, "test")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if tok.ClaudeSessionID != "sess-1" {
		t.Fatalf("Token.ClaudeSessionID = %q, want %q", tok.ClaudeSessionID, "sess-1")
	}
	if tok.TurnBoundaryVerified {
		t.Fatalf("Token.TurnBoundaryVerified = true, want false in M1")
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("expected the test child to have been signaled, Wait() returned nil")
	}

	got, err := st.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ClaudeSessionID != "sess-1" {
		t.Fatalf("persisted ClaudeSessionID = %q, want %q", got.ClaudeSessionID, "sess-1")
	}
	if got.Status != session.StatusExited {
		t.Fatalf("persisted Status = %q, want %q", got.Status, session.StatusExited)
	}
}

func TestResumeCheckpointer_Restore(t *testing.T) {
	c := NewResumeCheckpointer(nil, clocktest.NewFake(time.Now()), time.Second, "", map[string]string{"PATH": "/bin", "HOME": "/home/x"}, nil, "xterm-256color", "/tmp/corral.sock")

	rec := session.Session{
		ID:              "id-1",
		Name:            "work-1",
		Mode:            session.ModeInteractive,
		Cwd:             "/tmp/work",
		ClaudeBin:       "/usr/local/bin/claude",
		SettingSources:  "user,project,local",
		SettingsPath:    "/tmp/state/sessions/id-1/settings.json",
		ClaudeSessionID: "claude-sess-1",
		Rows:            40,
		Cols:            120,
	}

	spec, err := c.Restore(context.Background(), rec)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if spec.ResumeFrom != "claude-sess-1" {
		t.Fatalf("spec.ResumeFrom = %q, want %q", spec.ResumeFrom, "claude-sess-1")
	}
	if spec.Env["PATH"] != "/bin" {
		t.Fatalf("spec.Env[PATH] = %q, want %q", spec.Env["PATH"], "/bin")
	}
}

func TestResumeCheckpointer_RestoreRequiresClaudeSessionID(t *testing.T) {
	c := NewResumeCheckpointer(nil, clocktest.NewFake(time.Now()), time.Second, "", nil, nil, "xterm-256color", "/tmp/corral.sock")
	_, err := c.Restore(context.Background(), session.Session{ID: "id-1"})
	if err == nil {
		t.Fatalf("Restore: want error for missing claude_session_id, got nil")
	}
}
