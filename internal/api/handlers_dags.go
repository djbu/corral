package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/djbu/corral/internal/orchestrator"
	"github.com/djbu/corral/internal/review"
	"github.com/djbu/corral/internal/store"
)

// DagsDeps is everything handlers_dags.go's routes need. daemon.go
// constructs one at startup and passes it to RegisterDags.
type DagsDeps struct {
	Store  *store.Store
	Review *review.Service
}

// RegisterDags registers POST /v1/dags, GET /v1/dags, and GET
// /v1/dags/{id} on s (m4.md §13 step 24).
func (s *Server) RegisterDags(deps DagsDeps) {
	s.Handle("POST /v1/dags", deps.handleCreate)
	s.Handle("GET /v1/dags", deps.handleList)
	s.Handle("GET /v1/dags/{id}", deps.handleGet)
	s.Handle("GET /v1/tasks/{id}/review/preflight", deps.handleReviewPreflight)
	s.Handle("GET /v1/tasks/{id}/review/diff", deps.handleReviewDiff)
	s.Handle("POST /v1/tasks/{id}/review/release", deps.handleReviewRelease)
	s.Handle("POST /v1/tasks/{id}/review/discard", deps.handleReviewDiscard)
}

// dagNodeRequest is one entry in createDagRequest.Nodes: a task-to-be,
// identified within the request by Name (not yet an ID — handleCreate
// mints the real task id after validation).
type dagNodeRequest struct {
	Name           string   `json:"name"`
	Prompt         string   `json:"prompt"`
	Repo           string   `json:"repo"`
	Worktree       bool     `json:"worktree"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permission_mode"`
	MaxAttempts    int      `json:"max_attempts"`
	BudgetUSD      *float64 `json:"budget_usd"`
}

// dagEdgeRequest names a dependency edge by node NAME (not task id), since
// the caller doesn't know task ids until the dag is created.
type dagEdgeRequest struct {
	Task      string `json:"task"`
	DependsOn string `json:"depends_on"`
}

// createDagRequest is POST /v1/dags's body.
type createDagRequest struct {
	BudgetUSD *float64         `json:"budget_usd"`
	Nodes     []dagNodeRequest `json:"nodes"`
	Edges     []dagEdgeRequest `json:"edges"`
}

// dagTaskResponse is one task inside a dagDetailResponse. String fields use
// Go's zero value ("") plus omitempty rather than pointers, matching
// sessionResponse's style for fields that are "set or absent" rather than
// "explicitly null vs unset".
type dagTaskResponse struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	CostUSD      float64 `json:"cost_usd"`
	Worktree     string  `json:"worktree,omitempty"`
	Branch       string  `json:"branch,omitempty"`
	BaseCommit   string  `json:"base_commit,omitempty"`
	ReviewStatus string  `json:"review_status,omitempty"`
	Model        string  `json:"model,omitempty"`
	Attempts     int     `json:"attempts"`
	MaxAttempts  int     `json:"max_attempts"`
	SessionID    string  `json:"session_id,omitempty"`
	Repo         string  `json:"repo,omitempty"`
}

// dagEdgeResponse is one dependency edge in a dagDetailResponse, by task ID
// (unlike dagEdgeRequest, which is by node name).
type dagEdgeResponse struct {
	Task      string `json:"task"`
	DependsOn string `json:"depends_on"`
}

// dagDetailResponse is the body of POST /v1/dags's 201 and GET
// /v1/dags/{id}'s 200. BudgetUSD is nil when the dag is unbounded.
// CostUSD is always the denormalized dag_budgets rollup — never re-summed
// from tasks.
type dagDetailResponse struct {
	DAGID     string            `json:"dag_id"`
	BudgetUSD *float64          `json:"budget_usd"`
	CostUSD   float64           `json:"cost_usd"`
	Tasks     []dagTaskResponse `json:"tasks"`
	Edges     []dagEdgeResponse `json:"edges"`
}

