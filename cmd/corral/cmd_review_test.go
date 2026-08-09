package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// skipIfNoGit mirrors internal/git's worktree_test.go helper of the same
// name: skip rather than fail when the test environment has no git binary.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// runGitFixture runs a git command with cmd.Dir = dir, failing the test on
// any error. It is test-fixture setup only (a throwaway repo under
// t.TempDir()), mirroring internal/git/worktree_test.go's newTestRepo.
func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}

// TestWorktreeDiff_UsesForkPointNotBareDiff is the diff-base regression
// test (m4.md §13 step 24 (c)): agents commit their work on the task
// branch, so a bare `git -C <worktree> diff` (working tree vs branch HEAD)
// would print nothing for that committed work — exactly the case review
// exists to surface. worktreeDiff must instead diff against the
// merge-base with the repo's HEAD, which does see the committed change.
func TestWorktreeDiff_UsesForkPointNotBareDiff(t *testing.T) {
	skipIfNoGit(t)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	runGitFixture(t, repo, "init", "-q")
	runGitFixture(t, repo, "config", "user.email", "test@example.com")
	runGitFixture(t, repo, "config", "user.name", "corral test")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitFixture(t, repo, "add", "f.txt")
	runGitFixture(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(filepath.Dir(repo), "wt")
	runGitFixture(t, repo, "worktree", "add", "-b", "task/x", wt, "HEAD")

	// Make and commit a change INSIDE the worktree, on its own branch —
	// this is the state a real corral task leaves behind.
	if err := os.WriteFile(filepath.Join(wt, "g.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitFixture(t, wt, "add", "g.txt")
	runGitFixture(t, wt, "commit", "-q", "-m", "task change")

	ctx := context.Background()

	// Sanity-check the bug being guarded against: a bare working-tree
	// diff sees nothing, since the change was committed.
	bareOut, err := runGit(ctx, wt, "diff", "--stat")
	if err != nil {
		t.Fatalf("bare diff --stat: %v", err)
	}
	if strings.TrimSpace(bareOut) != "" {
		t.Fatalf("bare `git diff --stat` = %q, want empty (this is the premise of the regression test)", bareOut)
	}

	out, err := worktreeDiff(ctx, repo, wt, false)
	if err != nil {
		t.Fatalf("worktreeDiff: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("worktreeDiff --stat output is empty, want it to show the committed g.txt change")
	}
	if !strings.Contains(out, "g.txt") {
		t.Errorf("worktreeDiff --stat output = %q, want it to mention g.txt", out)
	}

	full, err := worktreeDiff(ctx, repo, wt, true)
	if err != nil {
		t.Fatalf("worktreeDiff (full): %v", err)
	}
	if !strings.Contains(full, "world") {
		t.Errorf("worktreeDiff full output = %q, want it to contain the added line", full)
	}
}

// TestWorktreeReady covers the "requested" pre-launch sentinel
// (handlers_dags.go's handleCreate): reviewDetail must not treat that
// literal string as a real path and hand it to git -C, which would fail
// with a "cannot change to 'requested'" error for every queued or
// first-attempt-running worktree task.
func TestWorktreeReady(t *testing.T) {
	cases := []struct {
		worktree string
		want     bool
	}{
		{"", false},
		{"requested", false},
		{"/state/worktrees/dag1/task-a1", true},
	}
	for _, tc := range cases {
		if got := worktreeReady(tc.worktree); got != tc.want {
			t.Errorf("worktreeReady(%q) = %v, want %v", tc.worktree, got, tc.want)
		}
	}
}
