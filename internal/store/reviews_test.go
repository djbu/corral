package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
)

func TestMarkTaskSucceededCreatesIndependentPendingReview(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(100, 0))
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), clk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	task, err := st.CreateTask(ctx, CreateTaskParams{ID: "t1", DAGID: "d1", Name: "work", Prompt: "p", Repo: "/repo", Cwd: "/wt", Worktree: "/wt", Branch: "corral/task/work-a1", Status: TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateTask(ctx, task.ID, func(t *Task) { t.BaseCommit = "abc" })
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskSucceeded(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskSucceeded {
		t.Fatalf("task status=%s", got.Status)
	}
	rv, err := st.GetTaskReview(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Status != ReviewPending {
		t.Fatalf("review status=%s", rv.Status)
	}
	if err := st.CompleteTaskReview(ctx, task.ID, ReviewReleased, "branch", "", "", "def", "", session.EventReviewReleaseSucceeded, `{"task_id":"t1"}`); err != nil {
		t.Fatal(err)
	}
	rv, err = st.GetTaskReview(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Status != ReviewReleased || rv.ResultCommit != "def" {
		t.Fatalf("review=%+v", rv)
	}
	events, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != session.EventReviewReleaseSucceeded {
		t.Fatalf("events=%+v", events)
	}
}

func TestMarkTaskSucceededDoesNotInventLegacyReview(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(100, 0))
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), clk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	_, err = st.CreateTask(ctx, CreateTaskParams{ID: "legacy", DAGID: "d", Name: "legacy", Prompt: "p", Repo: "/repo", Cwd: "/wt", Worktree: "/wt", Branch: "corral/task/legacy", Status: TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskSucceeded(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTaskReview(ctx, "legacy"); err != ErrNotFound {
		t.Fatalf("GetTaskReview err=%v, want ErrNotFound", err)
	}
}
