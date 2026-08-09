package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/supervisor"
	"github.com/djbu/corral/internal/version"
)

// --- fixtures ----------------------------------------------------------

// seedChainFixture builds the m5.md §10 example dag directly: three tasks
// A -> B -> C, where the edge notation means "B depends on A" and "C
// depends on B" (i.e. B, C are descendants of A; C is a descendant of B).
// Each task gets its own live-looking session row via seedEventsSession
// (handlers_sessions_test.go), then SetTaskSession links task to session —
// exactly what a real `corral run` submission followed by orchestrator
// scheduling would leave behind. A fourth task D, depending on C, is left
// with no session_id at all, to prove an empty session id never leaks into
// a subtree (a task not yet spawned contributes nothing).
func seedChainFixture(t *testing.T, st *store.Store) (dagID, sessA, sessB, sessC string) {
	t.Helper()
	ctx := context.Background()

	dagID = "dag-scope-chain"
	budget := 10.0
	sub := store.DAGSubmission{
		DAGID:     dagID,
		BudgetUSD: &budget,
		Tasks: []store.CreateTaskParams{
			scopeTaskParams("task-A", dagID, "A"),
			scopeTaskParams("task-B", dagID, "B"),
			scopeTaskParams("task-C", dagID, "C"),
			scopeTaskParams("task-D", dagID, "D"),
		},
		Deps: []store.Dep{
			{TaskID: "task-B", DependsOn: "task-A"},
			{TaskID: "task-C", DependsOn: "task-B"},
			{TaskID: "task-D", DependsOn: "task-C"},
		},
	}
	if err := st.SubmitDAG(ctx, sub); err != nil {
		t.Fatalf("SubmitDAG: %v", err)
	}

	sessA, sessB, sessC = "sess-A", "sess-B", "sess-C"
	seedEventsSession(t, st, sessA)
	seedEventsSession(t, st, sessB)
	seedEventsSession(t, st, sessC)
	// task-D deliberately gets no SetTaskSession call — its session_id
	// stays "".

	if err := st.SetTaskSession(ctx, "task-A", sessA); err != nil {
		t.Fatalf("SetTaskSession A: %v", err)
	}
	if err := st.SetTaskSession(ctx, "task-B", sessB); err != nil {
		t.Fatalf("SetTaskSession B: %v", err)
	}
	if err := st.SetTaskSession(ctx, "task-C", sessC); err != nil {
		t.Fatalf("SetTaskSession C: %v", err)
	}
	return dagID, sessA, sessB, sessC
}

func scopeTaskParams(id, dagID, name string) store.CreateTaskParams {
	return store.CreateTaskParams{
		ID:     id,
		DAGID:  dagID,
		Name:   name,
		Prompt: "do the thing",
		Repo:   "/repo",
		Cwd:    "/repo",
		Status: store.TaskPending,
	}
}

