package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// --- shared test scaffolding --------------------------------------------

func newTestStore(t *testing.T, clk clock.Clock) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), clk)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mkTask(t *testing.T, st *store.Store, dagID, id, name, repo string, maxAttempts int) *store.Task {
	t.Helper()
	tsk, err := st.CreateTask(context.Background(), store.CreateTaskParams{
		ID:          id,
		DAGID:       dagID,
		Name:        name,
		Prompt:      "do " + name,
		Repo:        repo,
		Cwd:         repo,
		MaxAttempts: maxAttempts,
	})
	if err != nil {
		t.Fatalf("CreateTask(%s): %v", id, err)
	}
	return tsk
}

// finishSession appends a session.result event (the shape supervisor.go's
// real headless path writes: is_error/total_cost_usd/stop_reason/
// num_turns) and marks the session exited — simulating a real headless
// child's terminal outcome without spawning a real process.
func finishSession(t *testing.T, st *store.Store, sessionID string, isError bool, costUSD float64) {
	t.Helper()
	ctx := context.Background()
	data, err := json.Marshal(map[string]any{
		"is_error":       isError,
		"total_cost_usd": costUSD,
		"stop_reason":    "end_turn",
		"num_turns":      1,
	})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if _, err := st.AppendEvent(ctx, sessionID, session.EventSessionResult, string(data)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := st.UpdateSession(ctx, sessionID, func(s *session.Session) {
		s.Status = session.StatusExited
	}); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
}

// exitWithNoResult marks a session exited WITHOUT recording any
// session.result event — the mid-turn-crash / killed-by-timeout case THE
// NIL TRAP guards against.
func exitWithNoResult(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	if _, err := st.UpdateSession(context.Background(), sessionID, func(s *session.Session) {
		s.Status = session.StatusExited
	}); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
}

// --- fakeRegistry: records Spawn/Kill calls, never runs a real process --

type fakeRegistry struct {
	mu       sync.Mutex
	spawns   []session.Spec
	kills    []string
	spawnErr error
	killErr  error
}

func (f *fakeRegistry) Spawn(ctx context.Context, spec session.Spec) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawns = append(f.spawns, spec)
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	return &session.Session{ID: spec.ID}, nil
}

func (f *fakeRegistry) Kill(ctx context.Context, idOrName string, grace *time.Duration) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, idOrName)
	if f.killErr != nil {
		return nil, f.killErr
	}
	return &session.Session{ID: idOrName}, nil
}

func (f *fakeRegistry) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawns)
}

func (f *fakeRegistry) lastSpawn() session.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns[len(f.spawns)-1]
}

func (f *fakeRegistry) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.kills)
}

func (f *fakeRegistry) lastKill() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kills[len(f.kills)-1]
}

// --- real supervisor.Registry + fakeclaude, for the one true end-to-end -
// happy-path test ---------------------------------------------------------

var (
	fakeClaudeBinOnce sync.Once
	fakeClaudeBinPath string
	fakeClaudeBinErr  error
)

// buildFakeClaude mirrors internal/supervisor/screen_bridge_test.go's
// helper of the same purpose (unexported there, so it can't be reused
// directly): builds test/fakeclaude once and caches the binary path across
// every test in this package that needs a real child process.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "corral-orchestrator-fakeclaude-")
		if err != nil {
			fakeClaudeBinErr = err
			return
		}
		bin := filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/danielbecerra/corral/test/fakeclaude")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeBinErr = fmt.Errorf("%w: %s", err, out)
			return
		}
		fakeClaudeBinPath = bin
	})
	if fakeClaudeBinErr != nil {
		t.Fatalf("building test/fakeclaude: %v", fakeClaudeBinErr)
	}
	return fakeClaudeBinPath
}

// killingCheckpointer mirrors internal/supervisor/supervisor_test.go's
// double of the same name (unexported there): a Checkpointer that actually
// SIGKILLs the process group rather than recording a call, so Kill's
// store/registry bookkeeping runs against a process that really goes away.
type killingCheckpointer struct{}

