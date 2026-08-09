package store

import (
	"context"
	"errors"
	"testing"
)

func TestStore_SubmitDAG_HappyPath(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	budget := 5.0
	sub := DAGSubmission{
		DAGID:     "dag1",
		BudgetUSD: &budget,
		Tasks: []CreateTaskParams{
			sampleCreateTaskParams("plan", "dag1", "plan"),
			sampleCreateTaskParams("implement", "dag1", "implement"),
			sampleCreateTaskParams("review", "dag1", "review"),
		},
		Deps: []Dep{
			{TaskID: "implement", DependsOn: "plan"},
			{TaskID: "review", DependsOn: "implement"},
		},
	}

	if err := st.SubmitDAG(ctx, sub); err != nil {
		t.Fatalf("SubmitDAG: %v", err)
	}

	for _, id := range []string{"plan", "implement", "review"} {
		tk, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if tk.DAGID != "dag1" {
			t.Fatalf("task %s DAGID = %q, want dag1", id, tk.DAGID)
		}
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

	b, err := st.GetDAGBudget(ctx, "dag1")
	if err != nil {
		t.Fatalf("GetDAGBudget: %v", err)
	}
	if b == nil {
		t.Fatalf("GetDAGBudget = nil, want a row")
	}
	if b.BudgetUSD == nil || *b.BudgetUSD != 5.0 {
		t.Fatalf("GetDAGBudget.BudgetUSD = %v, want 5.0", b.BudgetUSD)
	}
	if b.CostUSD != 0 {
		t.Fatalf("GetDAGBudget.CostUSD = %v, want 0", b.CostUSD)
	}
}

// TestStore_SubmitDAG_FailureRollsBackEverything proves the whole submission
// is one transaction: a duplicate task id within the batch fails the tasks
// INSERT partway through, and nothing from the batch — including the
// budget row inserted before it — must survive.
func TestStore_SubmitDAG_FailureRollsBackEverything(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	sub := DAGSubmission{
		DAGID: "dag1",
		Tasks: []CreateTaskParams{
			sampleCreateTaskParams("plan", "dag1", "plan"),
			sampleCreateTaskParams("plan", "dag1", "plan-again"), // duplicate id
		},
	}

	if err := st.SubmitDAG(ctx, sub); err == nil {
		t.Fatalf("SubmitDAG: want error for duplicate task id, got nil")
	}

	b, err := st.GetDAGBudget(ctx, "dag1")
	if err != nil {
		t.Fatalf("GetDAGBudget: %v", err)
	}
	if b != nil {
		t.Fatalf("GetDAGBudget = %+v, want nil (rolled back)", b)
	}

	if _, err := st.GetTask(ctx, "plan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask(plan): err = %v, want ErrNotFound (rolled back)", err)
	}
}

func TestStore_SubmitDAG_ValidationErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("empty dag id", func(t *testing.T) {
		st, _ := openTestStore(t)
		sub := DAGSubmission{
			DAGID: "",
			Tasks: []CreateTaskParams{sampleCreateTaskParams("t1", "", "plan")},
		}
		if err := st.SubmitDAG(ctx, sub); !errors.Is(err, ErrInvalidDAGSubmission) {
			t.Fatalf("SubmitDAG: err = %v, want ErrInvalidDAGSubmission", err)
		}
	})

	t.Run("empty tasks", func(t *testing.T) {
		st, _ := openTestStore(t)
		sub := DAGSubmission{DAGID: "dag1"}
		if err := st.SubmitDAG(ctx, sub); !errors.Is(err, ErrInvalidDAGSubmission) {
			t.Fatalf("SubmitDAG: err = %v, want ErrInvalidDAGSubmission", err)
		}
		b, err := st.GetDAGBudget(ctx, "dag1")
		if err != nil {
			t.Fatalf("GetDAGBudget: %v", err)
		}
		if b != nil {
			t.Fatalf("GetDAGBudget = %+v, want nil (no write before validation failure)", b)
		}
	})

	t.Run("self edge", func(t *testing.T) {
		st, _ := openTestStore(t)
		sub := DAGSubmission{
			DAGID: "dag1",
			Tasks: []CreateTaskParams{sampleCreateTaskParams("t1", "dag1", "plan")},
			Deps:  []Dep{{TaskID: "t1", DependsOn: "t1"}},
		}
		if err := st.SubmitDAG(ctx, sub); !errors.Is(err, ErrInvalidDAGSubmission) {
			t.Fatalf("SubmitDAG: err = %v, want ErrInvalidDAGSubmission", err)
		}
		b, err := st.GetDAGBudget(ctx, "dag1")
		if err != nil {
			t.Fatalf("GetDAGBudget: %v", err)
		}
		if b != nil {
			t.Fatalf("GetDAGBudget = %+v, want nil (no write before validation failure)", b)
		}
	})

	t.Run("dep endpoint not in tasks", func(t *testing.T) {
		st, _ := openTestStore(t)
		sub := DAGSubmission{
			DAGID: "dag1",
			Tasks: []CreateTaskParams{sampleCreateTaskParams("t1", "dag1", "plan")},
			Deps:  []Dep{{TaskID: "t1", DependsOn: "ghost"}},
		}
		if err := st.SubmitDAG(ctx, sub); !errors.Is(err, ErrInvalidDAGSubmission) {
			t.Fatalf("SubmitDAG: err = %v, want ErrInvalidDAGSubmission", err)
		}
		b, err := st.GetDAGBudget(ctx, "dag1")
		if err != nil {
			t.Fatalf("GetDAGBudget: %v", err)
		}
		if b != nil {
			t.Fatalf("GetDAGBudget = %+v, want nil (no write before validation failure)", b)
		}
	})
}