// dagSummaryResponse is one entry in listDagsResponse.
type dagSummaryResponse struct {
	DAGID     string   `json:"dag_id"`
	BudgetUSD *float64 `json:"budget_usd"`
	CostUSD   float64  `json:"cost_usd"`
}

type listDagsResponse struct {
	Dags []dagSummaryResponse `json:"dags"`
}

// toDagTaskResponse converts a store.Task into wire shape.
func toDagTaskResponse(t *store.Task, reviewStatus string) dagTaskResponse {
	return dagTaskResponse{
		ID:           t.ID,
		Name:         t.Name,
		Status:       string(t.Status),
		CostUSD:      t.CostUSD,
		Worktree:     t.Worktree,
		Branch:       t.Branch,
		BaseCommit:   t.BaseCommit,
		ReviewStatus: reviewStatus,
		Model:        t.Model,
		Attempts:     t.Attempts,
		MaxAttempts:  t.MaxAttempts,
		SessionID:    t.SessionID,
		Repo:         t.Repo,
	}
}

// dagDetail assembles a dagDetailResponse for dagID from ListTasks and
// TaskDeps — budgetUSD/costUSD are passed in rather than re-fetched, since
// both handleCreate (fresh submission, cost is always 0) and handleGet
// (already has the dag_budgets row) already have them on hand.
func (d DagsDeps) dagDetail(ctx context.Context, dagID string, budgetUSD *float64, costUSD float64) (dagDetailResponse, error) {
	tasks, err := d.Store.ListTasks(ctx, dagID)
	if err != nil {
		return dagDetailResponse{}, err
	}
	deps, err := d.Store.TaskDeps(ctx, dagID)
	if err != nil {
		return dagDetailResponse{}, err
	}

	taskResps := make([]dagTaskResponse, 0, len(tasks))
	for _, t := range tasks {
		reviewStatus := ""
		if rv, reviewErr := d.Store.GetTaskReview(ctx, t.ID); reviewErr == nil {
			reviewStatus = string(rv.Status)
		} else if !errors.Is(reviewErr, store.ErrNotFound) {
			return dagDetailResponse{}, reviewErr
		}
		taskResps = append(taskResps, toDagTaskResponse(t, reviewStatus))
	}
	edgeResps := make([]dagEdgeResponse, 0, len(deps))
	for _, dp := range deps {
		edgeResps = append(edgeResps, dagEdgeResponse{Task: dp.TaskID, DependsOn: dp.DependsOn})
	}

	return dagDetailResponse{
		DAGID:     dagID,
		BudgetUSD: budgetUSD,
		CostUSD:   costUSD,
		Tasks:     taskResps,
		Edges:     edgeResps,
	}, nil
}