// doScoped mirrors doVersioned/doAuthed (handlers_sessions_test.go /
// middleware_auth_test.go) but additionally injects tok into the request
// context via contextWithToken, the exact mechanism middleware_auth_test.go's
// authProbe uses — bypassing bearerAuth entirely, since these tests are
// about what handlers do with an already-resolved token, not about token
// verification itself. tok == nil leaves the context token-free, simulating
// the unix socket (TokenFromContext ok == false).
func doScoped(t *testing.T, h http.Handler, method, path string, body []byte, tok *store.TokenRow) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	if tok != nil {
		req = req.WithContext(contextWithToken(req.Context(), *tok))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func scopedToken(rootSessionID string) *store.TokenRow {
	return &store.TokenRow{ID: "tok-scoped", Scope: "session", SessionID: rootSessionID}
}

func adminToken() *store.TokenRow {
	// SessionID deliberately non-empty (m5.md §10 test gap): an admin token
	// must see everything regardless of what SessionID happens to carry —
	// the natural-but-wrong reading is "confine whenever SessionID is set."
	return &store.TokenRow{ID: "tok-admin", Scope: "admin", SessionID: "sess-A"}
}

// --- 1. sessionSubtree: the chain walk (REQUIRED test 1) ----------------

func TestSessionSubtree_ChainWalk(t *testing.T) {
	st := openMetaTestStore(t)
	_, sessA, sessB, sessC := seedChainFixture(t, st)
	ctx := context.Background()

	assertSet := func(t *testing.T, got map[string]struct{}, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("subtree = %v, want exactly %v", got, want)
		}
		for _, w := range want {
			if _, ok := got[w]; !ok {
				t.Fatalf("subtree = %v, missing %q", got, w)
			}
		}
	}

	t.Run("rooted at B includes B and C, excludes A", func(t *testing.T) {
		got, err := sessionSubtree(ctx, st, sessB)
		if err != nil {
			t.Fatalf("sessionSubtree: %v", err)
		}
		assertSet(t, got, sessB, sessC)
	})

	// Gap test: rooted at C must be {C} only. A rooted-at-B-only test alone
	// would still pass even if TaskID/DependsOn were swapped in the walk
	// (that bug happens to also produce {B,C} from B) — asserting the leaf
	// end catches an inverted edge.
	t.Run("rooted at C includes only C", func(t *testing.T) {
		got, err := sessionSubtree(ctx, st, sessC)
		if err != nil {
			t.Fatalf("sessionSubtree: %v", err)
		}
		assertSet(t, got, sessC)
	})

	t.Run("rooted at A includes the whole chain, never the unset task-D session", func(t *testing.T) {
		got, err := sessionSubtree(ctx, st, sessA)
		if err != nil {
			t.Fatalf("sessionSubtree: %v", err)
		}
		assertSet(t, got, sessA, sessB, sessC)
		if _, ok := got[""]; ok {
			t.Fatal("subtree contains the empty session id (task-D has no session_id)")
		}
	})

	t.Run("standalone session with no task is just itself", func(t *testing.T) {
		got, err := sessionSubtree(ctx, st, "standalone-session-no-task")
		if err != nil {
			t.Fatalf("sessionSubtree: %v", err)
		}
		assertSet(t, got, "standalone-session-no-task")
	})
}

// --- 2. absent token => allow (REQUIRED test 2) --------------------------

func TestTokenAllowsSession_AbsentTokenAllows(t *testing.T) {
	ctx := context.Background()
	// st is deliberately nil: tokenAllowsSession's short-circuit for "no
	// token in context" must return before ever touching the store — the
	// design's hot-path invariant for unix-socket callers. A store.Store
	// method call against a nil receiver would panic, so this test doubles
	// as proof the store is never consulted.
	allowed, err := tokenAllowsSession(ctx, nil, "some-foreign-session-id")
	if err != nil {
		t.Fatalf("tokenAllowsSession: %v", err)
	}
	if !allowed {
		t.Fatal("tokenAllowsSession with no token in context = false, want true (absent token => allow)")
	}
}

// --- 3. admin bypass (REQUIRED test 3) -----------------------------------

func TestTokenAllowsSession_AdminBypassEvenWithSessionIDSet(t *testing.T) {
	ctx := contextWithToken(context.Background(), *adminToken())
	// Same nil-store proof as the absent-token case: scope=admin must
	// short-circuit before touching the store too.
	allowed, err := tokenAllowsSession(ctx, nil, "some-completely-foreign-session")
	if err != nil {
		t.Fatalf("tokenAllowsSession: %v", err)
	}
	if !allowed {
		t.Fatal("admin token denied a foreign session, want allowed")
	}
}

// --- tokenDAGScope variants (feeds REQUIRED test 6, plus gap 3) ----------