func (killingCheckpointer) Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) (bool, error) {
	if s.PGID > 1 {
		_ = syscall.Kill(-s.PGID, syscall.SIGKILL)
	}
	return false, nil
}

func (killingCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	return session.Spec{}, nil
}

func (killingCheckpointer) Resumable(rec session.Session) (bool, string) { return false, "" }

func newRealRegistry(st *store.Store, stateDir string) *supervisor.Registry {
	return supervisor.New(st, state.New(st), killingCheckpointer{}, clock.Real(), supervisor.Config{
		StateDir:          stateDir,
		EnvSnapshot:       map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion:     "test",
		APIVersion:        1,
		OutputLogMaxBytes: 1 << 20,
	}, nil)
}

// initGitRepo creates a fresh, throwaway git repository as a *task* repo
// fixture (m4.md §6.2's worktree resolution needs a real repo with a HEAD
// to branch from). This is the product's own git usage (internal/git.
// AddWorktree, invoked by the orchestrator under test) against a disposable
// temp-dir fixture — never the corral repo itself, and every git
// invocation below is scoped with -c flags so nothing is ever written to
// any global or user git config.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-q", "-m", "init")
	return dir
}

// --- 1: happy path, real fakeclaude, 3-node DAG chain --------------------

func TestOrchestrator_HappyPath_ThreeNodeChainSucceeds(t *testing.T) {
	claudeBin := buildFakeClaude(t)
	repo := t.TempDir()
	st := newTestStore(t, clock.Real())
	stateDir := t.TempDir()
	reg := newRealRegistry(st, stateDir)
	ctx := context.Background()

	dagID := "dag-happy"
	a := mkTask(t, st, dagID, "task-a", "a", repo, 1)
	b := mkTask(t, st, dagID, "task-b", "b", repo, 1)
	c := mkTask(t, st, dagID, "task-c", "c", repo, 1)
	if err := st.AddDep(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("AddDep(b,a): %v", err)
	}
	if err := st.AddDep(ctx, c.ID, b.ID); err != nil {
		t.Fatalf("AddDep(c,b): %v", err)
	}

	orch := New(reg, st, clock.Real(), nil, Config{
		TaskTimeout:   time.Minute,
		MaxConcurrent: 4,
		StateDir:      stateDir,
	}, claudeBin)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		orch.tickOnce(ctx)
		got, err := st.GetTask(ctx, c.ID)
		if err != nil {
			t.Fatalf("GetTask(c): %v", err)
		}
		if got.Status == store.TaskSucceeded || got.Status == store.TaskFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, tsk := range []*store.Task{a, b, c} {
		got, err := st.GetTask(ctx, tsk.ID)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", tsk.ID, err)
		}
		if got.Status != store.TaskSucceeded {
			t.Fatalf("task %s status = %s, want succeeded", tsk.Name, got.Status)
		}
	}
}

// --- 2: THE NIL TRAP -------------------------------------------------------

// TestOrchestrator_NoResultEvent_EndsTaskFailed is the highest-severity
// case named for step 21 (m4.md §8.1): a headless session that terminates
// with NO session.result event recorded (a mid-turn crash) must end the
// task failed, never succeeded. A buggy implementation that passed
// &streamjson.Result{IsError:false} instead of nil to HeadlessOutcome
// would make this test fail by marking the task succeeded.
func TestOrchestrator_NoResultEvent_EndsTaskFailed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)
	st := newTestStore(t, clk)
	reg := &fakeRegistry{}
	repo := t.TempDir()
	ctx := context.Background()

	tsk := mkTask(t, st, "dag-nil", "task-x", "x", repo, 1) // max_attempts=1: no retry to hide behind

	orch := New(reg, st, clk, nil, Config{TaskTimeout: time.Hour, MaxConcurrent: 4, StateDir: t.TempDir()}, "/usr/bin/true")

	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 1 {
		t.Fatalf("spawn count = %d, want 1", got)
	}
	spawned := reg.lastSpawn()

	exitWithNoResult(t, st, spawned.ID)
	orch.tickOnce(ctx)

	got, err := st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskFailed {
		t.Fatalf("task status = %s, want failed (nil-result trap: no session.result event must map to failure, not success)", got.Status)
	}
}

