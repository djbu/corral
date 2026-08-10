package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/djbu/corral/internal/checkpoint"
	"github.com/djbu/corral/internal/claude/sessions"
	"github.com/djbu/corral/internal/claude/streamjson"
	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/supervisor"
)

// Registry is the subset of *supervisor.Registry the orchestrator needs to
// spawn and kill headless attempts. Declared locally (rather than importing
// *supervisor.Registry as a concrete type), exactly like reaper's own
// Supervisor seam, so tests can fake it without a real PTY/pipe spawn.
type Registry interface {
	Spawn(ctx context.Context, spec session.Spec) (*session.Session, error)
	Kill(ctx context.Context, idOrName string, grace *time.Duration) (*session.Session, error)
}

// Store is the subset of *store.Store the orchestrator needs.
type Store interface {
	RunningTasks(ctx context.Context) ([]*store.Task, error)
	ActiveDAGs(ctx context.Context) ([]string, error)
	ListTasks(ctx context.Context, dagID string) ([]*store.Task, error)
	ReadyTasks(ctx context.Context, dagID string) ([]*store.Task, error)
	TaskDeps(ctx context.Context, dagID string) ([]store.Dep, error)
	GetTask(ctx context.Context, id string) (*store.Task, error)
	UpdateTask(ctx context.Context, id string, mutate func(*store.Task)) (*store.Task, error)
	MarkTaskSucceeded(ctx context.Context, taskID string) error
	SetTaskSession(ctx context.Context, taskID, sessionID string) error
	AddCost(ctx context.Context, taskID string, usd float64) error
	GetDAGBudget(ctx context.Context, dagID string) (*store.DAGBudget, error)

	CreateSession(ctx context.Context, p store.CreateSessionParams) (*session.Session, error)
	GetSession(ctx context.Context, id string) (*session.Session, error)
	AppendEvent(ctx context.Context, sessionID string, kind session.EventKind, dataJSON string) (*session.Event, error)
	ListEvents(ctx context.Context, sessionID string) ([]*session.Event, error)
}

// Config bundles the orchestrator's tuning knobs (design doc §8.2's "config
// trust boundary"). These are NOT exposed via .corral.toml or any other
// per-repo config in v0.4.0 — deliberately: a repo-controlled task_timeout
// or max_concurrent could weaken or disable the only lifecycle backstop a
// headless bypassPermissions session has, since the idle reaper exempts
// headless sessions entirely (§8.2). They are plain Go fields injected at
// daemon-construction time, exactly like reaper.New's idleTimeout argument
// — never re-derived from a Task row or a cloned repo's settings.
type Config struct {
	// TaskTimeout is how long a single attempt may run before the
	// orchestrator kills it (§8.2). <= 0 defaults to 30m.
	TaskTimeout time.Duration
	// MaxConcurrent caps in-flight attempts across ALL active dags at
	// once. <= 0 defaults to 4.
	MaxConcurrent int
	// PollInterval is the tick loop's ticker period. <= 0 defaults to 2s.
	PollInterval time.Duration
	// StateDir is the daemon's state directory (§6.2): worktrees are
	// created under <StateDir>/worktrees/<dagID>/<name>-a<N>.
	StateDir string
	// ClaudeHome is <claudeHome> for resolving an orphan's transcript path
	// during recovery (§9). Injected at daemon construction, not TOML-exposed
	// (config-trust-boundary posture, §8.2). "" disables the harvest check —
	// classifyOrphan then always respawns (err-toward-respawn).
	ClaudeHome string
	// Templates is the daemon-start, operator-owned registry. Task rows only
	// carry a selected name; the trusted registry supplies the env allowlist.
	Templates map[string]config.Template
}

