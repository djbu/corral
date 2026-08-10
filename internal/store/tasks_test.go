package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func sampleCreateTaskParams(id, dagID, name string) CreateTaskParams {
	return CreateTaskParams{
		ID:     id,
		DAGID:  dagID,
		Name:   name,
		Prompt: "do the thing",
		Repo:   "/repo",
		Cwd:    "/repo",
		Status: TaskPending,
	}
}

func TestMigration0004_AppliesCleanly(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	v, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 7 {
		t.Fatalf("SchemaVersion = %d, want 7 (0004_tasks.sql and later applied)", v)
	}

	for _, table := range []string{"tasks", "task_deps", "dag_budgets"} {
		var n int
		if err := st.db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&n); err != nil {
			t.Fatalf("checking table %s exists: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s does not exist after migration", table)
		}
	}
}

func TestStore_CreateTask_DefaultsEmptyStatusToPending(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	p := sampleCreateTaskParams("t1", "dag1", "plan")
	p.Status = "" // zero-value, as an unset field in a caller's struct literal would be
	created, err := st.CreateTask(ctx, p)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.Status != TaskPending {
		t.Fatalf("Status = %q, want pending (empty status must default, not write \"\" and become unschedulable)", created.Status)
	}
}

func TestStore_CreateAndGetTask(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	created, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag1", "plan"))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID != "t1" || created.DAGID != "dag1" || created.Name != "plan" {
		t.Fatalf("CreateTask returned %+v", created)
	}
	if created.Status != TaskPending {
		t.Fatalf("Status = %q, want pending", created.Status)
	}
	if created.Attempts != 0 {
		t.Fatalf("Attempts = %d, want 0", created.Attempts)
	}
	if created.MaxAttempts != 1 {
		t.Fatalf("MaxAttempts = %d, want 1 (default)", created.MaxAttempts)
	}
	if created.CostUSD != 0 {
		t.Fatalf("CostUSD = %v, want 0", created.CostUSD)
	}
	if created.BudgetUSD != nil {
		t.Fatalf("BudgetUSD = %v, want nil", created.BudgetUSD)
	}
	if created.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty", created.SessionID)
	}

	fetched, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fetched.Name != "plan" {
		t.Fatalf("GetTask returned %+v", fetched)
	}
}

func TestStore_GetTask_NotFound(t *testing.T) {
	st, _ := openTestStore(t)
	_, err := st.GetTask(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask: err = %v, want ErrNotFound", err)
	}
}

func TestStore_ListTasks_ScopedToDAGAndOrdered(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag1", "plan")); err != nil {
		t.Fatalf("CreateTask t1: %v", err)
	}
	fc.Advance(time.Millisecond)
	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t2", "dag1", "implement")); err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}
	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t3", "dag2", "other-dag")); err != nil {
		t.Fatalf("CreateTask t3: %v", err)
	}

	got, err := st.ListTasks(ctx, "dag1")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTasks(dag1) = %d tasks, want 2: %+v", len(got), got)
	}
	if got[0].ID != "t1" || got[1].ID != "t2" {
		t.Fatalf("ListTasks(dag1) order = [%s, %s], want [t1, t2]", got[0].ID, got[1].ID)
	}
}

