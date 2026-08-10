package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// newSessionsTestServer mirrors newTestServer (handlers_meta_test.go) for
// the sessions routes.
func newSessionsTestServer(t *testing.T, deps SessionsDeps) *Server {
	t.Helper()
	s := New()
	s.RegisterSessions(deps)
	return s
}

// newSessionsTestDeps builds a SessionsDeps backed by a real, temp-dir
// sqlite store and a real *supervisor.Registry/state.NoopEngine — there is
// no test double for either since Registry is a concrete type (not an
// interface) and NoopEngine is state's only M1 implementation. The
// checkpointer is deliberately nil: every test below either never reaches
// Registry.Kill's live branch (idOrName never names a session this
// registry actually spawned, so Kill's ErrNotLive path returns before ever
// touching the checkpointer) or never reaches a live Spawn at all
// (handleCreate's happy path needs a real claude_bin to exec, which is
// exactly what the tests below never give it — see the package doc comment
// on claudeBinEnv).
func newSessionsTestDeps(t *testing.T) SessionsDeps {
	t.Helper()

	// Hermeticity: config.LoadSession consults $HOME/.corral/config.toml.
	// Point HOME at an empty temp dir so no real machine's config (or lack
	// thereof) can influence claude_bin/mode/etc. resolution in these
	// tests.
	t.Setenv("HOME", t.TempDir())

	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	engine := state.New(st)
	reg := supervisor.New(st, engine, nil, fc, supervisor.Config{
		StateDir:      t.TempDir(),
		EnvSnapshot:   map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion: "test",
		APIVersion:    1,
	}, nil)

	return SessionsDeps{Store: st, Engine: engine, Registry: reg}
}

// setUnresolvableClaudeBin points CORRAL_SESSION_CLAUDE_BIN at an absolute
// path that does not exist. handleCreate resolves an absolute claude_bin
// without ever calling exec.LookPath (handlers_sessions.go's
// !filepath.IsAbs(claudeBin) branch), so config.LoadSession and
// store.CreateSession both still succeed — only the later
// Registry.Spawn -> supervisor.Spawn -> pty.StartWithSize call fails
// (fork/exec: no such file or directory), synchronously and without ever
// starting a real child process or goroutine. This lets tests that only
// care about pre-Spawn behavior (default name assignment, the row
// CreateSession leaves behind) exercise the real handler end to end
// without needing a real claude binary or a fake-claude script.
func setUnresolvableClaudeBin(t *testing.T) {
	t.Helper()
	t.Setenv("CORRAL_SESSION_CLAUDE_BIN", filepath.Join(t.TempDir(), "no-such-claude-binary"))
}

func writeUserTemplates(t *testing.T, raw string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".corral")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates.toml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHandleCreateSession_TemplateIsAuthorizedAndAudited(t *testing.T) {
	deps := newSessionsTestDeps(t)
	setUnresolvableClaudeBin(t)
	writeUserTemplates(t, "[[template]]\nname=\"review\"\nmodel=\"template-model\"\n")
	srv := newSessionsTestServer(t, deps)
	body := []byte(`{"cwd":"` + t.TempDir() + `","name":"templated","template":"review"}`)
	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions", body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want spawn failure after durable create; body=%s", rec.Code, rec.Body.String())
	}
	sess, err := deps.Store.GetSessionByName(t.Context(), "templated")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Template != "review" || sess.Model != "template-model" {
		t.Fatalf("session template/model = %q/%q", sess.Template, sess.Model)
	}
	events, err := deps.Store.ListEvents(t.Context(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || !strings.Contains(events[0].DataJSON, `"template":"review"`) {
		t.Fatalf("session.created audit = %+v", events)
	}
}

func assertAPIVersionHeader(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	got := rec.Header().Get("Corral-Api-Version")
	want := strconv.Itoa(version.APIVersion)
	if got != want {
		t.Errorf("Corral-Api-Version header = %q, want %q", got, want)
	}
}

func decodeErrorEnvelope(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decoding error envelope: %v, body=%s", err, body)
	}
	return env
}

// --- 1. GET /v1/sessions/{unknown} --------------------------------------