func TestTokenDAGScope_Variants(t *testing.T) {
	st := openMetaTestStore(t)
	dagID, _, sessB, _ := seedChainFixture(t, st)

	t.Run("no token: not scoped", func(t *testing.T) {
		_, isScoped, err := tokenDAGScope(context.Background(), st)
		if err != nil {
			t.Fatalf("tokenDAGScope: %v", err)
		}
		if isScoped {
			t.Fatal("isScoped = true with no token, want false")
		}
	})

	t.Run("admin: not scoped", func(t *testing.T) {
		ctx := contextWithToken(context.Background(), *adminToken())
		_, isScoped, err := tokenDAGScope(ctx, st)
		if err != nil {
			t.Fatalf("tokenDAGScope: %v", err)
		}
		if isScoped {
			t.Fatal("isScoped = true for admin, want false")
		}
	})

	t.Run("scoped, root has a task: allowedDagID is that dag", func(t *testing.T) {
		ctx := contextWithToken(context.Background(), *scopedToken(sessB))
		allowedDagID, isScoped, err := tokenDAGScope(ctx, st)
		if err != nil {
			t.Fatalf("tokenDAGScope: %v", err)
		}
		if !isScoped || allowedDagID != dagID {
			t.Fatalf("tokenDAGScope = (%q, %v), want (%q, true)", allowedDagID, isScoped, dagID)
		}
	})

	t.Run("scoped, root has no task: allowedDagID is empty", func(t *testing.T) {
		ctx := contextWithToken(context.Background(), *scopedToken("standalone-session-no-task"))
		allowedDagID, isScoped, err := tokenDAGScope(ctx, st)
		if err != nil {
			t.Fatalf("tokenDAGScope: %v", err)
		}
		if !isScoped || allowedDagID != "" {
			t.Fatalf("tokenDAGScope = (%q, %v), want (\"\", true)", allowedDagID, isScoped)
		}
	})
}

// --- shared multi-route test deps ----------------------------------------

// scopeTestDeps bundles everything the session/dag/dashboard/events routes
// need, all wired to the SAME store — unlike newSessionsTestDeps/
// newDagsTestDeps, which each open their own private store, these tests
// need one store shared across SessionsDeps/DagsDeps/DashboardDeps/
// EventsDeps so a single seeded fixture is visible to all of them. The
// registry's checkpointer is a stub (never nil) rather than newSessionsTestDeps'
// nil: handleWake's scope-gate tests below reach Registry.Wake for a
// seeded-but-never-spawned session, and Wake's non-live path calls
// checkpointer.Resumable on whatever's in ctx — a nil Checkpointer
// interface value panics on that call, whereas the stub below safely
// answers "not resumable."
type scopeTestDeps struct {
	Store    *store.Store
	Engine   state.Engine
	Registry *supervisor.Registry
}

// stubCheckpointer implements supervisor.Checkpointer as inertly as
// possible: Checkpoint/Restore are never exercised by these tests (nothing
// here ever reaches a live session for them to act on), and Resumable
// always refuses, so Registry.Wake against a seeded-only (never spawned)
// session deterministically reaches its ErrNotResumable branch rather than
// panicking against a nil checkpointer.
type stubCheckpointer struct{}

func (stubCheckpointer) Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) (bool, error) {
	return false, nil
}
func (stubCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	return session.Spec{}, nil
}
func (stubCheckpointer) Resumable(rec session.Session) (bool, string) {
	return false, "scope_test stub: never resumable"
}

func newScopeTestDeps(t *testing.T) scopeTestDeps {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	engine := state.New(st)
	reg := supervisor.New(st, engine, stubCheckpointer{}, fc, supervisor.Config{
		StateDir:      t.TempDir(),
		EnvSnapshot:   map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion: "test",
		APIVersion:    1,
	}, nil)

	return scopeTestDeps{Store: st, Engine: engine, Registry: reg}
}

func (d scopeTestDeps) sessionsDeps() SessionsDeps {
	return SessionsDeps{Store: d.Store, Engine: d.Engine, Registry: d.Registry}
}
func (d scopeTestDeps) dagsDeps() DagsDeps { return DagsDeps{Store: d.Store} }
func (d scopeTestDeps) dashboardDeps() DashboardDeps {
	return DashboardDeps{Store: d.Store, Engine: d.Engine, Registry: d.Registry}
}

// --- 4. scoped 403/200 across the session point routes (REQUIRED test 4) --

