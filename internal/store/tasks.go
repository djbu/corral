package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TaskStatus mirrors the session.Status string-enum convention (design doc
// m4.md §3.2 "sharp edges"): tasks.status has no CHECK constraint, so
// validity is enforced in Go, not SQLite.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskReady     TaskStatus = "ready"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
	TaskBlocked   TaskStatus = "blocked"
)

// ErrBudgetRowMissing is returned by AddCost when taskID's dag has no
// dag_budgets row. CreateDAGBudget is the sole writer of that row, and
// `corral run` submission (m4.md §13 step 24) always calls it once before
// inserting any task belonging to the dag, so this should be unreachable
// in normal operation. AddCost deliberately refuses to paper over it by
// inserting the missing row itself — that would hide a submission-ordering
// bug (tasks landing before their dag's budget row) instead of surfacing
// it.
var ErrBudgetRowMissing = errors.New("store: dag_budgets row missing for task's dag")

// ErrDAGBudgetExists is returned by CreateDAGBudget when dagID already has
// a budget row. CreateDAGBudget is the sole writer of dag_budgets rows
// (m4.md §3.3) and errors on a duplicate insert rather than silently
// no-op'ing, so a caller that races or retries submission notices instead
// of assuming its budget_usd value won. A caller that wants idempotent
// resubmission should check GetTask/ListTasks (or catch this error) rather
// than rely on a soft no-op here.
var ErrDAGBudgetExists = errors.New("store: dag budget already exists")

// ErrInvalidDAGSubmission is returned by SubmitDAG when sub fails one of
// its cheap, pre-transaction shape checks (empty DAGID, no tasks, a dep
// edge naming a task not in the batch, or a self-edge). It is checked
// before any write, so a failure here never touches the database.
var ErrInvalidDAGSubmission = errors.New("store: invalid dag submission")

// Task is the persisted view of a row in the tasks table (m4.md §3.2).
// Nullable DB columns map to "" (string columns) or a nil pointer
// (BudgetUSD), matching the session.Session convention in sessions.go.
type Task struct {
	ID             string
	DAGID          string
	Name           string
	Prompt         string
	Repo           string
	Cwd            string
	Worktree       string
	Branch         string
	Model          string
	PermissionMode string
	Status         TaskStatus
	Attempts       int
	MaxAttempts    int
	SessionID      string
	CostUSD        float64
	BudgetUSD      *float64
	CreatedMs      int64
	UpdatedMs      int64
}

// CreateTaskParams is everything CreateTask needs to insert a new row.
// Attempts always starts at 0 and cost_usd at 0 — CreateTask hardcodes
// both rather than taking them as fields, since a freshly created task has
// made no attempts and accrued no cost. Timestamps are stamped by
// CreateTask from the Store's Clock, never passed in.
type CreateTaskParams struct {
	ID             string
	DAGID          string
	Name           string
	Prompt         string
	Repo           string
	Cwd            string
	Worktree       string
	Branch         string
	Model          string
	PermissionMode string
	Status         TaskStatus
	MaxAttempts    int // <= 0 defaults to 1, matching the column's DEFAULT 1
	BudgetUSD      *float64
}

// createTaskTx inserts a new task row against q (either s.db standalone or a
// *sql.Tx composed inside SubmitDAG's batch) — the INSERT only, with no
// re-read, so it can be called N times inside one transaction without ever
// reaching for s.db (which would deadlock under SetMaxOpenConns(1) if q is
// already a tx on the connection's one slot).
func (s *Store) createTaskTx(ctx context.Context, q dbtx, p CreateTaskParams) error {
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	status := p.Status
	if status == "" {
		// A task with no explicit status is newly created and has no
		// deps resolved yet either way — default to pending rather than
		// writing "" into a NOT NULL, non-CHECK-constrained column.
		// Leaving it "" would silently exclude the row from every status
		// filter (ReadyTasks' `status IN ('pending','ready')`,
		// RunningTasks' `status = 'running'`), making the task
		// permanently unschedulable with no error anywhere.
		status = TaskPending
	}
	now := s.clk.Now().UnixMilli()

	_, err := q.ExecContext(ctx, `
		INSERT INTO tasks (
			id, dag_id, name, prompt, repo, cwd, worktree, branch, model,
			permission_mode, status, attempts, max_attempts, session_id,
			cost_usd, budget_usd, created_ms, updated_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, NULL, 0, ?, ?, ?)
	`,
		p.ID, p.DAGID, p.Name, p.Prompt, p.Repo, p.Cwd, nullableStr(p.Worktree),
		nullableStr(p.Branch), nullableStr(p.Model), nullableStr(p.PermissionMode),
		string(status), maxAttempts, nullableFloatPtr(p.BudgetUSD), now, now,
	)
	if err != nil {
		return fmt.Errorf("store: creating task %s: %w", p.Name, err)
	}
	return nil
}