// handleCreate validates and atomically submits a new dag (m4.md §13 step
// 24). Node names are the request-side identifier for edges; handleCreate
// mints real task ids and translates edges through nameToID before ever
// touching the store.
func (d DagsDeps) handleCreate(w http.ResponseWriter, r *http.Request) {
	// A scope='session' token has no parent/attach point for a dynamically
	// submitted dag (m5.md §10) — there is no existing dag it could be
	// "attached" to, only a brand-new one it would then own outright — so
	// this route is denied outright for scoped tokens, before the body is
	// even decoded. Admin/absent tokens are unaffected.
	if _, isScoped, err := tokenDAGScope(r.Context(), d.Store); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	} else if isScoped {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to create a dag", nil)
		return
	}

	var req createDagRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
		return
	}

	if len(req.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "dag must have at least one node", nil)
		return
	}

	nameToID := make(map[string]string, len(req.Nodes))
	for _, n := range req.Nodes {
		if n.Name == "" {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "node name must not be empty", nil)
			return
		}
		if _, dup := nameToID[n.Name]; dup {
			writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("duplicate node name %q", n.Name), nil)
			return
		}
		if n.Prompt == "" {
			writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("node %q: prompt must not be empty", n.Name), nil)
			return
		}
		if n.Repo == "" {
			writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("node %q: repo must not be empty", n.Name), nil)
			return
		}
		nameToID[n.Name] = uuid.New().String()
	}

	for _, e := range req.Edges {
		if _, ok := nameToID[e.Task]; !ok {
			writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("edge names unknown task %q", e.Task), nil)
			return
		}
		if _, ok := nameToID[e.DependsOn]; !ok {
			writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("edge names unknown depends_on %q", e.DependsOn), nil)
			return
		}
	}

	dagID := uuid.New().String()

	tasks := make([]store.CreateTaskParams, 0, len(req.Nodes))
	stubs := make([]*store.Task, 0, len(req.Nodes))
	for _, n := range req.Nodes {
		id := nameToID[n.Name]
		// The orchestrator convention (not this handler) computes the
		// real on-disk worktree path at launch time; here we only record
		// the "an isolated worktree was requested" signal.
		worktree := ""
		if n.Worktree {
			worktree = "requested"
		}
		tasks = append(tasks, store.CreateTaskParams{
			ID:             id,
			DAGID:          dagID,
			Name:           n.Name,
			Prompt:         n.Prompt,
			Repo:           n.Repo,
			Cwd:            n.Repo,
			Worktree:       worktree,
			Branch:         "",
			Model:          n.Model,
			PermissionMode: n.PermissionMode,
			Status:         "",
			MaxAttempts:    n.MaxAttempts,
			BudgetUSD:      n.BudgetUSD,
		})
		stubs = append(stubs, &store.Task{ID: id, Name: n.Name})
	}

	deps := make([]store.Dep, 0, len(req.Edges))
	for _, e := range req.Edges {
		deps = append(deps, store.Dep{TaskID: nameToID[e.Task], DependsOn: nameToID[e.DependsOn]})
	}

	if err := orchestrator.DetectCycle(stubs, deps); err != nil {
		writeError(w, http.StatusBadRequest, CodeDAGCycle, err.Error(), nil)
		return
	}

	ctx := r.Context()
	if err := d.Store.SubmitDAG(ctx, store.DAGSubmission{
		DAGID:     dagID,
		BudgetUSD: req.BudgetUSD,
		Tasks:     tasks,
		Deps:      deps,
	}); err != nil {
		if errors.Is(err, store.ErrInvalidDAGSubmission) {
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	detail, err := d.dagDetail(ctx, dagID, req.BudgetUSD, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusCreated, detail)
}

// dagSummaries returns every submitted dag's denormalized budget/cost
// rollup as wire shape. Shared verbatim by handleList (GET /v1/dags) and
// GET /v1/dashboard (handlers_dashboard.go), since the two responses must
// carry the same dagSummaryResponse slice — extracted here rather than
// duplicated so that never drifts.
//
// A scope='session' token in ctx (step 34, m5.md §10) confines the result
// to its own dag — allowedDagID == "" (a root with no task) filters out
// everything, which falls out of the plain != comparison below with no
// special case needed, since a real DAGID is never "". Signature stays
// (ctx, st): the token itself is read from ctx, not passed as a param.
func dagSummaries(ctx context.Context, st *store.Store) ([]dagSummaryResponse, error) {
	budgets, err := st.ListDAGBudgets(ctx)
	if err != nil {
		return nil, err
	}
	allowedDagID, isScoped, err := tokenDAGScope(ctx, st)
	if err != nil {
		return nil, err
	}
	out := make([]dagSummaryResponse, 0, len(budgets))
	for _, b := range budgets {
		if isScoped && b.DAGID != allowedDagID {
			continue
		}
		out = append(out, dagSummaryResponse{DAGID: b.DAGID, BudgetUSD: b.BudgetUSD, CostUSD: b.CostUSD})
	}
	return out, nil
}

// handleList serves GET /v1/dags: every submitted dag's denormalized
// budget/cost rollup, for `corral review` with no argument.
func (d DagsDeps) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := dagSummaries(r.Context(), d.Store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, listDagsResponse{Dags: out})
}