func TestHandleGetSession_Unknown(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/sessions/no-such-session", nil)
	assertAPIVersionHeader(t, rec)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	// Exact envelope shape: {"error":{"code","message","details"}}, nothing
	// else at the top level.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("Unmarshal top level: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("top-level envelope has %d keys, want exactly 1 (\"error\"): %s", len(raw), rec.Body.String())
	}
	errRaw, ok := raw["error"]
	if !ok {
		t.Fatalf("top-level envelope missing \"error\" key: %s", rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(errRaw, &body); err != nil {
		t.Fatalf("Unmarshal error body: %v", err)
	}
	if len(body) != 3 {
		t.Fatalf("error body has %d keys, want exactly 3 (code, message, details): %s", len(body), errRaw)
	}
	for _, key := range []string{"code", "message", "details"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("error body missing %q key: %s", key, errRaw)
		}
	}

	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeSessionNotFound {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeSessionNotFound)
	}
	if env.Error.Message == "" {
		t.Fatal("error.message = \"\", want non-empty")
	}
	if env.Error.Details == nil {
		t.Fatal("error.details = nil, want non-nil (possibly empty) object")
	}
}

// --- 2. POST /v1/sessions: duplicate name -------------------------------

// TestHandleCreateSession_NameTaken locks down the status/code
// handlers_sessions.go actually returns for a name collision. Reading the
// handler: the pre-CreateSession check at handlers_sessions.go's
// `d.Store.GetSessionByName(ctx, name)` branch returns 409 with
// CodeSessionNameTaken ("session_name_taken") before ever reaching
// config.LoadSession or Registry.Spawn, so this test needs no claude_bin
// wiring at all — cwd validation and the name check both happen first.
func TestHandleCreateSession_NameTaken(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)
	cwd := t.TempDir()

	if _, err := deps.Store.CreateSession(t.Context(), store.CreateSessionParams{
		ID:             "11111111-1111-1111-1111-111111111111",
		Name:           "taken",
		Mode:           session.ModeInteractive,
		Cwd:            cwd,
		ClaudeBin:      "/bin/true",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusExited,
	}); err != nil {
		t.Fatalf("seeding existing session: %v", err)
	}

	reqBody, _ := json.Marshal(createSessionRequest{Name: "taken", Cwd: cwd})
	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions", reqBody)
	assertAPIVersionHeader(t, rec)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (409), body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeSessionNameTaken {
		t.Fatalf("FINDING: error.code = %q, want %q (session_name_taken) for a duplicate session name", env.Error.Code, CodeSessionNameTaken)
	}
}

// --- 3. POST /v1/sessions: validation failures --------------------------

