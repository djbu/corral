package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/hookrelay"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func createTestSession(t *testing.T, st *store.Store, id string, status session.Status) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             id,
		Name:           id,
		Mode:           session.ModeInteractive,
		Cwd:            "/tmp/work",
		ClaudeBin:      "/usr/local/bin/claude",
		Argv:           []string{"/usr/local/bin/claude"},
		EnvKeys:        []string{"HOME"},
		SettingsPath:   "/tmp/state/sessions/" + id + "/settings.json",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         status,
		PID:            1234,
		PGID:           1234,
		ProcStartNs:    1,
		Rows:           40,
		Cols:           120,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestNoopEngine_StateMapping(t *testing.T) {
	st := openTestStore(t)
	eng := New(st)

	tests := []struct {
		status session.Status
		want   session.AgentState
	}{
		{session.StatusStarting, session.AgentStarting},
		{session.StatusRunning, session.AgentRunning},
		{session.StatusStopping, session.AgentRunning},
		{session.StatusExited, session.AgentExited},
		{session.StatusFailed, session.AgentFailed},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			id := "sess-" + string(tt.status)
			createTestSession(t, st, id, tt.status)

			got := eng.State(id)
			if got != tt.want {
				t.Fatalf("State() for status %q = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestNoopEngine_StateUnknownSessionIsFailed(t *testing.T) {
	st := openTestStore(t)
	eng := New(st)

	got := eng.State("does-not-exist")
	if got != session.AgentFailed {
		t.Fatalf("State() for unknown session = %q, want %q", got, session.AgentFailed)
	}
}

func TestNoopEngine_LifecycleAndHookEventAreNoops(t *testing.T) {
	st := openTestStore(t)
	eng := New(st)

	if err := eng.OnHookEvent(context.Background(), "sess-1", HookEvent{Common: hookrelay.Common{HookEventName: "PreToolUse"}}); err != nil {
		t.Fatalf("OnHookEvent: %v", err)
	}
	if err := eng.OnLifecycle(context.Background(), "sess-1", session.EventSessionSpawned, nil); err != nil {
		t.Fatalf("OnLifecycle: %v", err)
	}
}