func (c Config) withDefaults() Config {
	if c.TaskTimeout <= 0 {
		c.TaskTimeout = 30 * time.Minute
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	return c
}

// inFlightAttempt is the loop-local record of one attempt this process
// itself spawned: which session is running it, and when it must be killed
// for exceeding cfg.TaskTimeout.
type inFlightAttempt struct {
	sessionID string
	deadline  time.Time
}

// Orchestrator owns the background tick-loop goroutine that drives task
// DAGs to completion (design doc §8, step 21). Modeled directly on
// internal/reaper.Reaper: a single goroutine, started by Start and joined
// by Close, doing all of its work inside tickOnce so tests can drive the
// policy directly without the ticker.
//
// inFlight and nextAttemptAt are loop-local bookkeeping (§8.1: "single
// goroutine, poll-driven; no per-task goroutines, no shared mutable state
// across goroutines") — both are touched only from tickOnce, which the
// run loop never calls concurrently with itself, so neither needs a mutex
// even though tests call tickOnce directly rather than through the ticker.
type Orchestrator struct {
	reg       Registry
	store     Store
	clk       clock.Clock
	log       *slog.Logger
	cfg       Config
	claudeBin string

	cancel context.CancelFunc
	done   chan struct{}

	inFlight      map[string]inFlightAttempt // task ID -> attempt this process spawned
	nextAttemptAt map[string]time.Time       // task ID -> earliest time a retry may launch
	// dagCursor is the next candidate in the deterministic ActiveDAGs order.
	// It is an in-memory fairness hint only: durable task states remain the
	// source of truth across a daemon restart.
	dagCursor int
}

// New builds an Orchestrator. log defaults to slog.Default(). claudeBin
// must already be resolved to an absolute path (mirroring the pattern
// internal/api/handlers_sessions.go uses: config.LoadSession + LookPath if
// not absolute) — the orchestrator does not re-resolve config trust
// decisions itself (see Config's doc comment and Spec construction below).
func New(reg Registry, st Store, clk clock.Clock, log *slog.Logger, cfg Config, claudeBin string) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		reg:           reg,
		store:         st,
		clk:           clk,
		log:           log,
		cfg:           cfg.withDefaults(),
		claudeBin:     claudeBin,
		inFlight:      make(map[string]inFlightAttempt),
		nextAttemptAt: make(map[string]time.Time),
	}
}

// Start launches the tick loop.
func (o *Orchestrator) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	o.done = make(chan struct{})
	go o.run(ctx)
	o.log.Info("orchestrator started",
		"poll_interval", o.cfg.PollInterval,
		"task_timeout", o.cfg.TaskTimeout,
		"max_concurrent", o.cfg.MaxConcurrent,
	)
}

// Close stops the tick loop and waits for the goroutine to exit. Safe to
// call when Start was never called.
func (o *Orchestrator) Close() {
	if o.cancel == nil {
		return
	}
	o.cancel()
	<-o.done
}