func TestSessionPointRoutes_ScopeEnforcement(t *testing.T) {
	deps := newScopeTestDeps(t)
	_, sessA, sessB, sessC := seedChainFixture(t, deps.Store)
	srv := New()
	srv.RegisterSessions(deps.sessionsDeps())
	h := srv.Handler()

	rootedAtB := scopedToken(sessB)

	assertForbidden := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != CodeForbidden {
			t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeForbidden)
		}
	}
	assertNotForbidden := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code == http.StatusForbidden {
			t.Fatalf("status = 403 (%s), want the gate to pass for an in-subtree session", rec.Body.String())
		}
	}

	t.Run("GET foreign session A is 403", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessA, nil, rootedAtB)
		assertForbidden(t, rec)
	})
	t.Run("GET root session B is allowed", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessB, nil, rootedAtB)
		assertNotForbidden(t, rec)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("GET descendant session C is allowed", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessC, nil, rootedAtB)
		assertNotForbidden(t, rec)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("GET events for foreign session A is 403", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessA+"/events", nil, rootedAtB)
		assertForbidden(t, rec)
	})
	t.Run("GET events for own session B is allowed", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessB+"/events", nil, rootedAtB)
		assertNotForbidden(t, rec)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("DELETE foreign session A is 403", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodDelete, "/v1/sessions/"+sessA, nil, rootedAtB)
		assertForbidden(t, rec)
	})
	t.Run("DELETE own session B is allowed (falls through to the not-live path)", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodDelete, "/v1/sessions/"+sessB, nil, rootedAtB)
		assertNotForbidden(t, rec)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("answer to foreign session A is 403", func(t *testing.T) {
		body := mustMarshal(t, answerRequest{Text: "hi"})
		rec := doScoped(t, h, http.MethodPost, "/v1/sessions/"+sessA+"/answer", body, rootedAtB)
		assertForbidden(t, rec)
	})
	t.Run("answer to descendant session C is allowed (fails later, not on scope)", func(t *testing.T) {
		body := mustMarshal(t, answerRequest{Text: "hi"})
		rec := doScoped(t, h, http.MethodPost, "/v1/sessions/"+sessC+"/answer", body, rootedAtB)
		assertNotForbidden(t, rec)
	})
	t.Run("wake of foreign session A is 403", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodPost, "/v1/sessions/"+sessA+"/wake", nil, rootedAtB)
		assertForbidden(t, rec)
	})
	t.Run("wake of own session B is allowed (fails later, not on scope)", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodPost, "/v1/sessions/"+sessB+"/wake", nil, rootedAtB)
		assertNotForbidden(t, rec)
		// Not resumable (stubCheckpointer always refuses) is the expected
		// non-scope failure proving the gate was passed, not skipped.
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (not resumable), body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin token reaches every route for the foreign session too", func(t *testing.T) {
		admin := adminToken()
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessA, nil, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin GET status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("absent token (simulated socket) reaches every route for a session it does not own", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions/"+sessA, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("no-token GET status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
}

// --- 5. scoped list filter: sessions + dashboard (REQUIRED test 5) -------

func TestBuildSessionResponses_ScopedFilter(t *testing.T) {
	deps := newScopeTestDeps(t)
	_, sessA, sessB, sessC := seedChainFixture(t, deps.Store)

	srv := New()
	srv.RegisterSessions(deps.sessionsDeps())
	srv.RegisterDashboard(deps.dashboardDeps())
	h := srv.Handler()

	rootedAtB := scopedToken(sessB)

	t.Run("GET /v1/sessions filtered to subtree only", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions", nil, rootedAtB)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		var got listSessionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		ids := make(map[string]bool, len(got.Sessions))
		for _, s := range got.Sessions {
			ids[s.ID] = true
		}
		if len(ids) != 2 || !ids[sessB] || !ids[sessC] {
			t.Fatalf("session ids = %v, want exactly {%s, %s}", ids, sessB, sessC)
		}
		if ids[sessA] {
			t.Fatalf("foreign session %s leaked into scoped list", sessA)
		}
	})

	t.Run("GET /v1/dashboard sessions filtered identically", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dashboard", nil, rootedAtB)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		var got dashboardResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		ids := make(map[string]bool, len(got.Sessions))
		for _, s := range got.Sessions {
			ids[s.ID] = true
		}
		if len(ids) != 2 || !ids[sessB] || !ids[sessC] {
			t.Fatalf("dashboard session ids = %v, want exactly {%s, %s}", ids, sessB, sessC)
		}
	})

	t.Run("admin sees every session", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/sessions", nil, adminToken())
		var got listSessionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(got.Sessions) != 3 {
			t.Fatalf("admin session count = %d, want 3 (A,B,C)", len(got.Sessions))
		}
	})
}