func TestStore_UpdateTask_MutatePersists(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	created, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag1", "plan"))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// tasks.session_id has a real FK to sessions(id): create the row it
	// will point at before UpdateTask can legally set it.
	if _, err := st.CreateSession(ctx, sampleCreateParams("sess-1", "sess-1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	fc.Advance(time.Millisecond)

	updated, err := st.UpdateTask(ctx, "t1", func(tk *Task) {
		tk.Status = TaskRunning
		tk.Attempts = 1
		tk.SessionID = "sess-1"
		// Attempt to mutate immutable fields; must be discarded.
		tk.DAGID = "should-not-stick"
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Status != TaskRunning {
		t.Fatalf("Status = %q, want running", updated.Status)
	}
	if updated.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", updated.Attempts)
	}
	if updated.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", updated.SessionID)
	}
	if updated.DAGID != "dag1" {
		t.Fatalf("DAGID = %q, want unchanged dag1 (immutable)", updated.DAGID)
	}
	if updated.CreatedMs != created.CreatedMs {
		t.Fatalf("CreatedMs changed: got %d, want unchanged %d", updated.CreatedMs, created.CreatedMs)
	}
	if updated.UpdatedMs != fc.Now().UnixMilli() {
		t.Fatalf("UpdatedMs = %d, want %d (bumped to current clock time)", updated.UpdatedMs, fc.Now().UnixMilli())
	}

	refetched, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if refetched.Status != TaskRunning || refetched.SessionID != "sess-1" {
		t.Fatalf("persisted task = %+v, want status=running session_id=sess-1", refetched)
	}
}

func TestStore_ReadyTasks(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	mustCreate := func(id, name string) *Task {
		tk, err := st.CreateTask(ctx, sampleCreateTaskParams(id, "dag1", name))
		if err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		return tk
	}

	plan := mustCreate("plan", "plan")
	implement := mustCreate("implement", "implement")
	review := mustCreate("review", "review")
	_ = review

	if err := st.AddDep(ctx, "implement", "plan"); err != nil {
		t.Fatalf("AddDep implement->plan: %v", err)
	}
	if err := st.AddDep(ctx, "review", "implement"); err != nil {
		t.Fatalf("AddDep review->implement: %v", err)
	}

	// Nothing has succeeded yet: only the no-dep task (plan) is ready.
	ready, err := st.ReadyTasks(ctx, "dag1")
	if err != nil {
		t.Fatalf("ReadyTasks: %v", err)
	}
	assertReadyIDs(t, ready, "plan")

	// plan succeeds -> implement (whose only dep is now succeeded) becomes
	// ready; review (dep on implement, still pending) does not.
	if _, err := st.UpdateTask(ctx, plan.ID, func(tk *Task) { tk.Status = TaskSucceeded }); err != nil {
		t.Fatalf("UpdateTask plan: %v", err)
	}
	ready, err = st.ReadyTasks(ctx, "dag1")
	if err != nil {
		t.Fatalf("ReadyTasks: %v", err)
	}
	assertReadyIDs(t, ready, "implement")

	// implement running (not pending/ready) -> not returned even though
	// its deps are satisfied.
	if _, err := st.UpdateTask(ctx, implement.ID, func(tk *Task) { tk.Status = TaskRunning }); err != nil {
		t.Fatalf("UpdateTask implement: %v", err)
	}
	ready, err = st.ReadyTasks(ctx, "dag1")
	if err != nil {
		t.Fatalf("ReadyTasks: %v", err)
	}
	assertReadyIDs(t, ready)

	// implement succeeds -> review becomes ready.
	if _, err := st.UpdateTask(ctx, implement.ID, func(tk *Task) { tk.Status = TaskSucceeded }); err != nil {
		t.Fatalf("UpdateTask implement: %v", err)
	}
	ready, err = st.ReadyTasks(ctx, "dag1")
	if err != nil {
		t.Fatalf("ReadyTasks: %v", err)
	}
	assertReadyIDs(t, ready, "review")
}

func assertReadyIDs(t *testing.T, got []*Task, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ReadyTasks = %d tasks %+v, want %d (%v)", len(got), got, len(want), want)
	}
	for i, tk := range got {
		if tk.ID != want[i] {
			t.Fatalf("ReadyTasks[%d] = %s, want %s (got %v, want %v)", i, tk.ID, want[i], idsOf(got), want)
		}
	}
}

func idsOf(tasks []*Task) []string {
	ids := make([]string, len(tasks))
	for i, tk := range tasks {
		ids[i] = tk.ID
	}
	return ids
}

func TestStore_AddCost_ErrorsWhenBudgetRowMissing(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag1", "plan")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// No CreateDAGBudget call for dag1: AddCost must refuse, not silently
	// no-op or insert the missing row itself.
	err := st.AddCost(ctx, "t1", 0.42)
	if !errors.Is(err, ErrBudgetRowMissing) {
		t.Fatalf("AddCost: err = %v, want ErrBudgetRowMissing", err)
	}

	// And the task-side update must have rolled back along with it (one
	// transaction, all-or-nothing).
	tk, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.CostUSD != 0 {
		t.Fatalf("task.cost_usd = %v, want 0 (rolled back with missing budget row)", tk.CostUSD)
	}
}

func TestStore_CreateDAGBudgetThenAddCost_RollsUpBothTables(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	budget := 10.0
	if err := st.CreateDAGBudget(ctx, "dag1", &budget); err != nil {
		t.Fatalf("CreateDAGBudget: %v", err)
	}
	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag1", "plan")); err != nil {
		t.Fatalf("CreateTask t1: %v", err)
	}
	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t2", "dag1", "implement")); err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}

	if err := st.AddCost(ctx, "t1", 1.5); err != nil {
		t.Fatalf("AddCost t1: %v", err)
	}
	if err := st.AddCost(ctx, "t2", 2.25); err != nil {
		t.Fatalf("AddCost t2: %v", err)
	}

	t1, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask t1: %v", err)
	}
	if t1.CostUSD != 1.5 {
		t.Fatalf("t1.CostUSD = %v, want 1.5", t1.CostUSD)
	}
	t2, err := st.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask t2: %v", err)
	}
	if t2.CostUSD != 2.25 {
		t.Fatalf("t2.CostUSD = %v, want 2.25", t2.CostUSD)
	}

	var dagCost float64
	var dagBudget float64
	if err := st.db.QueryRowContext(ctx,
		"SELECT cost_usd, budget_usd FROM dag_budgets WHERE dag_id = ?", "dag1",
	).Scan(&dagCost, &dagBudget); err != nil {
		t.Fatalf("querying dag_budgets: %v", err)
	}
	if dagCost != 3.75 {
		t.Fatalf("dag_budgets.cost_usd = %v, want 3.75 (1.5 + 2.25 rolled up)", dagCost)
	}
	if dagBudget != 10.0 {
		t.Fatalf("dag_budgets.budget_usd = %v, want 10.0 (unchanged)", dagBudget)
	}
}