func (o *Orchestrator) run(ctx context.Context) {
	defer close(o.done)
	ticker := o.clk.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			o.tickOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// tickOnce is the orchestrator's whole policy, run synchronously so tests
// can drive it directly without the ticker/goroutine (design doc §8.1
// step 21):
//  1. reconcile orphaned "running" tasks left behind by a restart
//  2. discover which dags are still active, and skip cyclic ones
//  3. launch ready tasks up to the concurrency cap
//  4. poll in-flight attempts for a terminal outcome
//  5. kill in-flight attempts that exceeded their per-task timeout
func (o *Orchestrator) tickOnce(ctx context.Context) {
	o.reconcileOrphans(ctx)

	dagIDs, err := o.store.ActiveDAGs(ctx)
	if err != nil {
		o.log.Error("orchestrator: listing active dags", "err", err)
		return
	}

	o.launchReady(ctx, dagIDs)
	o.pollInFlight(ctx)
	o.enforceTimeouts(ctx)
}

// orphanAction is classifyOrphan's verdict for one orphaned "running" task.
type orphanAction int

const (
	orphanRespawn orphanAction = iota
	orphanHarvest
)

// classifyOrphan decides what to do with a task the store says is
// "running" but that this process has no in-flight record for (i.e. it was
// left running by a daemon restart, design doc §9).
//
// It inspects the orphaned attempt's transcript to see whether the work
// actually landed before corral lost the pipe: if the last main-thread
// record is a completed assistant turn (stop_reason=="end_turn"), the
// child finished its work even though corral never observed the `result`
// line, so the task is harvested as succeeded rather than re-run (no
// double-run). Otherwise (mid-turn crash) it respawns a fresh attempt —
// fresh session, fresh worktree, never --resume (§9: a resumed transcript
// would reference edits that don't exist on the fresh worktree's disk).
//
// Err toward orphanRespawn on EVERY uncertainty (§9 — load-bearing,
// nil-trap severity: a wrong harvest marks unfinished work succeeded and
// releases dependents on a lie, whereas a wrong respawn merely re-runs
// completed work). orphanHarvest requires a positive, fully-resolved
// completed-end_turn verdict and nothing less.
func (o *Orchestrator) classifyOrphan(ctx context.Context, task *store.Task) orphanAction {
	if o.cfg.ClaudeHome == "" {
		return orphanRespawn
	}

	sess, err := o.store.GetSession(ctx, task.SessionID)
	if err != nil {
		o.log.Debug("orchestrator: reconciliation: loading orphan's session for transcript check, defaulting to respawn", "task_id", task.ID, "session_id", task.SessionID, "err", err)
		return orphanRespawn
	}
	if sess == nil || sess.ClaudeSessionID == "" {
		o.log.Debug("orchestrator: reconciliation: orphan's session missing claude_session_id, defaulting to respawn", "task_id", task.ID, "session_id", task.SessionID)
		return orphanRespawn
	}

	path := sessions.TranscriptPath(o.cfg.ClaudeHome, sess.Cwd, sess.ClaudeSessionID)
	done, err := checkpoint.LastRecordIsCompletedTurn(path)
	if err != nil {
		o.log.Info("orchestrator: reconciliation: reading orphan's transcript, defaulting to respawn", "task_id", task.ID, "path", path, "err", err)
		return orphanRespawn
	}
	if done {
		return orphanHarvest
	}
	return orphanRespawn
}

// reconcileOrphans finds every "running" task this process doesn't have an
// in-flight record for and applies classifyOrphan's verdict to each.
//
// Two-phase by construction (design doc §9): every orphan is classified
// into orphans first, and ONLY THEN acted on in a second loop. Classifying
// and acting in a single pass would let an earlier orphan's respawn (which
// creates a brand-new task attempt) land in the store before a later
// orphan in the same dag is classified — corrupting a future harvest-vs-
// respawn decision (step 23) that needs to see the dag's pre-respawn
// state.
func (o *Orchestrator) reconcileOrphans(ctx context.Context) {
	running, err := o.store.RunningTasks(ctx)
	if err != nil {
		o.log.Error("orchestrator: listing running tasks for reconciliation", "err", err)
		return
	}

	var orphans []*store.Task
	for _, t := range running {
		if _, ok := o.inFlight[t.ID]; ok {
			continue // genuinely in-flight in this process, not an orphan
		}
		orphans = append(orphans, t)
	}

	verdicts := make([]orphanAction, len(orphans))
	for i, t := range orphans {
		verdicts[i] = o.classifyOrphan(ctx, t)
	}

	for i, t := range orphans {
		switch verdicts[i] {
		case orphanRespawn:
			o.log.Info("orchestrator: reconciliation: orphaned attempt, respawning",
				"task_id", t.ID, "session_id", t.SessionID)
			if err := o.applyOutcome(ctx, t, false, false); err != nil {
				o.log.Error("orchestrator: reconciliation: applying orphan outcome", "task_id", t.ID, "err", err)
			}
		case orphanHarvest:
			o.log.Info("orchestrator: reconciliation: orphaned attempt completed its turn, harvesting",
				"task_id", t.ID, "session_id", t.SessionID)
			if err := o.applyOutcome(ctx, t, true, false); err != nil {
				o.log.Error("orchestrator: reconciliation: applying orphan harvest outcome", "task_id", t.ID, "err", err)
			}
		}
	}
}

// launchReady chooses one eligible task at a time, rotating through active
// DAGs after every launch. A large DAG therefore cannot fill every headless
// slot simply because it sorts before smaller DAGs; retries, dependency and
// budget gates are still evaluated before a task is admitted.
func (o *Orchestrator) launchReady(ctx context.Context, dagIDs []string) {
	if len(dagIDs) == 0 {
		return
	}
	for ctx.Err() == nil && len(o.inFlight) < o.cfg.MaxConcurrent {
		launched := false
		for offset := 0; offset < len(dagIDs); offset++ {
			index := (o.dagCursor + offset) % len(dagIDs)
			if !o.launchOneReady(ctx, dagIDs[index]) {
				continue
			}
			o.dagCursor = (index + 1) % len(dagIDs)
			launched = true
			break
		}
		if !launched {
			return
		}
	}
}

// launchOneReady applies every pre-existing DAG gate and launches at most one
// task. It returns true only once a task has entered inFlight, which makes it
// the unit of M9's round-robin scheduler.
func (o *Orchestrator) launchOneReady(ctx context.Context, dagID string) bool {
	tasks, err := o.store.ListTasks(ctx, dagID)
	if err != nil {
		o.log.Error("orchestrator: listing tasks", "dag_id", dagID, "err", err)
		return false
	}
	deps, err := o.store.TaskDeps(ctx, dagID)
	if err != nil {
		o.log.Error("orchestrator: listing task deps", "dag_id", dagID, "err", err)
		return false
	}
	if err := DetectCycle(tasks, deps); err != nil {
		o.log.Error("orchestrator: cyclic dag, skipping", "dag_id", dagID, "err", err)
		return false
	}
	over, err := o.dagOverBudget(ctx, dagID)
	if err != nil {
		o.log.Error("orchestrator: checking dag budget", "dag_id", dagID, "err", err)
		return false
	}
	if over {
		o.cancelRemaining(ctx, tasks)
		return false
	}
	ready, err := o.store.ReadyTasks(ctx, dagID)
	if err != nil {
		o.log.Error("orchestrator: listing ready tasks", "dag_id", dagID, "err", err)
		return false
	}
	byID := make(map[string]*store.Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	for _, task := range ready {
		if _, ok := o.inFlight[task.ID]; ok {
			continue
		}
		if next, wait := o.nextAttemptAt[task.ID]; wait && o.clk.Now().Before(next) {
			continue
		}
		if err := o.launchTask(ctx, task, deps, byID); err != nil {
			o.log.Error("orchestrator: launching task", "task_id", task.ID, "err", err)
			return false
		}
		return true
	}
	return false
}

// launchTask resolves task's worktree (if requested), builds its headless
// session.Spec, creates the session row, links it to the task, and spawns
// it — mirroring internal/api/handlers_sessions.go's handleCreate spawn
// sequence (CreateSession -> AppendEvent(session.created) -> Spec ->
// Registry.Spawn).
func (o *Orchestrator) launchTask(ctx context.Context, task *store.Task, deps []store.Dep, byID map[string]*store.Task) error {
	attemptN := task.Attempts + 1
	name := fmt.Sprintf("%s-a%d", task.Name, attemptN)

	// cwd defaults to the task's current cwd column, which m4.md §3.2's
	// schema comment documents as "worktree path once created, else repo"
	// — i.e. already correct for a task that never requested a worktree.
	cwd := task.Cwd
	newWorktree, newBranch, baseCommit := "", "", ""
	if task.Worktree != "" {
		// A non-empty task.Worktree is this task's "a worktree was
		// requested" signal (§3.2: "worktree path if --worktree, else
		// NULL"). Submission (step 24, not yet built) is what sets that
		// signal; step 21 treats ANY non-empty value there as the intent,
		// and is the sole owner of turning it into a real, attempt-
		// numbered, on-disk worktree — see DEVIATIONS in the step 21
		// report for why the exact pre-resolution sentinel isn't nailed
		// down yet. Every attempt gets its OWN fresh worktree/branch
		// (never reused across retries), matching the worktree-per-task-
		// isolation naming convention. Including the DAG id makes branch
		// ownership unambiguous even when several DAGs reuse the same task
		// name; the worktree path was already namespaced this way.
		newWorktree = filepath.Join(o.cfg.StateDir, "worktrees", task.DAGID, name)
		newBranch = fmt.Sprintf("corral/task/%s/%s", task.DAGID, name)
		var err error
		baseCommit, err = git.RevParse(ctx, task.Repo, "HEAD")
		if err != nil {
			return fmt.Errorf("resolving worktree base: %w", err)
		}
		if err := git.AddWorktreeAt(ctx, task.Repo, newWorktree, newBranch, baseCommit); err != nil {
			return fmt.Errorf("adding worktree: %w", err)
		}
		cwd = newWorktree
	}

	depWorktrees, err := buildDepWorktreesJSON(task, deps, byID)
	if err != nil {
		return fmt.Errorf("building dep worktrees: %w", err)
	}

	id := uuid.New().String()

	if _, err := o.store.CreateSession(ctx, store.CreateSessionParams{
		ID:           id,
		Name:         name,
		Mode:         session.ModeHeadless,
		Cwd:          cwd,
		ClaudeBin:    o.claudeBin,
		Model:        task.Model,
		SettingsPath: "", // filled in by Registry.Spawn once settings.Pin runs
		DesiredState: session.DesiredRunning,
		Status:       session.StatusStarting,
	}); err != nil {
		return fmt.Errorf("creating session row: %w", err)
	}
	if _, err := o.store.AppendEvent(ctx, id, session.EventSessionCreated, "{}"); err != nil {
		// Non-fatal, matching handlers_sessions.go's handleCreate: the
		// session row exists and spawning still proceeds.
		o.log.Warn("orchestrator: appending session.created event", "session_id", id, "err", err)
	}

	if err := o.store.SetTaskSession(ctx, task.ID, id); err != nil {
		return fmt.Errorf("linking task to session: %w", err)
	}
	if _, err := o.store.UpdateTask(ctx, task.ID, func(t *store.Task) {
		t.Status = store.TaskRunning
		t.Attempts = attemptN
		t.Cwd = cwd
		if newWorktree != "" {
			t.Worktree = newWorktree
			t.Branch = newBranch
			t.BaseCommit = baseCommit
		}
	}); err != nil {
		return fmt.Errorf("marking task running: %w", err)
	}

	spec := session.Spec{
		ID:             id,
		Name:           name,
		Mode:           session.ModeHeadless,
		Cwd:            cwd,
		ClaudeBin:      o.claudeBin,
		Model:          task.Model,
		Prompt:         task.Prompt,
		PermissionMode: task.PermissionMode,
		DepWorktrees:   depWorktrees,
	}
	if task.Template != "" {
		if template, ok := o.cfg.Templates[task.Template]; ok {
			spec.EnvPassthrough = template.EnvPassthrough
		} else {
			return fmt.Errorf("task %s references unavailable template %q", task.ID, task.Template)
		}
	}
	if _, err := o.reg.Spawn(ctx, spec); err != nil {
		return fmt.Errorf("spawning: %w", err)
	}

	o.inFlight[task.ID] = inFlightAttempt{
		sessionID: id,
		deadline:  o.clk.Now().Add(o.cfg.TaskTimeout),
	}
	delete(o.nextAttemptAt, task.ID)
	return nil
}

// depWorktreeEntry is one element of the CORRAL_DEP_WORKTREES JSON array
// (design doc §6.2).
type depWorktreeEntry struct {
	Name     string `json:"name"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
}

// buildDepWorktreesJSON assembles task's session.Spec.DepWorktrees payload:
// one entry per dependency task that has a non-empty RESOLVED worktree. A
// dependency with no worktree at all (ran directly in the repo) is simply
// omitted, not zero-valued — the child only ever learns about worktrees
// that actually exist on disk. Returns "" when there is nothing to report,
// which Spec.DepWorktrees / BuildEnv both treat as "omit the variable
// entirely" (§6.2).
func buildDepWorktreesJSON(task *store.Task, deps []store.Dep, byID map[string]*store.Task) (string, error) {
	var entries []depWorktreeEntry
	for _, d := range deps {
		if d.TaskID != task.ID {
			continue
		}
		dep, ok := byID[d.DependsOn]
		if !ok || dep.Worktree == "" {
			continue
		}
		entries = append(entries, depWorktreeEntry{Name: dep.Name, Worktree: dep.Worktree, Branch: dep.Branch})
	}
	if len(entries) == 0 {
		return "", nil
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// pollInFlight checks every attempt this process is tracking for a
// terminal session status, and applies its outcome once found.
func (o *Orchestrator) pollInFlight(ctx context.Context) {
	for taskID, attempt := range o.inFlight {
		if ctx.Err() != nil {
			return
		}

		sess, err := o.store.GetSession(ctx, attempt.sessionID)
		if err != nil {
			o.log.Error("orchestrator: polling session", "task_id", taskID, "session_id", attempt.sessionID, "err", err)
			continue
		}
		if sess.Status != session.StatusExited && sess.Status != session.StatusFailed {
			continue
		}

		task, err := o.store.GetTask(ctx, taskID)
		if err != nil {
			o.log.Error("orchestrator: loading task for outcome", "task_id", taskID, "err", err)
			continue
		}

		success, costUSD, captured, err := o.outcomeForSession(ctx, attempt.sessionID)
		if err != nil {
			o.log.Error("orchestrator: computing outcome", "task_id", taskID, "session_id", attempt.sessionID, "err", err)
			continue
		}
		overBudget := false
		if captured {
			if err := o.store.AddCost(ctx, taskID, costUSD); err != nil {
				// Logged, not fatal (see step 21 report's DEVIATIONS): a
				// missing dag_budgets row is a submission-side bug
				// (CreateDAGBudget is step 24's job), not a reason to
				// refuse to record this attempt's own terminal outcome.
				o.log.Warn("orchestrator: recording cost", "task_id", taskID, "err", err)
			}
			// overBudget is only ever computed off cost that was actually
			// recorded (§7): when !captured (a mid-turn crash, no result
			// line, no cost to add), overBudget stays false — the
			// documented "known under-count" rather than a guess.
			overBudget = o.overBudget(ctx, taskID)
			if overBudget {
				o.recordBudgetExceeded(ctx, taskID, attempt.sessionID)
			}
		}

		delete(o.inFlight, taskID)
		if err := o.applyOutcome(ctx, task, success, overBudget); err != nil {
			o.log.Error("orchestrator: applying outcome", "task_id", taskID, "err", err)
		}
	}
}

// outcomeForSession applies design doc §8.1's attempt-outcome rule to a
// terminal headless session: scan its events for the LAST
// session.EventSessionResult record, decode it into a *streamjson.Result,
// and defer to supervisor.HeadlessOutcome for the actual success/failure
// mapping — never a second, invented mapping (§8.1: "reuse
// supervisor.HeadlessOutcome ... do NOT invent a second success/failure
// mapping in run.go").
//
// THE NIL TRAP (§8.1, the highest-severity bug named for step 21): when no
// session.result event is found at all, result MUST stay nil — never
// &streamjson.Result{IsError:false} — so HeadlessOutcome sees "no result
// captured" (failure) rather than "result present and successful". A
// zero-valued Result would silently convert every mid-turn crash (killed
// by timeout, crashed before writing a result line, pipe closed early)
// into a success.
func (o *Orchestrator) outcomeForSession(ctx context.Context, sessionID string) (success bool, costUSD float64, captured bool, err error) {
	events, err := o.store.ListEvents(ctx, sessionID)
	if err != nil {
		return false, 0, false, fmt.Errorf("listing events for %s: %w", sessionID, err)
	}

	var result *streamjson.Result // stays nil unless a session.result event is found below
	for _, ev := range events {
		if ev.Kind != session.EventSessionResult {
			continue
		}
		var data struct {
			IsError      bool    `json:"is_error"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			StopReason   string  `json:"stop_reason"`
			NumTurns     int     `json:"num_turns"`
		}
		if err := json.Unmarshal([]byte(ev.DataJSON), &data); err != nil {
			o.log.Warn("orchestrator: decoding session.result event", "session_id", sessionID, "err", err)
			continue
		}
		// Keep scanning to the end rather than break: a headless attempt
		// emits at most one session.result event in practice, but the
		// design doc's rule is phrased as "the LAST such event", so this
		// matches it exactly rather than assuming uniqueness.
		result = &streamjson.Result{
			IsError:      data.IsError,
			TotalCostUSD: data.TotalCostUSD,
			StopReason:   data.StopReason,
			NumTurns:     data.NumTurns,
		}
	}

	success = supervisor.HeadlessOutcome(result)
	if result == nil {
		return success, 0, false, nil
	}
	return success, result.TotalCostUSD, true, nil
}

