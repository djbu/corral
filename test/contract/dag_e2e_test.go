package contract

// TestDagE2E_ThreeNodeChain_Unattended is the M4 v0.4.0 exit-criterion
// end-to-end test (design doc §13 step 25): it drives the real `corral`
// CLI as an unattended subprocess against a real daemon, submitting a
// three-node plan->implement->review dag.toml, and asserts on the same
// wire types the daemon exposes to any client — task statuses, cost
// rollup, dependency edges, per-task worktree isolation, and the no-
// merge/no-push moat on the fixture repo. A second, gated test
// (TestDagE2E_RealClaude) exercises the identical shape against a real
// `claude` binary when CORRAL_E2E_REAL_CLAUDE=1 is set.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// skipIfNoGit skips the test when no git binary is on PATH, mirroring
// cmd/corral/cmd_review_test.go's helper of the same name (a different
// package, so it cannot be reused directly).
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// runGitFixture runs a git command with cmd.Dir = dir, failing the test on
// any error. This is test-fixture setup only — a throwaway repo under
// t.TempDir() — mirroring cmd/corral/cmd_review_test.go's helper of the
// same name and internal/git's worktree_test.go newTestRepo. It always
// execs git directly via exec.Command with an argv slice, never a shell.
func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}

// gitRevParseHEAD returns the trimmed output of `git -C dir rev-parse HEAD`.
func gitRevParseHEAD(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s rev-parse HEAD: %v\n%s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// approxEqual reports whether a and b differ by no more than eps, used for
// the dag/task cost-rollup assertions below (float64 sums of the fakeclaude
// headless scenario's fixed total_cost_usd=0.001 per attempt).
func approxEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// buildDagToml renders the plan->implement->review three-node chain
// dag.toml described by the M4 exit criterion: every node runs in an
// isolated worktree against the same fixture repo, with a dag-level
// budget cap comfortably above the fake driver's fixed per-attempt cost
// (3 * 0.001 = 0.003).
func buildDagToml(repo string) string {
	return fmt.Sprintf(`budget_usd = 1.0

[[node]]
name = "plan"
prompt = "plan the work"
repo = "%s"
worktree = true

[[node]]
name = "implement"
prompt = "implement the plan"
repo = "%s"
worktree = true
depends_on = ["plan"]

[[node]]
name = "review"
prompt = "review the implementation"
repo = "%s"
worktree = true
depends_on = ["implement"]
`, repo, repo, repo)
}

// newDagFixtureRepo creates a throwaway git repo (no remote — that absence
// is itself part of the no-push moat assertion) with one committed file,
// and returns its real (symlink-resolved) path.
func newDagFixtureRepo(t *testing.T) string {
	t.Helper()
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

	return repo
}

// TestDagE2E_ThreeNodeChain_Unattended is the M4 v0.4.0 exit criterion: an
// unattended `corral run --file dag.toml` on a three-node plan->implement->
// review chain must exit 0 only once every task has succeeded, and the
// resulting dag state (read back purely through the client SDK, exactly as
// any other observer would see it) must show the correct cost rollup,
// dependency structure, and per-task worktree isolation — with the fixture
// repo's checked-out branch and remote configuration untouched throughout
// (corral never merges back and never pushes).
func TestDagE2E_ThreeNodeChain_Unattended(t *testing.T) {
	skipIfNoGit(t)

	bins := buildBinaries(t)

	repo := newDagFixtureRepo(t)
	repoHEADBefore := gitRevParseHEAD(t, repo)

	h := startDaemon(t, "CORRAL_SESSION_CLAUDE_BIN="+bins.fakeclaude, "CORRAL_FAKE_HEADLESS_SCENARIO=success")

	dagDir := t.TempDir()
	dagPath := filepath.Join(dagDir, "dag.toml")
	if err := os.WriteFile(dagPath, []byte(buildDagToml(repo)), 0o644); err != nil {
		t.Fatalf("WriteFile dag.toml: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()

	runCmd := exec.CommandContext(runCtx, bins.corral, "run", "--file", dagPath)
	runCmd.Env = append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+h.sockPath,
		"CORRAL_DAEMON_STATE_DIR="+h.stateDir,
		"CORRAL_FAKE_HEADLESS_SCENARIO=success",
	)
	var runOut bytes.Buffer
	runCmd.Stdout = &runOut
	runCmd.Stderr = &runOut

	// `corral run` (non-detach) blocks polling GET /v1/dags/{id} until
	// every task is terminal and exits 0 only if none of them
	// failed/cancelled — so a clean exit here already IS the unattended
	// success assertion the exit criterion calls for.
	if err := runCmd.Run(); err != nil {
		t.Fatalf("corral run --file %s: %v\noutput:\n%s", dagPath, err, runOut.String())
	}

	ctx := context.Background()

	dags, err := h.client.ListDAGs(ctx)
	if err != nil {
		t.Fatalf("ListDAGs: %v", err)
	}
	if len(dags) != 1 {
		t.Fatalf("ListDAGs returned %d dags, want exactly 1 (fresh daemon): %+v", len(dags), dags)
	}
	dagID := dags[0].DAGID

	detail, err := h.client.GetDAG(ctx, dagID)
	if err != nil {
		t.Fatalf("GetDAG(%s): %v", dagID, err)
	}

	if len(detail.Tasks) != 3 {
		t.Fatalf("len(detail.Tasks) = %d, want 3: %+v", len(detail.Tasks), detail.Tasks)
	}

	nameByID := make(map[string]string, len(detail.Tasks))
	for _, task := range detail.Tasks {
		nameByID[task.ID] = task.Name

		if task.Status != "succeeded" {
			t.Errorf("task %q status = %q, want %q", task.Name, task.Status, "succeeded")
		}
		if !approxEqual(task.CostUSD, 0.001, 1e-9) {
			t.Errorf("task %q CostUSD = %v, want ~0.001", task.Name, task.CostUSD)
		}
	}

	if !approxEqual(detail.CostUSD, 0.003, 1e-9) {
		t.Errorf("detail.CostUSD = %v, want ~0.003", detail.CostUSD)
	}

	// Dependency structure: edges are recorded by task ID (DagEdgeResp),
	// so translate them back to names via nameByID before comparing
	// against the expected { implement->plan, review->implement } set.
	if len(detail.Edges) != 2 {
		t.Fatalf("len(detail.Edges) = %d, want 2: %+v", len(detail.Edges), detail.Edges)
	}
	wantEdges := map[string]string{
		"implement": "plan",
		"review":    "implement",
	}
	gotEdges := make(map[string]string, len(detail.Edges))
	for _, e := range detail.Edges {
		taskName, ok := nameByID[e.Task]
		if !ok {
			t.Fatalf("edge Task id %q does not match any task in detail.Tasks: %+v", e.Task, detail.Tasks)
		}
		dependsOnName, ok := nameByID[e.DependsOn]
		if !ok {
			t.Fatalf("edge DependsOn id %q does not match any task in detail.Tasks: %+v", e.DependsOn, detail.Tasks)
		}
		gotEdges[taskName] = dependsOnName
	}
	for task, dependsOn := range wantEdges {
		if got, ok := gotEdges[task]; !ok || got != dependsOn {
			t.Errorf("edge for task %q depends_on = %q (present=%v), want %q", task, got, ok, dependsOn)
		}
	}
	if len(gotEdges) != len(wantEdges) {
		t.Errorf("gotEdges = %+v, want exactly %+v", gotEdges, wantEdges)
	}

	// Worktree isolation: every task must have a real, distinct worktree
	// path under <stateDir>/worktrees, never the empty string and never
	// the pre-launch "requested" sentinel.
	worktreeRoot := filepath.Clean(filepath.Join(h.stateDir, "worktrees"))
	seenWorktrees := make(map[string]bool, len(detail.Tasks))
	for _, task := range detail.Tasks {
		if task.Worktree == "" {
			t.Errorf("task %q Worktree is empty, want a real path", task.Name)
			continue
		}
		if task.Worktree == "requested" {
			t.Errorf("task %q Worktree is the pre-launch sentinel %q, want a real path", task.Name, task.Worktree)
			continue
		}
		cleaned := filepath.Clean(task.Worktree)
		if !strings.HasPrefix(cleaned, worktreeRoot) {
			t.Errorf("task %q Worktree = %q, want it under %q", task.Name, task.Worktree, worktreeRoot)
		}
		if st, err := os.Stat(cleaned); err != nil || !st.IsDir() {
			t.Errorf("task %q Worktree %q does not exist on disk as a directory (err=%v): recorded metadata alone would pass vacuously if corral stopped actually creating worktrees", task.Name, cleaned, err)
		}
		seenWorktrees[cleaned] = true
	}
	if len(seenWorktrees) != 3 {
		t.Errorf("distinct worktree paths = %d, want 3: %v", len(seenWorktrees), seenWorktrees)
	}

	// No-merge / no-push moat: corral never merges task branches back
	// into the fixture repo's checked-out branch, and the fixture repo
	// never had a remote configured, so nothing could have been pushed.
	repoHEADAfter := gitRevParseHEAD(t, repo)
	if repoHEADAfter != repoHEADBefore {
		t.Errorf("repo HEAD after run = %s, want unchanged %s (corral must never merge back into the fixture repo)", repoHEADAfter, repoHEADBefore)
	}

	remoteCmd := exec.Command("git", "-C", repo, "remote", "-v")
	remoteOut, err := remoteCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s remote -v: %v\n%s", repo, err, remoteOut)
	}
	if strings.TrimSpace(string(remoteOut)) != "" {
		t.Errorf("git remote -v = %q, want empty (fixture repo must never gain a remote)", remoteOut)
	}

	// Read-only review CLI rendering: `corral review <dag-id>` must exit
	// 0 and mention every task by name.
	reviewCtx, reviewCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer reviewCancel()
	reviewCmd := exec.CommandContext(reviewCtx, bins.corral, "review", dagID)
	reviewCmd.Env = append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+h.sockPath,
		"CORRAL_DAEMON_STATE_DIR="+h.stateDir,
	)
	var reviewOut bytes.Buffer
	reviewCmd.Stdout = &reviewOut
	reviewCmd.Stderr = &reviewOut
	if err := reviewCmd.Run(); err != nil {
		t.Fatalf("corral review %s: %v\noutput:\n%s", dagID, err, reviewOut.String())
	}
	for _, name := range []string{"plan", "implement", "review"} {
		if !strings.Contains(reviewOut.String(), name) {
			t.Errorf("corral review %s output does not contain task name %q:\n%s", dagID, name, reviewOut.String())
		}
	}
}

