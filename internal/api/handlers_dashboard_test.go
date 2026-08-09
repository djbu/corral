package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/api/dashboard"
	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/clock/clocktest"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/supervisor"
	"github.com/djbu/corral/internal/version"
)

// fakeDashboardEngine is a minimal state.Engine test double whose State
// and Stale results are configured per session id — standing in for M2's
// hook-driven engine so these tests can put a session into "blocked" (a
// state NoopEngine, the only other Engine in this package's test suite,
// can never produce) without any real hook traffic.
type fakeDashboardEngine struct {
	states   map[string]session.AgentState
	staleIDs map[string]bool
}

func (e *fakeDashboardEngine) OnHookEvent(ctx context.Context, sessionID string, ev state.HookEvent) error {
	return nil
}

func (e *fakeDashboardEngine) OnLifecycle(ctx context.Context, sessionID string, kind session.EventKind, data any) error {
	return nil
}

func (e *fakeDashboardEngine) State(sessionID string) session.AgentState {
	if s, ok := e.states[sessionID]; ok {
		return s
	}
	return session.AgentRunning
}

// Stale implements the optional interface staleForEngine
// (handlers_sessions.go) type-asserts for.
func (e *fakeDashboardEngine) Stale(sessionID string) bool {
	return e.staleIDs[sessionID]
}

func openDashboardTestStore(t *testing.T, fc clock.Clock) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedDashboardSession inserts a minimal, never-spawned session row
// directly (mirroring handlers_hooks_test.go's openHooksTestStore), since
// these tests only need a session for GET /v1/dashboard's enrichment path
// to enrich, never a live process.
func seedDashboardSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             id,
		Name:           id,
		Mode:           session.ModeInteractive,
		Cwd:            "/tmp/work",
		ClaudeBin:      "/usr/local/bin/claude",
		SettingsPath:   "/tmp/state/sessions/" + id + "/settings.json",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusRunning,
		PID:            1234,
		PGID:           1234,
		ProcStartNs:    1,
		Rows:           40,
		Cols:           120,
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
}

func newDashboardTestRegistry(t *testing.T, st *store.Store, engine state.Engine, fc clock.Clock) *supervisor.Registry {
	t.Helper()
	return supervisor.New(st, engine, nil, fc, supervisor.Config{
		StateDir:      t.TempDir(),
		EnvSnapshot:   map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion: "test",
		APIVersion:    1,
	}, nil)
}

// --- 1. GET /v1/dashboard happy path: shared enrichment + dag summaries ---

