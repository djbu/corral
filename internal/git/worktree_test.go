package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestRepo creates a throwaway git repo under t.TempDir() (never the
// corral project repo itself), with local user.email/user.name so `git
// commit` does not depend on any ambient global config, writes one file,
// and commits it so the repo has a HEAD. This is the required setup for
// exercising AddWorktree/RemoveWorktree/ListWorktrees against a real git
// binary; the repo it creates and mutates is disposable test fixture, not
// corral's own history.
func newTestRepo(t *testing.T) string {
	t.Helper()
	repo := resolvedTempDir(t)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "corral test")

	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write f.txt: %v", err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "init")

	return repo
}

// resolvedTempDir returns t.TempDir() with symlinks resolved. On macOS,
// t.TempDir() lives under /var/folders/..., which is itself a symlink to
// /private/var/folders/...; git resolves this when reporting paths in
// `worktree list --porcelain`, so tests that compare a path they handed to
// AddWorktree against ListWorktrees's output must resolve it the same way
// first, or the comparison spuriously fails on macOS.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return resolved
}

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func TestAddWorktree(t *testing.T) {
	skipIfNoGit(t)

	repo := newTestRepo(t)
	wtPath := filepath.Join(resolvedTempDir(t), "wt1")

	if err := AddWorktree(context.Background(), repo, wtPath, "corral/task/demo-a1"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree checkout not created at %s: %v", wtPath, err)
	}

	worktrees, err := ListWorktrees(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}

	var found *Worktree
	for i := range worktrees {
		if worktrees[i].Path == wtPath {
			found = &worktrees[i]
		}
	}
	if found == nil {
		t.Fatalf("ListWorktrees %+v does not contain %s", worktrees, wtPath)
	}
	if found.Branch != "corral/task/demo-a1" {
		t.Errorf("Branch = %q, want %q", found.Branch, "corral/task/demo-a1")
	}
	if found.HEAD == "" {
		t.Errorf("HEAD is empty, want a sha")
	}
	if found.Bare || found.Detached {
		t.Errorf("Bare=%v Detached=%v, want both false", found.Bare, found.Detached)
	}
}

func TestAddWorktree_DuplicateBranchFails(t *testing.T) {
	skipIfNoGit(t)

	repo := newTestRepo(t)
	base := resolvedTempDir(t)
	wtPath1 := filepath.Join(base, "wt1")
	wtPath2 := filepath.Join(base, "wt2")
	branch := "corral/task/demo-a1"

	if err := AddWorktree(context.Background(), repo, wtPath1, branch); err != nil {
		t.Fatalf("first AddWorktree: %v", err)
	}

	if err := AddWorktree(context.Background(), repo, wtPath2, branch); err == nil {
		t.Fatalf("second AddWorktree with same branch %q succeeded, want error", branch)
	}
}

func TestAddWorktree_DuplicatePathFails(t *testing.T) {
	skipIfNoGit(t)

	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt1")

	if err := AddWorktree(context.Background(), repo, wtPath, "corral/task/demo-a1"); err != nil {
		t.Fatalf("first AddWorktree: %v", err)
	}

	if err := AddWorktree(context.Background(), repo, wtPath, "corral/task/demo-a2"); err == nil {
		t.Fatalf("second AddWorktree at same path %q succeeded, want error", wtPath)
	}
}

func TestRemoveWorktree(t *testing.T) {
	skipIfNoGit(t)

	repo := newTestRepo(t)
	wtPath := filepath.Join(resolvedTempDir(t), "wt1")
	branch := "corral/task/demo-a1"

	if err := AddWorktree(context.Background(), repo, wtPath, branch); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	if err := RemoveWorktree(context.Background(), repo, wtPath); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir %s still exists after RemoveWorktree (err=%v)", wtPath, err)
	}

	worktrees, err := ListWorktrees(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	for _, w := range worktrees {
		if w.Path == wtPath {
			t.Errorf("ListWorktrees still contains removed worktree %s", wtPath)
		}
	}
}

func TestListWorktrees_MainWorktree(t *testing.T) {
	skipIfNoGit(t)

	repo := newTestRepo(t)

	worktrees, err := ListWorktrees(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(worktrees) != 1 {
		t.Fatalf("ListWorktrees on fresh repo = %+v, want exactly the main worktree", worktrees)
	}
	if worktrees[0].Path != repo {
		t.Errorf("main worktree Path = %q, want %q", worktrees[0].Path, repo)
	}
	if worktrees[0].Branch == "" {
		t.Errorf("main worktree Branch is empty, want the default branch name")
	}
}

func TestRemoveWorktree_NonexistentPathFails(t *testing.T) {
	skipIfNoGit(t)

	repo := newTestRepo(t)

	if err := RemoveWorktree(context.Background(), repo, filepath.Join(repo, "does-not-exist")); err == nil {
		t.Fatal("RemoveWorktree on nonexistent path succeeded, want error")
	}
}