// --- 3: error-turn failure -> retry with backoff respected ----------------

func TestOrchestrator_ErrorTurn_RetriesWithBackoffThenFails(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)
	st := newTestStore(t, clk)
	reg := &fakeRegistry{}
	repo := t.TempDir()
	ctx := context.Background()

	tsk := mkTask(t, st, "dag-retry", "task-r", "r", repo, 3)

	orch := New(reg, st, clk, nil, Config{TaskTimeout: time.Hour, MaxConcurrent: 4, StateDir: t.TempDir()}, "/usr/bin/true")

	// attempt 1
	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 1 {
		t.Fatalf("spawn count after attempt 1 = %d, want 1", got)
	}
	finishSession(t, st, reg.lastSpawn().ID, true, 0.01)
	orch.tickOnce(ctx)

	got, err := st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TaskPending {
		t.Fatalf("status after attempt 1 failure = %s, want pending (retry scheduled)", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts after attempt 1 failure = %d, want 1", got.Attempts)
	}

	// Backoff (2s) has not elapsed: must not relaunch yet.
	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 1 {
		t.Fatalf("spawn count before backoff elapses = %d, want 1 (no premature retry)", got)
	}

	clk.Advance(2 * time.Second) // 1st retry backoff: 2s * 2^(1-1)
	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 2 {
		t.Fatalf("spawn count after 1st backoff = %d, want 2", got)
	}

	// attempt 2
	finishSession(t, st, reg.lastSpawn().ID, true, 0.01)
	orch.tickOnce(ctx)
	got, err = st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TaskPending {
		t.Fatalf("status after attempt 2 failure = %s, want pending", got.Status)
	}

	clk.Advance(4 * time.Second) // 2nd retry backoff: 2s * 2^(2-1)
	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 3 {
		t.Fatalf("spawn count after 2nd backoff = %d, want 3", got)
	}

	// attempt 3 (== max_attempts): failure now ends the task, no more retries.
	finishSession(t, st, reg.lastSpawn().ID, true, 0.01)
	orch.tickOnce(ctx)
	got, err = st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TaskFailed {
		t.Fatalf("status after exhausting max_attempts = %s, want failed", got.Status)
	}

	clk.Advance(time.Hour)
	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 3 {
		t.Fatalf("spawn count after task is failed = %d, want 3 (no further attempts)", got)
	}
}

// --- 4: per-task timeout kills and flows to failure/retry ------------------

func TestOrchestrator_TaskTimeout_KillsThenRetries(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)
	st := newTestStore(t, clk)
	reg := &fakeRegistry{}
	repo := t.TempDir()
	ctx := context.Background()

	tsk := mkTask(t, st, "dag-timeout", "task-t", "t", repo, 2)

	orch := New(reg, st, clk, nil, Config{TaskTimeout: 5 * time.Minute, MaxConcurrent: 4, StateDir: t.TempDir()}, "/usr/bin/true")

	orch.tickOnce(ctx)
	if got := reg.spawnCount(); got != 1 {
		t.Fatalf("spawn count = %d, want 1", got)
	}
	s1 := reg.lastSpawn()

	clk.Advance(6 * time.Minute) // past the 5m task timeout
	orch.tickOnce(ctx)

	if got := reg.killCount(); got != 1 {
		t.Fatalf("kill count = %d, want 1", got)
	}
	if got := reg.lastKill(); got != s1.ID {
		t.Fatalf("killed id = %q, want %q", got, s1.ID)
	}

	// Simulate Kill's real effect: the child exits with no session.result
	// event captured — a timeout kill mid-turn is indistinguishable from
	// any other mid-turn crash, so it must flow through the same
	// nil-result -> failure rule (§8.1), not a timeout-specific one.
	exitWithNoResult(t, st, s1.ID)
	orch.tickOnce(ctx)

	got, err := st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TaskPending {
		t.Fatalf("status after timeout = %s, want pending (retry scheduled)", got.Status)
	}
}