// --- 6. DAG routes (REQUIRED test 6, plus gap 3: scoped-standalone) -------

func TestDagHandlers_ScopeEnforcement(t *testing.T) {
	deps := newScopeTestDeps(t)
	dagID, _, sessB, _ := seedChainFixture(t, deps.Store)

	srv := New()
	srv.RegisterDags(deps.dagsDeps())
	srv.RegisterDashboard(deps.dashboardDeps())
	h := srv.Handler()

	rootedAtB := scopedToken(sessB)

	// A second, foreign dag the scoped token has no claim to.
	foreignBudget := 3.0
	if err := deps.Store.SubmitDAG(context.Background(), store.DAGSubmission{
		DAGID:     "dag-foreign",
		BudgetUSD: &foreignBudget,
		Tasks:     []store.CreateTaskParams{scopeTaskParams("task-foreign", "dag-foreign", "solo")},
	}); err != nil {
		t.Fatalf("SubmitDAG(foreign): %v", err)
	}

	t.Run("GET foreign dag is 403", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dags/dag-foreign", nil, rootedAtB)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != CodeForbidden {
			t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeForbidden)
		}
	})
	t.Run("GET own dag is 200", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dags/"+dagID, nil, rootedAtB)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("GET unknown dag stays 404 even for a scoped token (existence-vs-scope ordering)", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dags/no-such-dag", nil, rootedAtB)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("POST /v1/dags is 403 outright for a scoped token", func(t *testing.T) {
		body := mustMarshal(t, createDagRequest{
			Nodes: []dagNodeRequest{{Name: "solo", Prompt: "p", Repo: "/repo"}},
		})
		rec := doScoped(t, h, http.MethodPost, "/v1/dags", body, rootedAtB)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != CodeForbidden {
			t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeForbidden)
		}
	})
	t.Run("POST /v1/dags still works for an admin token", func(t *testing.T) {
		body := mustMarshal(t, createDagRequest{
			Nodes: []dagNodeRequest{{Name: "solo", Prompt: "p", Repo: "/repo"}},
		})
		rec := doScoped(t, h, http.MethodPost, "/v1/dags", body, adminToken())
		if rec.Code != http.StatusCreated {
			t.Fatalf("admin create status = %d, want 201, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("GET /v1/dags list filtered to own dag only", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dags", nil, rootedAtB)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		var got listDagsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(got.Dags) != 1 || got.Dags[0].DAGID != dagID {
			t.Fatalf("dags = %+v, want exactly [%s]", got.Dags, dagID)
		}
	})
	t.Run("admin GET /v1/dags list sees both dags", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dags", nil, adminToken())
		var got listDagsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(got.Dags) < 2 {
			t.Fatalf("admin dag count = %d, want at least 2", len(got.Dags))
		}
	})

	// Gap test: a scoped token whose root session has no task at all
	// (tokenDAGScope returns ("", true)) must see an EMPTY dag list, not
	// every dag — the off-by-one this catches is allowedDagID=="" being
	// treated as "no restriction" instead of "restricted to nothing."
	t.Run("scoped token with no task sees an empty dag list and 403 on any real dag", func(t *testing.T) {
		noTask := scopedToken("standalone-session-no-task")
		rec := doScoped(t, h, http.MethodGet, "/v1/dags", nil, noTask)
		var got listDagsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(got.Dags) != 0 {
			t.Fatalf("dags = %+v, want empty", got.Dags)
		}

		rec2 := doScoped(t, h, http.MethodGet, "/v1/dags/"+dagID, nil, noTask)
		if rec2.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body=%s", rec2.Code, rec2.Body.String())
		}
	})

	t.Run("dashboard dags list inherits the same filter", func(t *testing.T) {
		rec := doScoped(t, h, http.MethodGet, "/v1/dashboard", nil, rootedAtB)
		var got dashboardResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(got.Dags) != 1 || got.Dags[0].DAGID != dagID {
			t.Fatalf("dashboard dags = %+v, want exactly [%s]", got.Dags, dagID)
		}
	})
}