// TestDagE2E_RealClaude exercises the identical three-node chain against a
// real `claude` binary rather than fakeclaude, gated behind
// CORRAL_E2E_REAL_CLAUDE=1 since it costs real money and requires a real
// Claude Code installation. It deliberately does not assert on exact cost
// (a real invocation's total_cost_usd will not be the fake driver's fixed
// 0.001-per-attempt figure) — only on unattended success and the same
// no-merge moat.
func TestDagE2E_RealClaude(t *testing.T) {
	if os.Getenv("CORRAL_E2E_REAL_CLAUDE") != "1" {
		t.Skip("set CORRAL_E2E_REAL_CLAUDE=1 to exercise the real claude binary")
	}
	skipIfNoGit(t)

	realClaude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found on PATH")
	}

	bins := buildBinaries(t)

	repo := newDagFixtureRepo(t)
	repoHEADBefore := gitRevParseHEAD(t, repo)

	h := startDaemon(t, "CORRAL_SESSION_CLAUDE_BIN="+realClaude)

	dagDir := t.TempDir()
	dagPath := filepath.Join(dagDir, "dag.toml")
	prompt := "Reply with the single word: done. Do not modify any files."
	dagToml := fmt.Sprintf(`budget_usd = 1.0

[[node]]
name = "plan"
prompt = "%[1]s"
repo = "%[2]s"
worktree = true

[[node]]
name = "implement"
prompt = "%[1]s"
repo = "%[2]s"
worktree = true
depends_on = ["plan"]

[[node]]
name = "review"
prompt = "%[1]s"
repo = "%[2]s"
worktree = true
depends_on = ["implement"]
`, prompt, repo)
	if err := os.WriteFile(dagPath, []byte(dagToml), 0o644); err != nil {
		t.Fatalf("WriteFile dag.toml: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	runCmd := exec.CommandContext(runCtx, bins.corral, "run", "--file", dagPath)
	runCmd.Env = append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+h.sockPath,
		"CORRAL_DAEMON_STATE_DIR="+h.stateDir,
	)
	var runOut bytes.Buffer
	runCmd.Stdout = &runOut
	runCmd.Stderr = &runOut
	if err := runCmd.Run(); err != nil {
		t.Fatalf("corral run --file %s: %v\noutput:\n%s", dagPath, err, runOut.String())
	}

	ctx := context.Background()
	dags, err := h.client.ListDAGs(ctx)
	if err != nil {
		t.Fatalf("ListDAGs: %v", err)
	}
	if len(dags) != 1 {
		t.Fatalf("ListDAGs returned %d dags, want exactly 1 (fresh daemon): %+v", len(dags), dags)
	}
	detail, err := h.client.GetDAG(ctx, dags[0].DAGID)
	if err != nil {
		t.Fatalf("GetDAG: %v", err)
	}
	if len(detail.Tasks) != 3 {
		t.Fatalf("len(detail.Tasks) = %d, want 3: %+v", len(detail.Tasks), detail.Tasks)
	}
	for _, task := range detail.Tasks {
		if task.Status != "succeeded" {
			t.Errorf("task %q status = %q, want %q", task.Name, task.Status, "succeeded")
		}
	}

	repoHEADAfter := gitRevParseHEAD(t, repo)
	if repoHEADAfter != repoHEADBefore {
		t.Errorf("repo HEAD after run = %s, want unchanged %s (corral must never merge back into the fixture repo)", repoHEADAfter, repoHEADBefore)
	}
}