// --- 5: reconciliation respawns an orphaned running task -------------------

func TestOrchestrator_Reconciliation_OrphanedRunningTaskRespawned(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)
	st := newTestStore(t, clk)
	reg := &fakeRegistry{}
	repo := t.TempDir()
	ctx := context.Background()

	tsk := mkTask(t, st, "dag-orphan", "task-o", "o", repo, 2)

	// A pre-restart attempt's session row (tasks.session_id is a real FK,
	// so the orphan's stale reference must point at an actual row).
	staleSessionID := "stale-session-id"
	if _, err := st.CreateSession(ctx, store.CreateSessionParams{
		ID:           staleSessionID,
		Name:         "o-a1",
		Mode:         session.ModeHeadless,
		Cwd:          repo,
		ClaudeBin:    "/usr/bin/true",
		DesiredState: session.DesiredRunning,
		Status:       session.StatusRunning,
	}); err != nil {
		t.Fatalf("CreateSession(stale): %v", err)
	}

	// Simulate a task left "running" by a crashed/restarted daemon: this
	// brand-new Orchestrator has no in-flight record for it at all.
	if _, err := st.UpdateTask(ctx, tsk.ID, func(task *store.Task) {
		task.Status = store.TaskRunning
		task.Attempts = 1
		task.SessionID = staleSessionID
	}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	orch := New(reg, st, clk, nil, Config{TaskTimeout: time.Hour, MaxConcurrent: 4, StateDir: t.TempDir()}, "/usr/bin/true")

	orch.tickOnce(ctx) // reconciliation classifies the orphan and respawns (=> failure => retry)

	got, err := st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TaskPending {
		t.Fatalf("status after orphan reconciliation = %s, want pending", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts after orphan reconciliation = %d, want unchanged at 1", got.Attempts)
	}
	if got := reg.spawnCount(); got != 0 {
		t.Fatalf("spawn count immediately after reconciliation = %d, want 0 (backoff not yet elapsed)", got)
	}

	clk.Advance(2 * time.Second) // 1st retry backoff
	orch.tickOnce(ctx)

	if got := reg.spawnCount(); got != 1 {
		t.Fatalf("spawn count after backoff elapses = %d, want 1 (fresh respawn)", got)
	}
	got, err = st.GetTask(ctx, tsk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TaskRunning {
		t.Fatalf("status after respawn = %s, want running", got.Status)
	}
	if got.Attempts != 2 {
		t.Fatalf("attempts after respawn = %d, want 2", got.Attempts)
	}
	if got.SessionID == "stale-session-id" {
		t.Fatalf("task still points at the stale orphaned session_id after respawn")
	}
}

// --- 6 (part 2 of 2 — dag_test.go has the pure DetectCycle table tests):
// dependency worktrees reach the launched task ------------------------------

func TestBuildDepWorktreesJSON(t *testing.T) {
	a := &store.Task{ID: "a", Name: "a", Worktree: "/repo/.worktrees/a", Branch: "corral/task/a-a1"}
	bNoWorktree := &store.Task{ID: "b", Name: "b"} // ran directly in the repo: no Worktree
	c := &store.Task{ID: "c", Name: "c", Worktree: "/repo/.worktrees/c", Branch: "corral/task/c-a1"}
	target := &store.Task{ID: "d", Name: "d"}
	byID := map[string]*store.Task{"a": a, "b": bNoWorktree, "c": c, "d": target}
	deps := []store.Dep{
		{TaskID: "d", DependsOn: "a"},
		{TaskID: "d", DependsOn: "b"},
		{TaskID: "d", DependsOn: "c"},
		{TaskID: "x", DependsOn: "a"}, // a different task's edge: must not leak in
	}

	got, err := buildDepWorktreesJSON(target, deps, byID)
	if err != nil {
		t.Fatalf("buildDepWorktreesJSON: %v", err)
	}

	var entries []depWorktreeEntry
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("unmarshal %q: %v", got, err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2 (b has no worktree and must be omitted)", entries)
	}
	want := map[string]depWorktreeEntry{
		"a": {Name: "a", Worktree: "/repo/.worktrees/a", Branch: "corral/task/a-a1"},
		"c": {Name: "c", Worktree: "/repo/.worktrees/c", Branch: "corral/task/c-a1"},
	}
	for _, e := range entries {
		if want[e.Name] != e {
			t.Fatalf("entry %+v, want %+v", e, want[e.Name])
		}
	}
}

func TestBuildDepWorktreesJSON_NoQualifyingDeps_ReturnsEmptyString(t *testing.T) {
	target := &store.Task{ID: "d", Name: "d"}
	bNoWorktree := &store.Task{ID: "b", Name: "b"}
	byID := map[string]*store.Task{"b": bNoWorktree, "d": target}
	deps := []store.Dep{{TaskID: "d", DependsOn: "b"}}

	got, err := buildDepWorktreesJSON(target, deps, byID)
	if err != nil {
		t.Fatalf("buildDepWorktreesJSON: %v", err)
	}
	if got != "" {
		t.Fatalf("buildDepWorktreesJSON = %q, want \"\" (no dep has a resolved worktree)", got)
	}
}

// TestOrchestrator_DepWorktrees_ReachSpawnedChildSpec is the orchestrator-
// side half of constraint #7 (CORRAL_DEP_WORKTREES reaches the spawned
// child's environment): it exercises the REAL worktree-resolution path
// (git.AddWorktree against a real, disposable git fixture repo) end to end
// through launchTask, and asserts the assembled JSON lands on the Spec
// handed to Registry.Spawn. The other half — that a non-empty
// Spec.DepWorktrees reaches the child's actual env map — is
// TestBuildEnvDepWorktreesCopiedVerbatim in internal/supervisor/spawn_test.go,
// since that hop (Spec -> BuildEnv -> child env) is BuildEnv's contract,
// not the orchestrator's.
func TestOrchestrator_DepWorktrees_ReachSpawnedChildSpec(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewFake(start)
	st := newTestStore(t, clk)
	reg := &fakeRegistry{}
	repo := initGitRepo(t)
	stateDir := t.TempDir()
	ctx := context.Background()

	dagID := "dag-depwt"
	// dep requests a worktree (non-empty Worktree column signals intent —
	// see launchTask's doc comment on the pre-resolution sentinel).
	dep, err := st.CreateTask(ctx, store.CreateTaskParams{
		ID: "task-dep", DAGID: dagID, Name: "dep", Prompt: "do dep",
		Repo: repo, Cwd: repo, Worktree: "requested", MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("CreateTask(dep): %v", err)
	}
	target := mkTask(t, st, dagID, "task-target", "target", repo, 1)
	if err := st.AddDep(ctx, target.ID, dep.ID); err != nil {
		t.Fatalf("AddDep: %v", err)
	}

	orch := New(reg, st, clk, nil, Config{TaskTimeout: time.Hour, MaxConcurrent: 4, StateDir: stateDir}, "/usr/bin/true")

	orch.tickOnce(ctx) // launches dep only (target isn't ready yet)
	if got := reg.spawnCount(); got != 1 {
		t.Fatalf("spawn count after 1st tick = %d, want 1 (only dep is ready)", got)
	}
	depSpawn := reg.lastSpawn()
	if depSpawn.DepWorktrees != "" {
		t.Fatalf("dep's own DepWorktrees = %q, want \"\" (dep has no deps of its own)", depSpawn.DepWorktrees)
	}

	finishSession(t, st, depSpawn.ID, false, 0.02) // dep succeeds
	orch.tickOnce(ctx)                             // applies dep's success
	orch.tickOnce(ctx)                             // launches target, now ready

	if got := reg.spawnCount(); got != 2 {
		t.Fatalf("spawn count after dep succeeds = %d, want 2 (target now ready)", got)
	}
	targetSpawn := reg.lastSpawn()
	if targetSpawn.DepWorktrees == "" {
		t.Fatalf("target's DepWorktrees is empty, want dep's resolved worktree entry")
	}

	var entries []depWorktreeEntry
	if err := json.Unmarshal([]byte(targetSpawn.DepWorktrees), &entries); err != nil {
		t.Fatalf("unmarshal target DepWorktrees %q: %v", targetSpawn.DepWorktrees, err)
	}
	if len(entries) != 1 || entries[0].Name != "dep" {
		t.Fatalf("target DepWorktrees entries = %+v, want exactly one entry named %q", entries, "dep")
	}
	if entries[0].Worktree == "" || entries[0].Worktree == "requested" {
		t.Fatalf("dep worktree entry.Worktree = %q, want a resolved on-disk path", entries[0].Worktree)
	}
	if entries[0].Branch == "" {
		t.Fatalf("dep worktree entry.Branch is empty, want a resolved branch name")
	}
}

// TestOrchestrator_DepWorktrees_ReachRealChildEnv is the other half of
// constraint #7: it proves CORRAL_DEP_WORKTREES doesn't just reach the
// Spec (that's TestOrchestrator_DepWorktrees_ReachSpawnedChildSpec above)
// and that BuildEnv copies a non-empty Spec.DepWorktrees verbatim in
// isolation (that's TestBuildEnvDepWorktreesCopiedVerbatim in
// internal/supervisor/spawn_test.go) — it drives the real
// supervisor.Registry (real fakeclaude child, real BuildEnv call) end to
// end and asserts the actually-spawned child's persisted session row
// reports CORRAL_DEP_WORKTREES in EnvKeys. supervisor.go sets
// sess.EnvKeys = envKeyNames(spec.Env) immediately after BuildEnv
// constructs the map handed to exec.Cmd, so EnvKeys is a faithful witness
// of what the child process actually received.
func TestOrchestrator_DepWorktrees_ReachRealChildEnv(t *testing.T) {
	claudeBin := buildFakeClaude(t)
	st := newTestStore(t, clock.Real())
	repo := initGitRepo(t)
	stateDir := t.TempDir()
	reg := newRealRegistry(st, stateDir)
	ctx := context.Background()

	dagID := "dag-depwt-realenv"
	dep, err := st.CreateTask(ctx, store.CreateTaskParams{
		ID: "task-dep-re", DAGID: dagID, Name: "dep", Prompt: "do dep",
		Repo: repo, Cwd: repo, Worktree: "requested", MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("CreateTask(dep): %v", err)
	}
	target := mkTask(t, st, dagID, "task-target-re", "target", repo, 1)
	if err := st.AddDep(ctx, target.ID, dep.ID); err != nil {
		t.Fatalf("AddDep: %v", err)
	}

	orch := New(reg, st, clock.Real(), nil, Config{
		TaskTimeout:   time.Minute,
		MaxConcurrent: 4,
		StateDir:      stateDir,
	}, claudeBin)

	// Drive ticks until target has been launched (has a session_id and is
	// no longer pending) or a deadline elapses.
	deadline := time.Now().Add(20 * time.Second)
	var targetSessionID string
	for time.Now().Before(deadline) {
		orch.tickOnce(ctx)
		got, err := st.GetTask(ctx, target.ID)
		if err != nil {
			t.Fatalf("GetTask(target): %v", err)
		}
		if got.SessionID != "" {
			targetSessionID = got.SessionID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if targetSessionID == "" {
		t.Fatalf("target task never got a session_id (dep never succeeded / target never launched)")
	}

	sess, err := st.GetSession(ctx, targetSessionID)
	if err != nil {
		t.Fatalf("GetSession(target): %v", err)
	}
	found := false
	for _, k := range sess.EnvKeys {
		if k == "CORRAL_DEP_WORKTREES" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("target session EnvKeys = %v, want CORRAL_DEP_WORKTREES present (real BuildEnv call against a real dep worktree)", sess.EnvKeys)
	}
}