func TestStore_CreateDAGBudget_DuplicateIsTypedError(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	if err := st.CreateDAGBudget(ctx, "dag1", nil); err != nil {
		t.Fatalf("first CreateDAGBudget: %v", err)
	}
	err := st.CreateDAGBudget(ctx, "dag1", nil)
	if !errors.Is(err, ErrDAGBudgetExists) {
		t.Fatalf("second CreateDAGBudget: err = %v, want ErrDAGBudgetExists", err)
	}
}

func TestStore_GetDAGBudget(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	// (i) present, bounded row: returns both cost and budget.
	budget := 10.0
	if err := st.CreateDAGBudget(ctx, "dag-bounded", &budget); err != nil {
		t.Fatalf("CreateDAGBudget(dag-bounded): %v", err)
	}
	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag-bounded", "plan")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := st.AddCost(ctx, "t1", 2.5); err != nil {
		t.Fatalf("AddCost: %v", err)
	}

	got, err := st.GetDAGBudget(ctx, "dag-bounded")
	if err != nil {
		t.Fatalf("GetDAGBudget(dag-bounded): %v", err)
	}
	if got == nil {
		t.Fatalf("GetDAGBudget(dag-bounded) = nil, want a row")
	}
	if got.DAGID != "dag-bounded" {
		t.Fatalf("DAGID = %q, want %q", got.DAGID, "dag-bounded")
	}
	if got.BudgetUSD == nil || *got.BudgetUSD != 10.0 {
		t.Fatalf("BudgetUSD = %v, want 10.0", got.BudgetUSD)
	}
	if got.CostUSD != 2.5 {
		t.Fatalf("CostUSD = %v, want 2.5", got.CostUSD)
	}

	// (ii) present row with NULL budget_usd: BudgetUSD comes back nil
	// (unbounded), not a zero value.
	if err := st.CreateDAGBudget(ctx, "dag-unbounded", nil); err != nil {
		t.Fatalf("CreateDAGBudget(dag-unbounded): %v", err)
	}
	got, err = st.GetDAGBudget(ctx, "dag-unbounded")
	if err != nil {
		t.Fatalf("GetDAGBudget(dag-unbounded): %v", err)
	}
	if got == nil {
		t.Fatalf("GetDAGBudget(dag-unbounded) = nil, want a row")
	}
	if got.BudgetUSD != nil {
		t.Fatalf("BudgetUSD = %v, want nil (NULL budget_usd means unbounded)", *got.BudgetUSD)
	}

	// (iii) absent row: (nil, nil), never an error — an absent row means
	// unbounded (m4.md §7), and most dags have no row at all until step 24.
	got, err = st.GetDAGBudget(ctx, "dag-never-created")
	if err != nil {
		t.Fatalf("GetDAGBudget(dag-never-created): err = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("GetDAGBudget(dag-never-created) = %+v, want nil", got)
	}
}

func TestStore_SetTaskSession(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateTask(ctx, sampleCreateTaskParams("t1", "dag1", "plan")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// tasks.session_id has a real FK to sessions(id): create the row it
	// will point at before SetTaskSession can legally set it.
	if _, err := st.CreateSession(ctx, sampleCreateParams("sess-1", "sess-1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.SetTaskSession(ctx, "t1", "sess-1"); err != nil {
		t.Fatalf("SetTaskSession: %v", err)
	}
	tk, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", tk.SessionID)
	}

	if err := st.SetTaskSession(ctx, "does-not-exist", "sess-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetTaskSession on missing task: err = %v, want ErrNotFound", err)
	}
}