// CreateTask inserts a new task row and returns it persisted (i.e.
// re-fetched, matching CreateSession's pattern).
func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (*Task, error) {
	if err := s.createTaskTx(ctx, s.db, p); err != nil {
		return nil, err
	}
	return s.GetTask(ctx, p.ID)
}

// GetTask returns the task with the given id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, taskSelectColumns+" FROM tasks WHERE id = ?", id)
	return scanTask(row)
}

// ListTasks returns every task belonging to dagID, ordered by created_ms
// ascending (i.e. creation order) for deterministic output.
func (s *Store) ListTasks(ctx context.Context, dagID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx,
		taskSelectColumns+" FROM tasks WHERE dag_id = ? ORDER BY created_ms ASC, id ASC", dagID)
	if err != nil {
		return nil, fmt.Errorf("store: listing tasks for dag %s: %w", dagID, err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing tasks for dag %s: %w", dagID, err)
	}
	return out, nil
}

// UpdateTask loads the task with id, applies mutate to it, and writes
// every mutable column back — all inside one transaction, mirroring
// UpdateSession's read-modify-write pattern exactly. updated_ms is always
// bumped to the Store's current clock time. mutate must not change ID,
// DAGID, or CreatedMs; if it does, those changes are discarded (id is the
// WHERE key, CreatedMs is immutable by design, and DAGID must stay stable
// because AddCost's dag rollup and ReadyTasks' dep-graph queries are both
// keyed on it).
func (s *Store) UpdateTask(ctx context.Context, id string, mutate func(*Task)) (*Task, error) {
	var updated *Task
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, taskSelectColumns+" FROM tasks WHERE id = ?", id)
		t, err := scanTask(row)
		if err != nil {
			return err
		}

		originalID, originalDAGID, originalCreated := t.ID, t.DAGID, t.CreatedMs
		mutate(t)
		t.ID = originalID
		t.DAGID = originalDAGID
		t.CreatedMs = originalCreated
		t.UpdatedMs = s.clk.Now().UnixMilli()

		_, err = tx.ExecContext(ctx, `
			UPDATE tasks SET
				name = ?, prompt = ?, repo = ?, cwd = ?, worktree = ?, branch = ?,
				model = ?, permission_mode = ?, status = ?, attempts = ?,
				max_attempts = ?, session_id = ?, cost_usd = ?, budget_usd = ?,
				updated_ms = ?
			WHERE id = ?
		`,
			t.Name, t.Prompt, t.Repo, t.Cwd, nullableStr(t.Worktree), nullableStr(t.Branch),
			nullableStr(t.Model), nullableStr(t.PermissionMode), string(t.Status), t.Attempts,
			t.MaxAttempts, nullableStr(t.SessionID), t.CostUSD, nullableFloatPtr(t.BudgetUSD),
			t.UpdatedMs, t.ID,
		)
		if err != nil {
			return fmt.Errorf("store: updating task %s: %w", t.ID, err)
		}
		updated = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// addDepTx inserts one task_deps row against q, the tx-scoped counterpart of