func TestHandleSnapshot_HappyPath(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st := openDashboardTestStore(t, fc)

	seedDashboardSession(t, st, "sess-blocked")
	seedDashboardSession(t, st, "sess-normal")
	l, err := st.UpsertLearningCandidate(context.Background(), store.UpsertLearningParams{
		ID: "proposal", Repo: "/repo", Kind: store.LearningPermissionRule, Fingerprint: "fp-dashboard",
		ContentJSON:  `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`,
		BaselineJSON: `{}`, EvidenceCount: 3, ExpiresMs: fc.Now().Add(90 * 24 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err = st.TransitionLearning(context.Background(), l.ID, store.LearningTransition{From: store.LearningCandidate, To: store.LearningVerified})
	if err == nil {
		l, err = st.TransitionLearning(context.Background(), l.ID, store.LearningTransition{From: store.LearningVerified, To: store.LearningProposed})
	}
	if err != nil {
		t.Fatal(err)
	}

	// Give sess-blocked an actual blocked_reason payload so the response
	// field the dashboard's blocked-card UI renders (sessionResponse's
	// BlockedReason, sourced from Session.BlockedReasonJSON) is exercised
	// by this test, not just the agent_state label.
	if _, err := st.UpdateSession(context.Background(), "sess-blocked", func(s *session.Session) {
		s.BlockedReasonJSON = `{"prompt":"Allow write?"}`
	}); err != nil {
		t.Fatalf("UpdateSession(sess-blocked): %v", err)
	}

	engine := &fakeDashboardEngine{
		states:   map[string]session.AgentState{"sess-blocked": session.AgentBlocked},
		staleIDs: map[string]bool{"sess-normal": true},
	}
	reg := newDashboardTestRegistry(t, st, engine, fc)

	// Seed one dag budget row the same way an operator would: through the
	// real POST /v1/dags handler, on the same store.
	dagsSrv := New()
	dagsSrv.RegisterDags(DagsDeps{Store: st})
	createBody := createDagRequest{
		Nodes: []dagNodeRequest{{Name: "plan", Prompt: "plan it", Repo: "/repo"}},
	}
	createRec := doVersioned(t, dagsSrv.Handler(), http.MethodPost, "/v1/dags", mustMarshal(t, createBody))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("seed POST /v1/dags: status = %d, body=%s", createRec.Code, createRec.Body.String())
	}
	var created dagDetailResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal seeded dag: %v", err)
	}

	srv := New()
	srv.RegisterDashboard(DashboardDeps{Store: st, Engine: engine, Registry: reg})

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/dashboard", nil)
	assertAPIVersionHeader(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/dashboard: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// "stale" is json:",omitempty" on sessionResponse, so decoding into the
	// struct can't distinguish "false" from "absent" — assert its presence
	// directly against the raw body, proving the shared enrichment path
	// (not some second, divergent computation) produced it.
	if !strings.Contains(rec.Body.String(), `"stale":true`) {
		t.Fatalf("GET /v1/dashboard body missing stale:true for sess-normal: %s", rec.Body.String())
	}
	// blocked_reason is json.RawMessage — must round-trip byte-for-byte
	// through the shared enrichment path, not just be present.
	if !strings.Contains(rec.Body.String(), `"blocked_reason":{"prompt":"Allow write?"}`) {
		t.Fatalf("GET /v1/dashboard body missing blocked_reason for sess-blocked: %s", rec.Body.String())
	}

	var got dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("len(sessions) = %d, want 2: %+v", len(got.Sessions), got.Sessions)
	}
	var sawBlocked bool
	for _, s := range got.Sessions {
		if s.ID == "sess-blocked" {
			sawBlocked = true
			if s.AgentState != string(session.AgentBlocked) {
				t.Fatalf("sess-blocked agent_state = %q, want %q", s.AgentState, session.AgentBlocked)
			}
		}
	}
	if !sawBlocked {
		t.Fatalf("sess-blocked missing from sessions: %+v", got.Sessions)
	}

	if len(got.Dags) != 1 {
		t.Fatalf("len(dags) = %d, want 1: %+v", len(got.Dags), got.Dags)
	}
	if got.Dags[0].DAGID != created.DAGID {
		t.Fatalf("dags[0].dag_id = %q, want %q", got.Dags[0].DAGID, created.DAGID)
	}
	if len(got.Learnings) != 1 || got.Learnings[0].Rule != "Bash(npm test)" || got.Learnings[0].Status != "proposed" {
		t.Fatalf("learnings = %+v", got.Learnings)
	}
}

func TestDashboardLearningSummariesRespectSessionRepoScope(t *testing.T) {
	fc := clocktest.NewFake(time.Now())
	st := openDashboardTestStore(t, fc)
	repoA, repoB := t.TempDir(), t.TempDir()
	createLearningSession(t, st, "root-learning", repoA)
	canonicalA, _ := corralgit.CanonicalPath(repoA)
	canonicalB, _ := corralgit.CanonicalPath(repoB)
	for _, item := range []struct{ id, repo, fp string }{{"a", canonicalA, "fa"}, {"b", canonicalB, "fb"}} {
		l, err := st.UpsertLearningCandidate(context.Background(), store.UpsertLearningParams{
			ID: item.id, Repo: item.repo, Kind: store.LearningPermissionRule, Fingerprint: item.fp,
			ContentJSON:  `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`,
			BaselineJSON: `{}`, ExpiresMs: fc.Now().Add(time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		l, _ = st.TransitionLearning(context.Background(), l.ID, store.LearningTransition{From: store.LearningCandidate, To: store.LearningVerified})
		_, _ = st.TransitionLearning(context.Background(), l.ID, store.LearningTransition{From: store.LearningVerified, To: store.LearningProposed})
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/dashboard", nil)
	req = req.WithContext(contextWithToken(req.Context(), store.TokenRow{Scope: "session", SessionID: "root-learning"}))
	rows, err := (DashboardDeps{Store: st}).learningSummaries(req)
	if err != nil || len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("scoped learning summaries = (%+v,%v)", rows, err)
	}
}

// --- 2. POST /v1/dashboard -> 405 -----------------------------------------

func TestHandleSnapshot_PostNotAllowed(t *testing.T) {
	fc := clocktest.NewFake(time.Now())
	st := openDashboardTestStore(t, fc)
	engine := &fakeDashboardEngine{}
	reg := newDashboardTestRegistry(t, st, engine, fc)

	srv := New()
	srv.RegisterDashboard(DashboardDeps{Store: st, Engine: engine, Registry: reg})

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/dashboard", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/dashboard: status = %d, want 405, body=%s", rec.Code, rec.Body.String())
	}
}

// --- 3. allowlist / fall-through auth (HARD CONSTRAINT security test) ----

// TestAuthenticatedHandler_DashboardAllowlist proves
// Server.AuthenticatedHandler's shell allowlist is exactly "/" and
// "/dashboard/*", that everything else — including paths that were never
// registered on the mux at all — falls through to the bearer wall (a 404
// here would prove a request reached the mux ahead of auth, which would be
// a bug), and that /v1/dashboard itself is still reachable with a valid
// token.
func TestAuthenticatedHandler_DashboardAllowlist(t *testing.T) {
	f := newFakeAuthStore(t)
	plaintext := mintToken(t, f)

	fc := clocktest.NewFake(time.Now())
	engine := &fakeDashboardEngine{}
	reg := newDashboardTestRegistry(t, f.store, engine, fc)

	srv := New()
	srv.RegisterDashboard(DashboardDeps{Store: f.store, Engine: engine, Registry: reg})

	h := srv.AuthenticatedHandler(f, nil, dashboard.Handler())

	noTokenCases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/dashboard/app.js", http.StatusOK},
		{http.MethodGet, "/v1/dashboard", http.StatusUnauthorized},
		{http.MethodGet, "/admin", http.StatusUnauthorized},
		{http.MethodGet, "/v1/nonexistent", http.StatusUnauthorized},
		{http.MethodGet, "/dashboard", http.StatusUnauthorized}, // no trailing slash: must NOT match the shell
	}
	for _, tc := range noTokenCases {
		t.Run("no-token "+tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s %s (no token): status = %d, want %d, body=%s", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// GET /v1/dashboard WITH a valid token -> 200: the wall itself still
	// works normally for a real caller, the allowlist just doesn't widen
	// it.
	req := httptest.NewRequest(http.MethodGet, "/v1/dashboard", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/dashboard (valid token): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// --- 4. unix Handler() excludes the shell entirely (TCP-only) ------------

// TestHandler_UnixSocketExcludesDashboardShell proves the unix-socket
// Handler() (which has no shell parameter to accept a dashboard.Handler())
// never serves the dashboard shell at GET /, even though the TCP-facing
// AuthenticatedHandler does. Handler() has nothing registered at "/", so
// the expected outcome is versionMiddleware's 400 version_mismatch (no
// Corral-Api-Version header sent) — the only assertion that actually
// matters is that it is NOT a 200 text/html response.
func TestHandler_UnixSocketExcludesDashboardShell(t *testing.T) {
	srv := New()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK && strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("unix Handler() served the dashboard shell at GET / (status=200, content-type=%q) — the shell must be TCP-only", rec.Header().Get("Content-Type"))
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("unix Handler() GET / status = %d, want 400 (version_mismatch) or 404", rec.Code)
	}
}