func TestStore_RunningTasks_FiltersAcrossDAGs(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	mk := func(id, dagID, name string, status TaskStatus) {
		if _, err := st.CreateTask(ctx, sampleCreateTaskParams(id, dagID, name)); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		if _, err := st.UpdateTask(ctx, id, func(tk *Task) { tk.Status = status }); err != nil {
			t.Fatalf("UpdateTask %s: %v", id, err)
		}
	}

	mk("t1", "dag1", "plan", TaskRunning)
	mk("t2", "dag1", "implement", TaskPending)
	mk("t3", "dag2", "other", TaskRunning)
	mk("t4", "dag2", "done", TaskSucceeded)

	running, err := st.RunningTasks(ctx)
	if err != nil {
		t.Fatalf("RunningTasks: %v", err)
	}
	if len(running) != 2 {
		t.Fatalf("RunningTasks = %d, want 2: %+v", len(running), running)
	}
	gotIDs := map[string]bool{running[0].ID: true, running[1].ID: true}
	if !gotIDs["t1"] || !gotIDs["t3"] {
		t.Fatalf("RunningTasks ids = %v, want {t1, t3}", idsOf(running))
	}
}

func TestStore_ActiveDAGs_ExcludesFullyTerminalDAGs(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	mk := func(id, dagID, name string, status TaskStatus) {
		if _, err := st.CreateTask(ctx, sampleCreateTaskParams(id, dagID, name)); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		if _, err := st.UpdateTask(ctx, id, func(tk *Task) { tk.Status = status }); err != nil {
			t.Fatalf("UpdateTask %s: %v", id, err)
		}
	}

	// dag1: one task still running -> active.
	mk("t1", "dag1", "plan", TaskRunning)
	// dag2: every task terminal (succeeded/failed/cancelled) -> not active.
	mk("t2", "dag2", "plan", TaskSucceeded)
	mk("t3", "dag2", "implement", TaskFailed)
	mk("t4", "dag2", "review", TaskCancelled)
	// dag3: one task pending -> active.
	mk("t5", "dag3", "plan", TaskPending)
	// dag4: one task blocked -> active (blocked is deliberately not
	// terminal here; explicit blocked-marking of dependents is deferred,
	// but a blocked task's dag may still have other branches progressing).
	mk("t6", "dag4", "plan", TaskBlocked)

	active, err := st.ActiveDAGs(ctx)
	if err != nil {
		t.Fatalf("ActiveDAGs: %v", err)
	}
	want := []string{"dag1", "dag3", "dag4"}
	if len(active) != len(want) {
		t.Fatalf("ActiveDAGs = %v, want %v", active, want)
	}
	for i, id := range want {
		if active[i] != id {
			t.Fatalf("ActiveDAGs = %v, want %v", active, want)
		}
	}
}

func TestStore_TaskDeps_ScopedToDAG(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	for _, tk := range []struct{ id, dagID, name string }{
		{"plan", "dag1", "plan"},
		{"implement", "dag1", "implement"},
		{"review", "dag1", "review"},
		{"other-plan", "dag2", "plan"},
		{"other-impl", "dag2", "implement"},
	} {
		if _, err := st.CreateTask(ctx, sampleCreateTaskParams(tk.id, tk.dagID, tk.name)); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.id, err)
		}
	}
	if err := st.AddDep(ctx, "implement", "plan"); err != nil {
		t.Fatalf("AddDep: %v", err)
	}
	if err := st.AddDep(ctx, "review", "implement"); err != nil {
		t.Fatalf("AddDep: %v", err)
	}
	if err := st.AddDep(ctx, "other-impl", "other-plan"); err != nil {
		t.Fatalf("AddDep: %v", err)
	}

	deps, err := st.TaskDeps(ctx, "dag1")
	if err != nil {
		t.Fatalf("TaskDeps: %v", err)
	}
	want := []Dep{
		{TaskID: "implement", DependsOn: "plan"},
		{TaskID: "review", DependsOn: "implement"},
	}
	if len(deps) != len(want) {
		t.Fatalf("TaskDeps = %+v, want %+v", deps, want)
	}
	for i := range want {
		if deps[i] != want[i] {
			t.Fatalf("TaskDeps[%d] = %+v, want %+v", i, deps[i], want[i])
		}
	}
}