// AddDep used by SubmitDAG so every edge in a batch lands on the same
// transaction as the tasks and budget row around it.
func (s *Store) addDepTx(ctx context.Context, q dbtx, taskID, dependsOn string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO task_deps (task_id, depends_on) VALUES (?, ?)`,
		taskID, dependsOn,
	)
	if err != nil {
		return fmt.Errorf("store: adding dep %s -> %s: %w", taskID, dependsOn, err)
	}
	return nil
}

// AddDep records that taskID depends on dependsOn (task_deps, m4.md §3.2):
// taskID cannot become ready until dependsOn reaches status='succeeded'.
// Both ids must already exist — the ON DELETE CASCADE foreign keys enforce
// that at runtime (the long-lived connection runs with foreign_keys ON) and
// a violation surfaces as a wrapped error here. Cycle detection is the
// orchestrator's job (m4.md §8.1), not the store's.
func (s *Store) AddDep(ctx context.Context, taskID, dependsOn string) error {
	return s.addDepTx(ctx, s.db, taskID, dependsOn)
}

// ReadyTasks returns dagID's tasks that are ready to run: status is
// 'pending' or 'ready', and every dependency (if any) has reached
// status='succeeded'. A task with no deps at all is ready by definition —
// the NOT EXISTS subquery is vacuously true for it.
func (s *Store) ReadyTasks(ctx context.Context, dagID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx,
		taskSelectColumns+`
		FROM tasks
		WHERE dag_id = ?
		AND status IN ('pending', 'ready')
		AND NOT EXISTS (
			SELECT 1 FROM task_deps d
			JOIN tasks dep ON dep.id = d.depends_on
			WHERE d.task_id = tasks.id AND dep.status != 'succeeded'
		)
		ORDER BY created_ms ASC, id ASC
	`, dagID)
	if err != nil {
		return nil, fmt.Errorf("store: listing ready tasks for dag %s: %w", dagID, err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing ready tasks for dag %s: %w", dagID, err)
	}
	return out, nil
}

// RunningTasks returns every task with status='running' across ALL dags,
// for the orchestrator's daemon-start reconciliation (m4.md §9): on
// restart it rebuilds its in-flight set from this rather than trusting
// anything held in memory before the crash/restart.
func (s *Store) RunningTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx,
		taskSelectColumns+" FROM tasks WHERE status = ? ORDER BY dag_id ASC, created_ms ASC, id ASC",
		string(TaskRunning),
	)
	if err != nil {
		return nil, fmt.Errorf("store: listing running tasks: %w", err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing running tasks: %w", err)
	}
	return out, nil
}

// terminalDAGStatuses is the set of task statuses ActiveDAGs treats as
// "this dag is done" (m4.md §8.2 step 21's tickOnce step 2, "distinct
// dag_id from tasks WHERE status NOT IN the terminal set"). blocked is
// deliberately NOT terminal here — a blocked task's dag may still have
// other branches making progress, and explicit blocked-marking of
// dependents is itself deferred (m4.md §8.1, "do not implement
// blocked_count here"), so ActiveDAGs must not treat it as if the whole
// dag stopped.
var terminalDAGStatuses = []TaskStatus{TaskSucceeded, TaskFailed, TaskCancelled}

// ActiveDAGs returns the distinct dag_id of every dag that has at least one
// task NOT in a terminal status (succeeded/failed/cancelled) — the
// orchestrator's per-tick "which dags still need attention" query (m4.md
// §8.1 step 21). Order is dag_id ascending, purely for deterministic
// iteration in tests and logs.
func (s *Store) ActiveDAGs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT dag_id FROM tasks WHERE status NOT IN (?, ?, ?) ORDER BY dag_id ASC`,
		string(terminalDAGStatuses[0]), string(terminalDAGStatuses[1]), string(terminalDAGStatuses[2]),
	)
	if err != nil {
		return nil, fmt.Errorf("store: listing active dags: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var dagID string
		if err := rows.Scan(&dagID); err != nil {
			return nil, fmt.Errorf("store: scanning active dag id: %w", err)
		}
		out = append(out, dagID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing active dags: %w", err)
	}
	return out, nil
}

// Dep is one raw task_deps edge: TaskID depends on DependsOn. Used by
// DetectCycle (internal/orchestrator/dag.go) and by the orchestrator's
// dependency-worktree assembly (m4.md §6.2) — both need the edge list, not
// the joined/derived views ReadyTasks computes in SQL.
type Dep struct {
	TaskID    string
	DependsOn string
}

// TaskDeps returns every task_deps edge among dagID's own tasks (joined
// through tasks so a dep row belonging to a different dag's task ids never
// leaks in, even though task_deps itself carries no dag_id column). Order
// is task_id then depends_on, ascending, for deterministic test/log output.
func (s *Store) TaskDeps(ctx context.Context, dagID string) ([]Dep, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.task_id, d.depends_on
		FROM task_deps d
		JOIN tasks t ON t.id = d.task_id
		WHERE t.dag_id = ?
		ORDER BY d.task_id ASC, d.depends_on ASC
	`, dagID)
	if err != nil {
		return nil, fmt.Errorf("store: listing task deps for dag %s: %w", dagID, err)
	}
	defer rows.Close()

	var out []Dep
	for rows.Next() {
		var d Dep
		if err := rows.Scan(&d.TaskID, &d.DependsOn); err != nil {
			return nil, fmt.Errorf("store: scanning task dep: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing task deps for dag %s: %w", dagID, err)
	}
	return out, nil
}

// SetTaskSession points taskID at the session (or the current attempt's
// session) by id. Pass "" to clear it back to NULL. Returns ErrNotFound if
// taskID doesn't exist.
func (s *Store) SetTaskSession(ctx context.Context, taskID, sessionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET session_id = ?, updated_ms = ? WHERE id = ?`,
		nullableStr(sessionID), s.clk.Now().UnixMilli(), taskID,
	)
	if err != nil {
		return fmt.Errorf("store: setting session for task %s: %w", taskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: checking task %s update: %w", taskID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddCost adds usd to both taskID's cost_usd and its dag's dag_budgets
// row, in one transaction (m4.md §3.3, §7). It only UPDATEs — it never
// inserts the dag_budgets row — so if that row is missing (CreateDAGBudget
// was never called for this dag) it returns ErrBudgetRowMissing and rolls
// back the task-side update too, rather than silently leaving the task's
// cost updated but the dag's rollup untouched.
func (s *Store) AddCost(ctx context.Context, taskID string, usd float64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var dagID string
		err := tx.QueryRowContext(ctx, `SELECT dag_id FROM tasks WHERE id = ?`, taskID).Scan(&dagID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: looking up dag for task %s: %w", taskID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET cost_usd = cost_usd + ?, updated_ms = ? WHERE id = ?`,
			usd, s.clk.Now().UnixMilli(), taskID,
		); err != nil {
			return fmt.Errorf("store: adding cost to task %s: %w", taskID, err)
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE dag_budgets SET cost_usd = cost_usd + ? WHERE dag_id = ?`,
			usd, dagID,
		)
		if err != nil {
			return fmt.Errorf("store: adding cost to dag %s budget: %w", dagID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: checking dag %s budget update: %w", dagID, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: dag %s (task %s)", ErrBudgetRowMissing, dagID, taskID)
		}
		return nil
	})
}

// DAGBudget is one dag_budgets row: the per-dag budget cap and the
// denormalized cost rollup. A nil BudgetUSD means unbounded.
type DAGBudget struct {
	DAGID     string
	BudgetUSD *float64
	CostUSD   float64
}

// GetDAGBudget returns the dag_budgets row for dagID, or (nil, nil) if no
// row exists — an ABSENT row means "unbounded", NOT an error (m4.md §7:
// CreateDAGBudget is step 24's job, so most dags have no row yet and must
// never be gated). A present row with budget_usd NULL is also unbounded.
func (s *Store) GetDAGBudget(ctx context.Context, dagID string) (*DAGBudget, error) {
	var (
		b         DAGBudget
		budgetUSD sql.NullFloat64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT dag_id, budget_usd, cost_usd FROM dag_budgets WHERE dag_id = ?`, dagID,
	).Scan(&b.DAGID, &budgetUSD, &b.CostUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: getting dag budget for %s: %w", dagID, err)
	}
	if budgetUSD.Valid {
		v := budgetUSD.Float64
		b.BudgetUSD = &v
	}
	return &b, nil
}

// createDAGBudgetTx inserts dagID's dag_budgets row against q, the
// tx-scoped counterpart of CreateDAGBudget used by SubmitDAG so the budget
// row lands in the same transaction as the tasks and deps around it.
func (s *Store) createDAGBudgetTx(ctx context.Context, q dbtx, dagID string, budgetUSD *float64) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO dag_budgets (dag_id, budget_usd, cost_usd) VALUES (?, ?, 0)`,
		dagID, nullableFloatPtr(budgetUSD),
	)
	if err != nil {
		if isPrimaryKeyConstraintError(err) {
			return fmt.Errorf("%w: %s", ErrDAGBudgetExists, dagID)
		}
		return fmt.Errorf("store: creating dag budget for %s: %w", dagID, err)
	}
	return nil
}

// CreateDAGBudget inserts dagID's dag_budgets row (cost_usd starts at 0).
// It is the SOLE writer of that row — AddCost only ever UPDATEs it and
// assumes it already exists (m4.md §3.3). Calling this twice for the same
// dagID returns ErrDAGBudgetExists rather than silently no-op'ing or
// clobbering an in-progress rollup: callers that want idempotent
// resubmission must check first, not rely on a soft second call.
func (s *Store) CreateDAGBudget(ctx context.Context, dagID string, budgetUSD *float64) error {
	return s.createDAGBudgetTx(ctx, s.db, dagID, budgetUSD)
}

// DAGSubmission is one atomic dag: its budget cap plus all task nodes and
// the dependency edges among them. SubmitDAG writes all of them in a
// single transaction so the orchestrator never observes a partially-built
// dag.
type DAGSubmission struct {
	DAGID     string
	BudgetUSD *float64           // nil = unbounded (no cap)
	Tasks     []CreateTaskParams // >= 1; each task's DAGID is forced to sub.DAGID
	Deps      []Dep              // {TaskID, DependsOn}; both endpoints must be in Tasks
}

// SubmitDAG writes sub's budget row, every task, and every dep edge in one
// transaction, so a caller building a whole dag (`corral run`, m4.md §13
// step 24) never leaves it half-built for the orchestrator's polling
// goroutine to observe — e.g. an `implement` task ready before `plan`'s
// dependency edge exists, breaking dependency order.
//
// It validates sub's shape before opening the transaction at all, so a bad
// submission never touches the database:
//   - sub.DAGID must be non-empty.
//   - sub.Tasks must be non-empty.
//   - every Deps[i].TaskID and Deps[i].DependsOn must name a task present
//     in sub.Tasks, and a task may not depend on itself.
//
// Cycle detection is deliberately NOT done here — that is
// orchestrator.DetectCycle's job. Doing it in this package would require
// store to import orchestrator, creating an import cycle (orchestrator
// already imports store).
func (s *Store) SubmitDAG(ctx context.Context, sub DAGSubmission) error {
	if sub.DAGID == "" {
		return fmt.Errorf("%w: dag id is empty", ErrInvalidDAGSubmission)
	}
	if len(sub.Tasks) == 0 {
		return fmt.Errorf("%w: dag %s has no tasks", ErrInvalidDAGSubmission, sub.DAGID)
	}

	taskIDs := make(map[string]struct{}, len(sub.Tasks))
	for _, t := range sub.Tasks {
		taskIDs[t.ID] = struct{}{}
	}
	for _, d := range sub.Deps {
		if d.TaskID == d.DependsOn {
			return fmt.Errorf("%w: dag %s: task %s depends on itself",
				ErrInvalidDAGSubmission, sub.DAGID, d.TaskID)
		}
		if _, ok := taskIDs[d.TaskID]; !ok {
			return fmt.Errorf("%w: dag %s: dep task %s is not in Tasks",
				ErrInvalidDAGSubmission, sub.DAGID, d.TaskID)
		}
		if _, ok := taskIDs[d.DependsOn]; !ok {
			return fmt.Errorf("%w: dag %s: dep target %s is not in Tasks",
				ErrInvalidDAGSubmission, sub.DAGID, d.DependsOn)
		}
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		// Budget row first: the AddCost invariant (a task's dag_budgets
		// row must already exist) then holds the instant any cost
		// arrives for a task in this dag.
		if err := s.createDAGBudgetTx(ctx, tx, sub.DAGID, sub.BudgetUSD); err != nil {
			return err
		}
		// All tasks before any dep: task_deps' ON DELETE CASCADE foreign
		// keys reference tasks(id), and the long-lived connection runs
		// with foreign_keys ON, so a dep inserted before its endpoints
		// exist would fail the FK check.
		for _, t := range sub.Tasks {
			t.DAGID = sub.DAGID // force — a caller can't mismatch a task into another dag
			if err := s.createTaskTx(ctx, tx, t); err != nil {
				return err
			}
		}
		for _, d := range sub.Deps {
			if err := s.addDepTx(ctx, tx, d.TaskID, d.DependsOn); err != nil {
				return err
			}
		}
		return nil
	})
}

const taskSelectColumns = `SELECT
	id, dag_id, name, prompt, repo, cwd, worktree, branch, model,
	permission_mode, status, attempts, max_attempts, session_id,
	cost_usd, budget_usd, created_ms, updated_ms`

func scanTask(row rowScanner) (*Task, error) {
	var (
		t                                       Task
		status                                  string
		worktree, branch, model, permMode, sess sql.NullString
		budgetUSD                               sql.NullFloat64
	)

	err := row.Scan(
		&t.ID, &t.DAGID, &t.Name, &t.Prompt, &t.Repo, &t.Cwd, &worktree, &branch,
		&model, &permMode, &status, &t.Attempts, &t.MaxAttempts, &sess,
		&t.CostUSD, &budgetUSD, &t.CreatedMs, &t.UpdatedMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scanning task: %w", err)
	}

	t.Status = TaskStatus(status)
	t.Worktree = worktree.String
	t.Branch = branch.String
	t.Model = model.String
	t.PermissionMode = permMode.String
	t.SessionID = sess.String
	if budgetUSD.Valid {
		v := budgetUSD.Float64
		t.BudgetUSD = &v
	}

	return &t, nil
}

func nullableFloatPtr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