// --- 7. SSE filter (REQUIRED test 7, plus gap 4: nil-Store safety) -------

func TestHandleStream_ScopeFilter(t *testing.T) {
	deps := newScopeTestDeps(t)
	_, sessA, sessB, sessC := seedChainFixture(t, deps.Store)

	broker := NewBroker()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sc := newSignalingClock(fc)
	streamDeps := EventsDeps{Broker: broker, Store: deps.Store, Clock: sc}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil).WithContext(ctx)
	req = req.WithContext(contextWithToken(req.Context(), *scopedToken(sessB)))

	go streamDeps.handleStream(w, req)
	waitSubscribed(t, sc.created)

	r := bufio.NewReader(pr)

	// Foreign session A's frame must never arrive; a daemon-scoped frame
	// (SessionID=="") must never arrive either. Publish both, then publish
	// an in-subtree frame (session C) and confirm THAT is what shows up
	// first on the wire — proving the earlier two were filtered, not just
	// delayed.
	broker.PublishEvent(session.Event{Seq: 1, SessionID: sessA, Kind: session.EventSessionCreated, DataJSON: "{}"})
	broker.PublishEvent(session.Event{Seq: 2, SessionID: "", Kind: session.EventTokenUnauthorized, DataJSON: "{}"})
	broker.PublishEvent(session.Event{Seq: 3, SessionID: sessC, Kind: session.EventSessionCreated, DataJSON: "{}"})

	lines := readFrame(t, r)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("unexpected frame: %v", lines)
	}
	var got frameJSON
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "data: ")), &got); err != nil {
		t.Fatalf("unmarshal frame JSON: %v (line=%q)", err, lines[0])
	}
	if got.Seq != 3 || got.SessionID != sessC {
		t.Fatalf("first frame delivered = %+v, want the seq=3/session=%s frame (A and daemon-scoped filtered)", got, sessC)
	}

	// Publish one more in-subtree frame (root session B itself, not just its
	// descendant C) and confirm it arrives immediately next — proving A and
	// the daemon-scoped frame were actually dropped, not merely queued
	// behind seq=3 in the subscriber's buffered channel.
	broker.PublishEvent(session.Event{Seq: 4, SessionID: sessB, Kind: session.EventSessionCreated, DataJSON: "{}"})
	lines4 := readFrame(t, r)
	if len(lines4) != 1 || !strings.HasPrefix(lines4[0], "data: ") {
		t.Fatalf("unexpected frame: %v", lines4)
	}
	var got4 frameJSON
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines4[0], "data: ")), &got4); err != nil {
		t.Fatalf("unmarshal frame JSON: %v (line=%q)", err, lines4[0])
	}
	if got4.Seq != 4 || got4.SessionID != sessB {
		t.Fatalf("second frame delivered = %+v, want the seq=4/session=%s frame (root session B itself)", got4, sessB)
	}
}

// TestHandleStream_AdminSeesEverything proves the filter is genuinely
// opt-in for scoped tokens only: an admin token (or, by extension, no
// token — the socket path — already covered by every pre-existing
// handlers_events_test.go case, none of which sets a token) still receives
// a foreign-looking and a daemon-scoped frame.
func TestHandleStream_AdminSeesEverything(t *testing.T) {
	deps := newScopeTestDeps(t)
	_, sessA, _, _ := seedChainFixture(t, deps.Store)

	broker := NewBroker()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sc := newSignalingClock(fc)
	streamDeps := EventsDeps{Broker: broker, Store: deps.Store, Clock: sc}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil).WithContext(ctx)
	req = req.WithContext(contextWithToken(req.Context(), *adminToken()))

	go streamDeps.handleStream(w, req)
	waitSubscribed(t, sc.created)

	r := bufio.NewReader(pr)
	broker.PublishEvent(session.Event{Seq: 1, SessionID: sessA, Kind: session.EventSessionCreated, DataJSON: "{}"})
	lines := readFrame(t, r)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("admin did not receive the foreign-session frame: %v", lines)
	}

	broker.PublishEvent(session.Event{Seq: 2, SessionID: "", Kind: session.EventTokenUnauthorized, DataJSON: "{}"})
	lines2 := readFrame(t, r)
	if len(lines2) != 1 || !strings.HasPrefix(lines2[0], "data: ") {
		t.Fatalf("admin did not receive the daemon-scoped frame: %v", lines2)
	}
}