// TestStore_SubmitDAG_BudgetRowLandsBeforeAnyCost proves the budget-first
// write order inside the transaction: after a successful SubmitDAG, AddCost
// on one of its tasks must succeed rather than returning
// ErrBudgetRowMissing.
func TestStore_SubmitDAG_BudgetRowLandsBeforeAnyCost(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	sub := DAGSubmission{
		DAGID: "dag1",
		Tasks: []CreateTaskParams{sampleCreateTaskParams("t1", "dag1", "plan")},
	}
	if err := st.SubmitDAG(ctx, sub); err != nil {
		t.Fatalf("SubmitDAG: %v", err)
	}

	if err := st.AddCost(ctx, "t1", 0.5); err != nil {
		t.Fatalf("AddCost: err = %v, want nil (budget row must already exist)", err)
	}
}

// TestStore_ListDAGBudgets proves ListDAGBudgets returns every submitted
// dag's budget row, ordered by dag_id, with the denormalized cost rollup
// (never re-summed from tasks).
func TestStore_ListDAGBudgets(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	budget1 := 5.0
	sub1 := DAGSubmission{
		DAGID:     "dag1",
		BudgetUSD: &budget1,
		Tasks:     []CreateTaskParams{sampleCreateTaskParams("t1", "dag1", "plan")},
	}
	if err := st.SubmitDAG(ctx, sub1); err != nil {
		t.Fatalf("SubmitDAG(dag1): %v", err)
	}

	sub2 := DAGSubmission{
		DAGID: "dag2",
		Tasks: []CreateTaskParams{sampleCreateTaskParams("t2", "dag2", "plan")},
	}
	if err := st.SubmitDAG(ctx, sub2); err != nil {
		t.Fatalf("SubmitDAG(dag2): %v", err)
	}

	got, err := st.ListDAGBudgets(ctx)
	if err != nil {
		t.Fatalf("ListDAGBudgets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListDAGBudgets = %+v, want 2 rows", got)
	}

	if got[0].DAGID != "dag1" {
		t.Fatalf("got[0].DAGID = %q, want dag1", got[0].DAGID)
	}
	if got[0].BudgetUSD == nil || *got[0].BudgetUSD != 5.0 {
		t.Fatalf("got[0].BudgetUSD = %v, want 5.0", got[0].BudgetUSD)
	}
	if got[0].CostUSD != 0 {
		t.Fatalf("got[0].CostUSD = %v, want 0", got[0].CostUSD)
	}

	if got[1].DAGID != "dag2" {
		t.Fatalf("got[1].DAGID = %q, want dag2", got[1].DAGID)
	}
	if got[1].BudgetUSD != nil {
		t.Fatalf("got[1].BudgetUSD = %v, want nil (unbounded)", got[1].BudgetUSD)
	}
	if got[1].CostUSD != 0 {
		t.Fatalf("got[1].CostUSD = %v, want 0", got[1].CostUSD)
	}
}