func TestHandleCreateSession_ValidationFailures(t *testing.T) {
	tests := []struct {
		name     string
		body     []byte // nil means send no body at all
		wantCode Code
	}{
		{
			name:     "missing body",
			body:     nil,
			wantCode: CodeBadRequest,
		},
		{
			name:     "bad JSON",
			body:     []byte(`{"cwd": "/tmp", not valid json`),
			wantCode: CodeBadRequest,
		},
		{
			name:     "relative cwd",
			body:     mustJSON(t, createSessionRequest{Cwd: "relative/path"}),
			wantCode: CodeBadRequest,
		},
		{
			name:     "nonexistent cwd dir",
			body:     mustJSON(t, createSessionRequest{Cwd: filepath.Join(t.TempDir(), "does-not-exist")}),
			wantCode: CodeBadRequest,
		},
		{
			name:     "unsupported mode",
			body:     mustJSON(t, createSessionRequest{Cwd: t.TempDir(), Mode: "headless"}),
			wantCode: CodeUnsupportedMode,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newSessionsTestDeps(t)
			srv := newSessionsTestServer(t, deps)

			rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions", tt.body)
			assertAPIVersionHeader(t, rec)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (400), body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			env := decodeErrorEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tt.wantCode {
				t.Fatalf("error.code = %q, want %q, body=%s", env.Error.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling request body: %v", err)
	}
	return b
}

// --- 4. DELETE /v1/sessions/{idOrName}?grace= ---------------------------

// TestHandleDeleteSession_InvalidGrace exercises handlers_sessions.go's
// `time.ParseDuration(g)` validation, which runs strictly before
// Registry.Kill is ever called — so this needs a resolvable (store row
// exists) but non-live session, never a real process.
func TestHandleDeleteSession_InvalidGrace(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	sess, err := deps.Store.CreateSession(t.Context(), store.CreateSessionParams{
		ID:             "22222222-2222-2222-2222-222222222222",
		Name:           "grace-invalid",
		Mode:           session.ModeInteractive,
		Cwd:            t.TempDir(),
		ClaudeBin:      "/bin/true",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusExited,
	})
	if err != nil {
		t.Fatalf("seeding session: %v", err)
	}

	rec := doVersioned(t, srv.Handler(), http.MethodDelete, "/v1/sessions/"+sess.ID+"?grace=not-a-duration", nil)
	assertAPIVersionHeader(t, rec)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (400), body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeBadRequest {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
	}
}

// TestHandleDeleteSession_ValidGrace exercises the same query param on a
// non-live session (never registered with the Registry, since nothing in
// this test file successfully Spawns). Registry.Kill's first step,
// r.Get(idOrName), reports not-ok and returns supervisor.ErrNotLive before
// ever touching grace or the checkpointer, so handleDelete falls through to
// its own store.UpdateSession fallback (DesiredState -> stopped) and
// returns 200 — this test only confirms that a valid grace value parses
// and does not itself cause an error; it does not exercise grace actually
// reaching a live checkpoint (that is supervisor package territory).
func TestHandleDeleteSession_ValidGrace(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	sess, err := deps.Store.CreateSession(t.Context(), store.CreateSessionParams{
		ID:             "33333333-3333-3333-3333-333333333333",
		Name:           "grace-valid",
		Mode:           session.ModeInteractive,
		Cwd:            t.TempDir(),
		ClaudeBin:      "/bin/true",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusExited,
	})
	if err != nil {
		t.Fatalf("seeding session: %v", err)
	}

	rec := doVersioned(t, srv.Handler(), http.MethodDelete, "/v1/sessions/"+sess.ID+"?grace=5s", nil)
	assertAPIVersionHeader(t, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (200), body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got struct {
		DesiredState string `json:"desired_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.DesiredState != string(session.DesiredStopped) {
		t.Fatalf("desired_state = %q, want %q", got.DesiredState, session.DesiredStopped)
	}
}

// --- 5. Name defaulting --------------------------------------------------

// TestHandleCreateSession_DefaultNameSequence exercises defaultSessionName
// through the real POST handler. handleCreate assigns the default name and
// calls store.CreateSession (both of which succeed here) strictly before
// Registry.Spawn is reached; setUnresolvableClaudeBin makes the later
// Spawn fail fast (fork/exec: no such file or directory) without starting
// a real process, so the response itself is a 500 (CodeInternal) — this
// test asserts the actual stored name, not the HTTP response, since the
// task at hand is the naming scheme, not the spawn outcome (already
// covered elsewhere in the supervisor package and via e2e).
func TestHandleCreateSession_DefaultNameSequence(t *testing.T) {
	setUnresolvableClaudeBin(t)
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)
	cwd := t.TempDir()
	base := filepath.Base(cwd)

	// First session in cwd: expect "<base>-1".
	rec1 := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions", mustJSON(t, createSessionRequest{Cwd: cwd}))
	assertAPIVersionHeader(t, rec1)
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (spawn was expected to fail against an unresolvable claude_bin), body=%s", rec1.Code, http.StatusInternalServerError, rec1.Body.String())
	}
	want1 := base + "-1"
	if _, err := deps.Store.GetSessionByName(t.Context(), want1); err != nil {
		t.Fatalf("GetSessionByName(%q) after first create: %v, want the row to exist with the default name", want1, err)
	}

	// Second session, same cwd, still no name: expect "<base>-2" since
	// "<base>-1" is now taken.
	rec2 := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions", mustJSON(t, createSessionRequest{Cwd: cwd}))
	assertAPIVersionHeader(t, rec2)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body=%s", rec2.Code, http.StatusInternalServerError, rec2.Body.String())
	}
	want2 := base + "-2"
	if _, err := deps.Store.GetSessionByName(t.Context(), want2); err != nil {
		t.Fatalf("GetSessionByName(%q) after second create: %v, want the row to exist with the default name", want2, err)
	}
}

func TestHandleCreateSession_AppliesInteractivePermissionMode(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)
	cwd := t.TempDir()

	fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatalf("writing fake claude: %v", err)
	}
	t.Setenv("CORRAL_SESSION_CLAUDE_BIN", fakeClaude)
	t.Setenv("CORRAL_SESSION_PERMISSION_MODE", "manual")

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions", mustJSON(t, createSessionRequest{Name: "manual-mode", Cwd: cwd}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	sess, err := deps.Store.GetSessionByName(t.Context(), "manual-mode")
	if err != nil {
		t.Fatalf("GetSessionByName: %v", err)
	}
	wantTail := []string{"--permission-mode", "manual"}
	if len(sess.Argv) < len(wantTail) || !reflect.DeepEqual(sess.Argv[len(sess.Argv)-len(wantTail):], wantTail) {
		t.Fatalf("stored argv = %v, want suffix %v", sess.Argv, wantTail)
	}
}

// --- 6. Corral-Api-Version header on every response ---------------------

// TestEveryResponse_CarriesAPIVersionHeader is a belt-and-suspenders check
// across the handler methods exercised above: versionMiddleware sets the
// header before calling into the mux (server.go), so it should be present
// on literally every response including error ones. The individual tests
// above already assert this via assertAPIVersionHeader; this test picks a
// small spread of status codes (200, 400, 404, 409) to confirm the header
// survives all of them from one place.
func TestEveryResponse_CarriesAPIVersionHeader(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)
	cwd := t.TempDir()

	if _, err := deps.Store.CreateSession(t.Context(), store.CreateSessionParams{
		ID:             "44444444-4444-4444-4444-444444444444",
		Name:           "hdr-check",
		Mode:           session.ModeInteractive,
		Cwd:            cwd,
		ClaudeBin:      "/bin/true",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusExited,
	}); err != nil {
		t.Fatalf("seeding session: %v", err)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		body       []byte
		wantStatus int
	}{
		{"list (empty ok)", http.MethodGet, "/v1/sessions", nil, http.StatusOK},
		{"get known", http.MethodGet, "/v1/sessions/hdr-check", nil, http.StatusOK},
		{"get unknown -> 404", http.MethodGet, "/v1/sessions/nope", nil, http.StatusNotFound},
		{"create bad request -> 400", http.MethodPost, "/v1/sessions", []byte("not json"), http.StatusBadRequest},
		{"create name taken -> 409", http.MethodPost, "/v1/sessions", mustJSON(t, createSessionRequest{Name: "hdr-check", Cwd: cwd}), http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doVersioned(t, srv.Handler(), tt.method, tt.path, tt.body)
			assertAPIVersionHeader(t, rec)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// --- FINDING: mux-generated 405/404 responses skip the error envelope ---

// TestHandleSessions_MuxRejections_BypassEnvelope documents (does not
// assert a bug — nothing here fails) that RegisterSessions registers
// method-qualified patterns ("GET /v1/sessions", "POST /v1/sessions",
// etc., handlers_sessions.go), so a method mismatch or unmatched subpath
// is answered directly by http.ServeMux, never reaching writeError
// (errors.go). That means these responses are plain text, NOT the
// {"error":{"code","message","details"}} envelope errors.go's comment
// claims for "every non-2xx response" — unlike /v1/version
// (handlers_meta.go), which is registered path-only and does its own
// in-handler method check via writeError, so TestHandleVersion_WrongMethod
// (handlers_meta_test.go) gets a real envelope on 405. This is an
// inconsistency between handlers_meta.go and handlers_sessions.go route
// registration, reported as a finding, not fixed here.
//
// The Corral-Api-Version header is still present on these responses,
// since versionMiddleware (server.go) sets it before ever dispatching into
// the mux — so requirement 6 (every response carries the header) still
// holds even though the envelope shape does not.
func TestHandleSessions_MuxRejections_BypassEnvelope(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"PUT /v1/sessions: method not registered", http.MethodPut, "/v1/sessions", http.StatusMethodNotAllowed},
		{"PATCH /v1/sessions/{id}: method not registered", http.MethodPatch, "/v1/sessions/some-id", http.StatusMethodNotAllowed},
		{"unregistered subpath", http.MethodGet, "/v1/sessions/x/attach", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doVersioned(t, srv.Handler(), tt.method, tt.path, nil)
			assertAPIVersionHeader(t, rec)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			// This is the finding: mux rejections are plain text, not the
			// JSON error envelope.
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err == nil && env.Error.Code != "" {
				t.Fatalf("FINDING invalidated: mux rejection body decoded as a real error envelope (code=%q) — expected plain text, body=%s", env.Error.Code, rec.Body.String())
			}
		})
	}
}

// --- GET /v1/sessions/{idOrName}/events ---------------------------------

func seedEventsSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID: id, Name: id, Mode: session.ModeInteractive, Cwd: "/tmp/work",
		ClaudeBin: "/usr/local/bin/claude", Argv: []string{"/usr/local/bin/claude"},
		EnvKeys: []string{"HOME"}, SettingsPath: "/tmp/state/" + id + "/settings.json",
		SettingSources: "user,project,local", DesiredState: session.DesiredRunning,
		Status: session.StatusRunning, PID: 1234, PGID: 1234, ProcStartNs: 1, Rows: 40, Cols: 120,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestHandleSessionEvents(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)
	ctx := context.Background()

	const id = "sess-events"
	seedEventsSession(t, deps.Store, id)

	if _, err := deps.Store.AppendEvent(ctx, id, session.EventHookReceived, `{"event":"PreToolUse"}`); err != nil {
		t.Fatalf("AppendEvent 1: %v", err)
	}
	if _, err := deps.Store.AppendEvent(ctx, id, session.EventPermissionRequested, `{"tool_name":"Bash"}`); err != nil {
		t.Fatalf("AppendEvent 2: %v", err)
	}

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/sessions/"+id+"/events", nil)
	assertAPIVersionHeader(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Events []struct {
			Seq  int64           `json:"seq"`
			Ts   string          `json:"ts"`
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if len(resp.Events) != 2 {
		t.Fatalf("got %d events, want 2: %s", len(resp.Events), rec.Body.String())
	}
	// seq-ascending append order.
	if resp.Events[0].Seq >= resp.Events[1].Seq {
		t.Fatalf("events not seq-ascending: %d then %d", resp.Events[0].Seq, resp.Events[1].Seq)
	}
	if resp.Events[0].Kind != string(session.EventHookReceived) {
		t.Fatalf("event[0].kind = %q, want %q", resp.Events[0].Kind, session.EventHookReceived)
	}
	// data is embedded verbatim (never re-encoded).
	if string(resp.Events[1].Data) != `{"tool_name":"Bash"}` {
		t.Fatalf("event[1].data = %s, want verbatim payload", resp.Events[1].Data)
	}
	if resp.Events[0].Ts == "" {
		t.Fatal("event ts empty")
	}
}

func TestHandleSessionEvents_Empty(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	const id = "sess-events-empty"
	seedEventsSession(t, deps.Store, id)

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/sessions/"+id+"/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	// Empty stream must serialize as [] (not null) so clients can range safely.
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.Events == nil {
		t.Fatalf("events serialized as null, want []: %s", rec.Body.String())
	}
	if len(resp.Events) != 0 {
		t.Fatalf("got %d events, want 0", len(resp.Events))
	}
}

func TestHandleSessionEvents_UnknownSession(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/sessions/no-such/events", nil)
	assertAPIVersionHeader(t, rec)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// --- 5. POST /v1/sessions/{idOrName}/wake -------------------------------

// TestHandleWakeSession_Unknown proves the wake route is actually
// registered and routable: a typo'd pattern string in RegisterSessions
// would 404 for a reason unrelated to "session_not_found" (net/http's own
// no-matching-pattern 404, with no JSON body), which this test would catch
// via the envelope decode failing. Safe with the nil checkpointer
// newSessionsTestDeps builds: Registry.Wake resolves idOrName through
// r.Get then store.GetSession/GetSessionByName, both of which return
// store.ErrNotFound for an unknown idOrName before Wake ever calls
// r.checkpointer.Resumable.
func TestHandleWakeSession_Unknown(t *testing.T) {
	deps := newSessionsTestDeps(t)
	srv := newSessionsTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/sessions/no-such-session/wake", nil)
	assertAPIVersionHeader(t, rec)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeSessionNotFound {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeSessionNotFound)
	}
}