// handleGet serves GET /v1/dags/{id}: the dag's tasks and edges plus its
// budget/cost rollup. A dag with no dag_budgets row (GetDAGBudget's (nil,
// nil) return) is reported as 404 — SubmitDAG is the sole writer of that
// row, so an absent row means this id was never submitted. The existence
// check runs before the step 34 (m5.md §10) scope check, deliberately not
// reordered: an unknown id stays 404 even for a scoped token, and only a
// real-but-foreign dag becomes 403 — the same existence-vs-scope leak
// posture handlers_sessions.go's resolveSession uses.
func (d DagsDeps) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	budget, err := d.Store.GetDAGBudget(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	if budget == nil {
		writeError(w, http.StatusNotFound, CodeDAGNotFound, fmt.Sprintf("no dag %q", id), nil)
		return
	}

	if allowedDagID, isScoped, err := tokenDAGScope(ctx, d.Store); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	} else if isScoped && id != allowedDagID {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this dag", nil)
		return
	}

	detail, err := d.dagDetail(ctx, id, budget.BudgetUSD, budget.CostUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (d DagsDeps) reviewService() *review.Service {
	if d.Review != nil {
		return d.Review
	}
	return review.New(d.Store)
}

func (d DagsDeps) authorizeReviewTask(w http.ResponseWriter, r *http.Request, mutate bool) (*store.Task, bool) {
	task, err := d.Store.GetTask(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeTaskNotFound, "task not found", nil)
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return nil, false
	}
	row, hasToken := TokenFromContext(r.Context())
	if mutate && hasToken && row.Scope != "admin" {
		writeError(w, http.StatusForbidden, CodeForbidden, "admin token required for review mutation", nil)
		return nil, false
	}
	if !mutate {
		if allowed, isScoped, scopeErr := tokenDAGScope(r.Context(), d.Store); scopeErr != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, scopeErr.Error(), nil)
			return nil, false
		} else if isScoped && allowed != task.DAGID {
			writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this task", nil)
			return nil, false
		}
	}
	return task, true
}

func (d DagsDeps) handleReviewPreflight(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.authorizeReviewTask(w, r, false); !ok {
		return
	}
	p, err := d.reviewService().Preflight(r.Context(), r.PathValue("id"), review.Strategy(r.URL.Query().Get("strategy")), r.URL.Query().Get("target"))
	if errors.Is(err, review.ErrNotReviewable) {
		writeError(w, http.StatusConflict, CodeReviewConflict, err.Error(), nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (d DagsDeps) handleReviewDiff(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.authorizeReviewTask(w, r, false); !ok {
		return
	}
	diff, err := d.reviewService().Diff(r.Context(), r.PathValue("id"), r.URL.Query().Get("full") == "1")
	if errors.Is(err, review.ErrNotReviewable) {
		writeError(w, http.StatusConflict, CodeReviewConflict, err.Error(), nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"diff": diff})
}

func decodeStrictJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeReviewError(w http.ResponseWriter, err error, p review.Preflight) {
	switch {
	case errors.Is(err, review.ErrPreflight):
		writeJSON(w, http.StatusConflict, map[string]any{"error": errorBody{Code: CodeReviewPreflight, Message: err.Error(), Details: map[string]any{}}, "preflight": p})
	case errors.Is(err, review.ErrIdentityChanged), errors.Is(err, store.ErrReviewConflict), errors.Is(err, review.ErrNotReviewable):
		writeError(w, http.StatusConflict, CodeReviewConflict, err.Error(), nil)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeTaskNotFound, "task not found", nil)
	default:
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
	}
}

func (d DagsDeps) handleReviewRelease(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.authorizeReviewTask(w, r, true); !ok {
		return
	}
	var req review.ReleaseRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
		return
	}
	result, err := d.reviewService().Release(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeReviewError(w, err, result.Preflight)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (d DagsDeps) handleReviewDiscard(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.authorizeReviewTask(w, r, true); !ok {
		return
	}
	var req review.DiscardRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
		return
	}
	result, err := d.reviewService().Discard(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeReviewError(w, err, result.Preflight)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