// applyOutcome persists task's terminal-or-retry state (§8.1's post-
// outcome bookkeeping, §7's budget enforcement). It is also reconciliation's
// respawn path (§9): an orphaned attempt is applied here with
// success=false, overBudget=false, exactly like a normal terminal failure,
// so it flows through the same max-attempts/backoff decision as any other
// failure.
//
// This is the SINGLE terminal-disposition site (same "no second mapping"
// discipline as HeadlessOutcome, §8.1) — overBudget is threaded in as a
// param rather than re-decided here so there is exactly one place that
// turns (success, overBudget, attempts) into a status.
//
// Governing invariant (§7): budget never rewrites the outcome of a turn
// that already happened; it only forbids FUTURE spend. A successful
// attempt stays succeeded even if it pushed cost over budget — the paid-for
// work is never discarded, and ReadyTasks keys dependents on
// dep.status='succeeded'. A failing attempt while over budget skips the
// retry path entirely: retrying would spend more, which is exactly what
// the budget forbids.
func (o *Orchestrator) applyOutcome(ctx context.Context, task *store.Task, success, overBudget bool) error {
	if success {
		err := o.store.MarkTaskSucceeded(ctx, task.ID)
		delete(o.nextAttemptAt, task.ID)
		return err
	}

	if overBudget || task.Attempts >= task.MaxAttempts {
		_, err := o.store.UpdateTask(ctx, task.ID, func(t *store.Task) {
			t.Status = store.TaskFailed
		})
		delete(o.nextAttemptAt, task.ID)
		return err
	}

	o.nextAttemptAt[task.ID] = o.clk.Now().Add(retryBackoff(task.Attempts))
	_, err := o.store.UpdateTask(ctx, task.ID, func(t *store.Task) {
		t.Status = store.TaskPending
	})
	return err
}