// TestHandleStream_NilStoreSafeWithNoToken guards the exact case every
// pre-existing handlers_events_test.go test already relies on without
// asserting it: EventsDeps{Store: nil} (no scope wiring at all) must still
// serve a frame when the request carries no token, since the short-circuit
// in handleStream's scope block must never reach d.Store when !ok. If a
// later change moved the subtree computation above the token check, every
// test in that file would panic instead of merely losing coverage — this
// pins the safe order explicitly.
func TestHandleStream_NilStoreSafeWithNoToken(t *testing.T) {
	broker := NewBroker()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sc := newSignalingClock(fc)
	deps := EventsDeps{Broker: broker, Store: nil, Clock: sc}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil).WithContext(ctx)

	go deps.handleStream(w, req)
	waitSubscribed(t, sc.created)

	r := bufio.NewReader(pr)
	broker.PublishEvent(session.Event{Seq: 1, SessionID: "whatever", Kind: session.EventSessionCreated, DataJSON: "{}"})
	lines := readFrame(t, r)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("unexpected frame with nil Store/no token: %v", lines)
	}
}

// --- 8. attach (REQUIRED test 4's attach clause) -------------------------

func TestHandleAttach_ScopeEnforcement(t *testing.T) {
	fakeClaudeBin := buildFakeClaudeForAnswerE2E(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CORRAL_SESSION_CLAUDE_BIN", fakeClaudeBin)

	deps := newScopeTestDeps(t)
	// Give the registry room to actually spawn: stubCheckpointer's other
	// two methods are never exercised on a live Spawn path, only Resumable
	// (wake), so reusing scopeTestDeps' registry here is fine.

	srv := New()
	sessionsDeps := deps.sessionsDeps()
	srv.RegisterSessions(sessionsDeps)
	srv.RegisterAttach(deps.Registry, deps.Store, nil)
	h := srv.Handler()

	cwd := t.TempDir()
	createBody := mustMarshal(t, createSessionRequest{Cwd: cwd})
	rec := doVersioned(t, h, http.MethodPost, "/v1/sessions", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var created sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding create response: %v", err)
	}
	t.Cleanup(func() { _, _ = deps.Registry.Kill(context.Background(), created.ID, nil) })

	attachReq := func(tok *store.TokenRow) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+created.ID+"/attach", nil)
		req.Header.Set("Upgrade", "corral-attach/1")
		req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
		if tok != nil {
			req = req.WithContext(contextWithToken(req.Context(), *tok))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("scoped token rooted elsewhere is 403, never hijacked", func(t *testing.T) {
		foreign := scopedToken("some-other-session-entirely")
		rec := attachReq(foreign)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != CodeForbidden {
			t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeForbidden)
		}
	})

	t.Run("scoped token rooted at the target session passes the gate", func(t *testing.T) {
		// httptest.ResponseRecorder implements neither http.Hijacker nor
		// http.Flusher's real semantics, so handleAttach's rc.Hijack() call
		// deterministically fails once the scope gate is passed, with a 500
		// CodeInternal mentioning "hijack" — this proves gate traversal
		// (the check ran and allowed it), not a working attachment.
		own := scopedToken(created.ID)
		rec := attachReq(own)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (hijack failure past the gate), body=%s", rec.Code, rec.Body.String())
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env.Error.Code != CodeInternal || !strings.Contains(env.Error.Message, "hijack") {
			t.Fatalf("error = %+v, want CodeInternal mentioning hijack", env.Error)
		}
	})

	t.Run("admin token also passes the gate", func(t *testing.T) {
		rec := attachReq(adminToken())
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (hijack failure past the gate), body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("absent token (socket) also passes the gate", func(t *testing.T) {
		rec := attachReq(nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (hijack failure past the gate), body=%s", rec.Code, rec.Body.String())
		}
	})
}
