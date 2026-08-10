package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// newDagsTestServer mirrors newTestServer (handlers_meta_test.go) for the
// dags routes.
func newDagsTestServer(t *testing.T, deps DagsDeps) *Server {
	t.Helper()
	s := New()
	s.RegisterDags(deps)
	return s
}

func newDagsTestDeps(t *testing.T) DagsDeps {
	t.Helper()
	return DagsDeps{Store: openMetaTestStore(t)}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// --- 1. POST /v1/dags happy path ----------------------------------------

func TestHandleCreateDag_HappyPath(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	budget := 5.0
	reqBody := createDagRequest{
		BudgetUSD: &budget,
		Nodes: []dagNodeRequest{
			{Name: "plan", Prompt: "plan it", Repo: "/repo"},
			{Name: "implement", Prompt: "implement it", Repo: "/repo", Worktree: true},
			{Name: "review", Prompt: "review it", Repo: "/repo"},
		},
		Edges: []dagEdgeRequest{
			{Task: "implement", DependsOn: "plan"},
			{Task: "review", DependsOn: "implement"},
		},
	}

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", mustMarshal(t, reqBody))
	assertAPIVersionHeader(t, rec)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	var got dagDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if got.DAGID == "" {
		t.Fatal("dag_id is empty, want non-empty")
	}
	if got.BudgetUSD == nil || *got.BudgetUSD != 5.0 {
		t.Fatalf("budget_usd = %v, want 5.0", got.BudgetUSD)
	}
	if got.CostUSD != 0 {
		t.Fatalf("cost_usd = %v, want 0", got.CostUSD)
	}
	if len(got.Tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3: %+v", len(got.Tasks), got.Tasks)
	}
	byName := make(map[string]dagTaskResponse, len(got.Tasks))
	for _, task := range got.Tasks {
		byName[task.Name] = task
		if task.Status != "pending" {
			t.Errorf("task %s status = %q, want pending", task.Name, task.Status)
		}
		if task.ID == "" {
			t.Errorf("task %s has empty id", task.Name)
		}
		if task.MaxAttempts != 1 {
			t.Errorf("task %s max_attempts = %d, want 1 (store default)", task.Name, task.MaxAttempts)
		}
	}
	// "implement" requested a worktree at creation time; the store records
	// this as the literal marker "requested" (not a real path yet — the
	// orchestrator overwrites it at launch). Pinning this here so a change
	// in wire behavior is a deliberate, reviewed decision, not a silent
	// drift: today's GET on a not-yet-launched dag exposes this marker
	// string as-is in the "worktree" field.
	if byName["implement"].Worktree != "requested" {
		t.Errorf("implement.worktree = %q, want %q", byName["implement"].Worktree, "requested")
	}
	if byName["plan"].Worktree != "" {
		t.Errorf("plan.worktree = %q, want empty (no worktree requested)", byName["plan"].Worktree)
	}
	if len(got.Edges) != 2 {
		t.Fatalf("len(edges) = %d, want 2: %+v", len(got.Edges), got.Edges)
	}
	// Assert edges translate name->id in the right direction (Task depends
	// on DependsOn), not transposed.
	wantEdges := map[string]string{
		byName["implement"].ID: byName["plan"].ID,
		byName["review"].ID:    byName["implement"].ID,
	}
	for _, e := range got.Edges {
		wantDep, ok := wantEdges[e.Task]
		if !ok {
			t.Errorf("edge task id %q not expected", e.Task)
			continue
		}
		if e.DependsOn != wantDep {
			t.Errorf("edge task=%s depends_on=%s, want depends_on=%s", e.Task, e.DependsOn, wantDep)
		}
		delete(wantEdges, e.Task)
	}
	if len(wantEdges) != 0 {
		t.Errorf("missing expected edges: %+v", wantEdges)
	}

	// GET /v1/dags/{id} should return the same dag.
	getRec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/dags/"+got.DAGID, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body=%s", getRec.Code, getRec.Body.String())
	}
	var getGot dagDetailResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &getGot); err != nil {
		t.Fatalf("Unmarshal GET: %v, body=%s", err, getRec.Body.String())
	}
	if getGot.DAGID != got.DAGID {
		t.Fatalf("GET dag_id = %q, want %q", getGot.DAGID, got.DAGID)
	}
	if len(getGot.Tasks) != 3 || len(getGot.Edges) != 2 {
		t.Fatalf("GET tasks/edges = %d/%d, want 3/2", len(getGot.Tasks), len(getGot.Edges))
	}
	if getGot.CostUSD != 0 {
		t.Fatalf("GET cost_usd = %v, want 0", getGot.CostUSD)
	}
	if getGot.BudgetUSD == nil || *getGot.BudgetUSD != 5.0 {
		t.Fatalf("GET budget_usd = %v, want 5.0", getGot.BudgetUSD)
	}

	// GET /v1/dags should list this dag.
	listRec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/dags", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("LIST status = %d, want 200, body=%s", listRec.Code, listRec.Body.String())
	}
	var listGot listDagsResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &listGot); err != nil {
		t.Fatalf("Unmarshal LIST: %v, body=%s", err, listRec.Body.String())
	}
	found := false
	for _, dag := range listGot.Dags {
		if dag.DAGID == got.DAGID {
			found = true
			if dag.BudgetUSD == nil || *dag.BudgetUSD != 5.0 {
				t.Fatalf("LIST budget_usd = %v, want 5.0", dag.BudgetUSD)
			}
			if dag.CostUSD != 0 {
				t.Fatalf("LIST cost_usd = %v, want 0", dag.CostUSD)
			}
		}
	}
	if !found {
		t.Fatalf("LIST did not contain created dag %q: %+v", got.DAGID, listGot.Dags)
	}
}