// retryBackoff is design doc §8.1's schedule: min(60s, 2s * 2^(attempts-1)).
// attempts is the number of attempts already made (i.e. the one that just
// failed), so the first retry (attempts==1) waits 2s, the second
// (attempts==2) waits 4s, and so on, capped at 60s.
func retryBackoff(attempts int) time.Duration {
	d := 2 * time.Second
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= 60*time.Second {
			return 60 * time.Second
		}
	}
	return d
}

// overBudget reports whether taskID's task-level or dag-level budget is
// already at or over its cap (§7: "enforcement purely post-hoc — gate only
// when cost_usd already at/over budget"; est=0, no cost estimator). It
// reloads the task fresh via GetTask rather than trusting the caller's
// (possibly pre-AddCost) copy, since pollInFlight calls this immediately
// after AddCost and needs the just-recorded cost. A missing task-level
// BudgetUSD or a missing/nil-budget dag_budgets row both mean unbounded —
// never gate — matching GetDAGBudget's own doc contract. Any lookup error
// is logged and treated as "not over budget" rather than blocking progress
// on a transient store error.
func (o *Orchestrator) overBudget(ctx context.Context, taskID string) bool {
	t, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		o.log.Error("orchestrator: reloading task for budget check", "task_id", taskID, "err", err)
		return false
	}
	if t.BudgetUSD != nil && t.CostUSD >= *t.BudgetUSD {
		return true
	}
	b, err := o.store.GetDAGBudget(ctx, t.DAGID)
	if err != nil {
		o.log.Error("orchestrator: checking dag budget", "dag_id", t.DAGID, "task_id", taskID, "err", err)
		return false
	}
	return b != nil && b.BudgetUSD != nil && b.CostUSD >= *b.BudgetUSD
}

