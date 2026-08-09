package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/danielbecerra/corral/internal/orchestrator"
	"github.com/danielbecerra/corral/internal/store"
)

// DagsDeps is everything handlers_dags.go's routes need. daemon.go
// constructs one at startup and passes it to RegisterDags.
type DagsDeps struct {
	Store *store.Store
}

// RegisterDags registers POST /v1/dags, GET /v1/dags, and GET
// /v1/dags/{id} on s (m4.md §13 step 24).
func (s *Server) RegisterDags(deps DagsDeps) {
	s.Handle("POST /v1/dags", deps.handleCreate)
	s.Handle("GET /v1/dags", deps.handleList)
	s.Handle("GET /v1/dags/{id}", deps.handleGet)
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
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	CostUSD     float64 `json:"cost_usd"`
	Worktree    string  `json:"worktree,omitempty"`
	Branch      string  `json:"branch,omitempty"`
	Model       string  `json:"model,omitempty"`
	Attempts    int     `json:"attempts"`
	MaxAttempts int     `json:"max_attempts"`
	SessionID   string  `json:"session_id,omitempty"`
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
func toDagTaskResponse(t *store.Task) dagTaskResponse {
	return dagTaskResponse{
		ID:          t.ID,
		Name:        t.Name,
		Status:      string(t.Status),
		CostUSD:     t.CostUSD,
		Worktree:    t.Worktree,
		Branch:      t.Branch,
		Model:       t.Model,
		Attempts:    t.Attempts,
		MaxAttempts: t.MaxAttempts,
		SessionID:   t.SessionID,
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
		taskResps = append(taskResps, toDagTaskResponse(t))
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

// handleList serves GET /v1/dags: every submitted dag's denormalized
// budget/cost rollup, for `corral review` with no argument.
func (d DagsDeps) handleList(w http.ResponseWriter, r *http.Request) {
	budgets, err := d.Store.ListDAGBudgets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out := make([]dagSummaryResponse, 0, len(budgets))
	for _, b := range budgets {
		out = append(out, dagSummaryResponse{DAGID: b.DAGID, BudgetUSD: b.BudgetUSD, CostUSD: b.CostUSD})
	}
	writeJSON(w, http.StatusOK, listDagsResponse{Dags: out})
}

// handleGet serves GET /v1/dags/{id}: the dag's tasks and edges plus its
// budget/cost rollup. A dag with no dag_budgets row (GetDAGBudget's (nil,
// nil) return) is reported as 404 — SubmitDAG is the sole writer of that
// row, so an absent row means this id was never submitted.
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

	detail, err := d.dagDetail(ctx, id, budget.BudgetUSD, budget.CostUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}