func TestHandleCreateDag_RejectsCapacityBeforePersistence(t *testing.T) {
	deps := newDagsTestDeps(t)
	deps.MaxPendingDAGTasks = 1
	srv := newDagsTestServer(t, deps)
	reqBody := createDagRequest{Nodes: []dagNodeRequest{
		{Name: "one", Prompt: "one", Repo: "/repo"},
		{Name: "two", Prompt: "two", Repo: "/repo"},
	}}

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", mustMarshal(t, reqBody))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(CodeCapacityExhausted)) {
		t.Fatalf("body = %s, want capacity code", rec.Body.String())
	}
	if got, err := deps.Store.ListDAGBudgets(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("DAG persisted despite rejection: dags=%+v err=%v", got, err)
	}
}

// --- 2. POST /v1/dags cyclic ---------------------------------------------

func TestHandleCreateDag_Cyclic(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	reqBody := createDagRequest{
		Nodes: []dagNodeRequest{
			{Name: "a", Prompt: "a", Repo: "/repo"},
			{Name: "b", Prompt: "b", Repo: "/repo"},
		},
		Edges: []dagEdgeRequest{
			{Task: "a", DependsOn: "b"},
			{Task: "b", DependsOn: "a"},
		},
	}

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", mustMarshal(t, reqBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeDAGCycle {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeDAGCycle)
	}
}

// --- 3. POST /v1/dags unknown edge endpoint -------------------------------

func TestHandleCreateDag_UnknownEdgeEndpoint(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	reqBody := createDagRequest{
		Nodes: []dagNodeRequest{
			{Name: "a", Prompt: "a", Repo: "/repo"},
		},
		Edges: []dagEdgeRequest{
			{Task: "a", DependsOn: "ghost"},
		},
	}

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", mustMarshal(t, reqBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeBadRequest {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
	}
}

// --- 4. POST /v1/dags unknown JSON field ----------------------------------

func TestHandleCreateDag_UnknownField(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	body := []byte(`{"nodes":[{"name":"a","prompt":"a","repo":"/repo"}],"bogus_field":true}`)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeBadRequest {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
	}
}

// --- 5. GET /v1/dags/{unknown} ---------------------------------------------

func TestHandleGetDag_Unknown(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/dags/no-such-dag", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeDAGNotFound {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeDAGNotFound)
	}
}

// --- 6. POST /v1/dags duplicate node name ---------------------------------

func TestHandleCreateDag_DuplicateNodeName(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	reqBody := createDagRequest{
		Nodes: []dagNodeRequest{
			{Name: "a", Prompt: "a", Repo: "/repo"},
			{Name: "a", Prompt: "a-again", Repo: "/repo"},
		},
	}

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", mustMarshal(t, reqBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeBadRequest {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
	}
}

// --- 7. POST /v1/dags with no nodes ---------------------------------------

func TestHandleCreateDag_NoNodes(t *testing.T) {
	deps := newDagsTestDeps(t)
	srv := newDagsTestServer(t, deps)

	body := []byte(`{"nodes":[]}`)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dags", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeBadRequest {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
	}
}