// dagOverBudget reports whether dagID's dag-level budget is already at or
// over its cap. Unlike overBudget, this never looks at a per-task
// BudgetUSD — it is launchReady's per-dag gate (§7's "path (a): starting a
// new ready task"), which has no single task in hand yet. No row, or a
// present row with a nil BudgetUSD, both mean unbounded (never gated).
func (o *Orchestrator) dagOverBudget(ctx context.Context, dagID string) (bool, error) {
	b, err := o.store.GetDAGBudget(ctx, dagID)
	if err != nil {
		return false, err
	}
	if b == nil || b.BudgetUSD == nil {
		return false, nil
	}
	return b.CostUSD >= *b.BudgetUSD, nil
}

// recordBudgetExceeded appends the durable EventTaskBudgetExceeded event
// (§7) to the tripping attempt's own session — the exact attempt whose
// AddCost pushed cost at/over the cap — carrying both task- and dag-level
// cost/budget figures so the record is self-contained without a join.
// Warn-not-fatal on any error: a failure to record the reason must never
// block applyOutcome from persisting the (already-decided) terminal status.
func (o *Orchestrator) recordBudgetExceeded(ctx context.Context, taskID, sessionID string) {
	t, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		o.log.Warn("orchestrator: reloading task for budget_exceeded event", "task_id", taskID, "err", err)
		return
	}
	b, err := o.store.GetDAGBudget(ctx, t.DAGID)
	if err != nil {
		o.log.Warn("orchestrator: loading dag budget for budget_exceeded event", "dag_id", t.DAGID, "task_id", taskID, "err", err)
		return
	}

	payload := struct {
		TaskID        string   `json:"task_id"`
		DAGID         string   `json:"dag_id"`
		TaskCostUSD   float64  `json:"task_cost_usd"`
		TaskBudgetUSD *float64 `json:"task_budget_usd,omitempty"`
		DAGCostUSD    float64  `json:"dag_cost_usd"`
		DAGBudgetUSD  *float64 `json:"dag_budget_usd"`
	}{
		TaskID:        t.ID,
		DAGID:         t.DAGID,
		TaskCostUSD:   t.CostUSD,
		TaskBudgetUSD: t.BudgetUSD,
	}
	if b != nil {
		payload.DAGCostUSD = b.CostUSD
		payload.DAGBudgetUSD = b.BudgetUSD
	}

	data, err := json.Marshal(payload)
	if err != nil {
		o.log.Warn("orchestrator: encoding budget_exceeded event", "task_id", taskID, "err", err)
		return
	}
	if _, err := o.store.AppendEvent(ctx, sessionID, session.EventTaskBudgetExceeded, string(data)); err != nil {
		o.log.Warn("orchestrator: appending budget_exceeded event", "task_id", taskID, "session_id", sessionID, "err", err)
	}
}

