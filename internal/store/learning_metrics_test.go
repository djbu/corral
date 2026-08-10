package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
)

func TestLearningMetricQueriesUseHalfOpenWindow(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC))
	st, err := Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.CreateSession(ctx, CreateSessionParams{
		ID: "s", Name: "s", Mode: session.ModeInteractive, Cwd: t.TempDir(),
		ClaudeBin: "/bin/true", SettingSources: "user,project,local",
		DesiredState: session.DesiredRunning, Status: session.StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendEvent(ctx, "s", session.EventPermissionBlocked, `{}`)
	_, err = st.CreateTask(ctx, CreateTaskParams{
		ID: "t", DAGID: "d", Name: "t", Prompt: "p", Repo: t.TempDir(), Cwd: t.TempDir(), Status: TaskSucceeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateTask(ctx, "t", func(task *Task) { task.CostUSD = 2.5 })
	if err != nil {
		t.Fatal(err)
	}
	end := fc.Now().Add(time.Millisecond).UnixMilli()
	activity, err := st.ListSessionActivity(ctx, fc.Now().Add(-time.Hour).UnixMilli(), end)
	if err != nil || len(activity) != 1 || activity[0].BlockedEvents != 1 {
		t.Fatalf("activity = (%+v,%v)", activity, err)
	}
	tasks, err := st.ListTerminalTaskMetrics(ctx, fc.Now().Add(-time.Hour).UnixMilli(), end)
	if err != nil || len(tasks) != 1 || tasks[0].CostUSD != 2.5 {
		t.Fatalf("tasks = (%+v,%v)", tasks, err)
	}
	if atEnd, err := st.ListSessionActivity(ctx, fc.Now().UnixMilli(), end); err != nil || len(atEnd) != 1 {
		t.Fatalf("inclusive start activity = (%+v,%v)", atEnd, err)
	}
}
