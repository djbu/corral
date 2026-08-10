package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/store"
)

type fixture struct {
	repo, wt, base, taskHead string
	st                       *store.Store
	svc                      *Service
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func newFixture(t *testing.T, id string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "init", "-q", "-b", "main")
	gitCmd(t, repo, "config", "user.email", "test@example.com")
	gitCmd(t, repo, "config", "user.name", "corral test")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base\n"), 0o644)
	gitCmd(t, repo, "add", "f.txt")
	gitCmd(t, repo, "commit", "-q", "-m", "base")
	base := gitCmd(t, repo, "rev-parse", "HEAD")
	wt := filepath.Join(root, "wt")
	branch := "corral/task/" + id + "-a1"
	if err := corralgit.AddWorktreeAt(context.Background(), repo, wt, branch, base); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "g.txt"), []byte("task\n"), 0o644)
	gitCmd(t, wt, "add", "g.txt")
	gitCmd(t, wt, "commit", "-q", "-m", "task")
	head := gitCmd(t, wt, "rev-parse", "HEAD")
	clk := clocktest.NewFake(time.Unix(123, 0))
	st, err := store.Open(filepath.Join(root, "state.db"), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task, err := st.CreateTask(context.Background(), store.CreateTaskParams{ID: id, DAGID: "d", Name: id, Prompt: "p", Repo: repo, Cwd: wt, Worktree: wt, Branch: branch, Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateTask(context.Background(), task.ID, func(x *store.Task) { x.BaseCommit = base })
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskSucceeded(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return &fixture{repo: repo, wt: wt, base: base, taskHead: head, st: st, svc: New(st, clk)}
}

func TestReleaseMergeHappyPath(t *testing.T) {
	f := newFixture(t, "merge")
	ctx := context.Background()
	p, err := f.svc.Preflight(ctx, "merge", StrategyMerge, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanApply {
		t.Fatalf("preflight blockers=%v", p.Blockers)
	}
	result, err := f.svc.Release(ctx, "merge", ReleaseRequest{Strategy: StrategyMerge, Target: "main", ExpectedTargetHead: p.TargetHead})
	if err != nil {
		t.Fatal(err)
	}
	if result.Review.Status != store.ReviewReleased {
		t.Fatalf("review=%+v", result.Review)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "g.txt")); err != nil {
		t.Fatal("merged file missing")
	}
}

func TestReleaseCherryPickHappyPath(t *testing.T) {
	f := newFixture(t, "pick")
	ctx := context.Background()
	p, err := f.svc.Preflight(ctx, "pick", StrategyCherryPick, "main")
	if err != nil || !p.CanApply {
		t.Fatalf("preflight=%+v err=%v", p, err)
	}
	result, err := f.svc.Release(ctx, "pick", ReleaseRequest{Strategy: StrategyCherryPick, Target: "main", ExpectedTargetHead: p.TargetHead})
	if err != nil {
		t.Fatal(err)
	}
	if result.Review.Status != store.ReviewReleased || result.Review.ResultCommit == "" {
		t.Fatalf("review=%+v", result.Review)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "g.txt")); err != nil {
		t.Fatal("cherry-picked file missing")
	}
}

func TestPreflightDetectsConflictWithoutMutatingTarget(t *testing.T) {
	f := newFixture(t, "conflict")
	os.WriteFile(filepath.Join(f.wt, "f.txt"), []byte("task side\n"), 0o644)
	gitCmd(t, f.wt, "add", "f.txt")
	gitCmd(t, f.wt, "commit", "-q", "-m", "task conflict")
	os.WriteFile(filepath.Join(f.repo, "f.txt"), []byte("main side\n"), 0o644)
	gitCmd(t, f.repo, "add", "f.txt")
	gitCmd(t, f.repo, "commit", "-q", "-m", "main conflict")
	before := gitCmd(t, f.repo, "rev-parse", "HEAD")
	p, err := f.svc.Preflight(context.Background(), "conflict", StrategyMerge, "main")
	if err != nil {
		t.Fatal(err)
	}
	if p.CanApply || !contains(p.Blockers, "task conflicts with target") {
		t.Fatalf("preflight=%+v", p)
	}
	if after := gitCmd(t, f.repo, "rev-parse", "HEAD"); after != before {
		t.Fatalf("preflight mutated target %s -> %s", before, after)
	}
}

func TestReleaseRejectsChangedTargetIdentity(t *testing.T) {
	f := newFixture(t, "race")
	ctx := context.Background()
	p, err := f.svc.Preflight(ctx, "race", StrategyMerge, "main")
	if err != nil || !p.CanApply {
		t.Fatalf("preflight=%+v err=%v", p, err)
	}
	os.WriteFile(filepath.Join(f.repo, "main.txt"), []byte("advance\n"), 0o644)
	gitCmd(t, f.repo, "add", "main.txt")
	gitCmd(t, f.repo, "commit", "-q", "-m", "advance")
	_, err = f.svc.Release(ctx, "race", ReleaseRequest{Strategy: StrategyMerge, Target: "main", ExpectedTargetHead: p.TargetHead})
	if err != ErrIdentityChanged {
		t.Fatalf("err=%v, want identity changed", err)
	}
}

func TestDiscardCreatesRecoveryRefAndKeepsBranch(t *testing.T) {
	f := newFixture(t, "discard")
	ctx := context.Background()
	p, err := f.svc.Preflight(ctx, "discard", StrategyBranch, "")
	if err != nil || !p.CanApply {
		t.Fatalf("preflight=%+v err=%v", p, err)
	}
	result, err := f.svc.Discard(ctx, "discard", DiscardRequest{ExpectedTaskHead: p.TaskHead})
	if err != nil {
		t.Fatal(err)
	}
	if result.Review.Status != store.ReviewDiscarded || result.Review.RecoveryRef == "" {
		t.Fatalf("review=%+v", result.Review)
	}
	if got := gitCmd(t, f.repo, "rev-parse", result.Review.RecoveryRef); got != f.taskHead {
		t.Fatalf("recovery=%s", got)
	}
	if got := gitCmd(t, f.repo, "rev-parse", "refs/heads/"+p.Branch); got != f.taskHead {
		t.Fatalf("branch=%s", got)
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
}

func TestDiscardDirtyRequiresForceAndExactIdentity(t *testing.T) {
	f := newFixture(t, "dirty")
	if err := os.WriteFile(filepath.Join(f.wt, "untracked.txt"), []byte("lost if forced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := f.svc.Preflight(ctx, "dirty", StrategyBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.CanApply || !p.TaskDirty {
		t.Fatalf("preflight=%+v", p)
	}
	if _, err := f.svc.Discard(ctx, "dirty", DiscardRequest{ExpectedTaskHead: p.TaskHead}); err != ErrPreflight {
		t.Fatalf("err=%v", err)
	}
	if _, err := f.svc.Discard(ctx, "dirty", DiscardRequest{ExpectedTaskHead: p.TaskHead, ExpectedRepo: "wrong", ExpectedWorktree: p.Worktree, ExpectedBranch: p.Branch, Force: true}); err != ErrIdentityChanged {
		t.Fatalf("err=%v", err)
	}
	result, err := f.svc.Discard(ctx, "dirty", DiscardRequest{ExpectedTaskHead: p.TaskHead, ExpectedRepo: p.Repo, ExpectedWorktree: p.Worktree, ExpectedBranch: p.Branch, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Review.Status != store.ReviewDiscarded {
		t.Fatalf("review=%+v", result.Review)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