// cancelRemaining marks every non-terminal, non-running task in tasks as
// cancelled (§7 "no perpetual stall"): pending/ready/blocked tasks in an
// over-budget dag have no future — launchReady will never start them, and
// leaving them pending forever would keep the dag out of terminalDAGStatuses
// indefinitely. Running tasks are deliberately left alone: they finish and
// record their spend normally (§7 "in-flight siblings finish current
// turn"), reaching the dag's clean terminal state on their own via the
// normal pollInFlight/applyOutcome path.
func (o *Orchestrator) cancelRemaining(ctx context.Context, tasks []*store.Task) {
	for _, t := range tasks {
		switch t.Status {
		case store.TaskPending, store.TaskReady, store.TaskBlocked:
		default:
			continue
		}
		o.log.Warn("orchestrator: dag over budget, cancelling task", "task_id", t.ID, "dag_id", t.DAGID)
		if _, err := o.store.UpdateTask(ctx, t.ID, func(task *store.Task) {
			task.Status = store.TaskCancelled
		}); err != nil {
			o.log.Error("orchestrator: cancelling over-budget task", "task_id", t.ID, "err", err)
			continue
		}
		delete(o.nextAttemptAt, t.ID)
	}
}

// enforceTimeouts kills any in-flight attempt that has exceeded
// cfg.TaskTimeout (§8.2). It deliberately does NOT remove the task from
// inFlight or apply an outcome itself: the killed session reaches a
// terminal store status with no session.result event on its own, and the
// NEXT tick's pollInFlight observes that terminal status and applies the
// standard nil-result -> failure -> retry rule (§8.1) — there is no
// timeout-specific outcome branch, by design, so a killed attempt is
// indistinguishable from any other mid-turn crash.
func (o *Orchestrator) enforceTimeouts(ctx context.Context) {
	now := o.clk.Now()
	for taskID, attempt := range o.inFlight {
		if now.Before(attempt.deadline) {
			continue
		}
		o.log.Warn("orchestrator: task timeout, killing",
			"task_id", taskID, "session_id", attempt.sessionID, "timeout", o.cfg.TaskTimeout)
		if _, err := o.reg.Kill(ctx, attempt.sessionID, nil); err != nil {
			o.log.Error("orchestrator: killing timed-out session", "task_id", taskID, "session_id", attempt.sessionID, "err", err)
		}
	}
}
