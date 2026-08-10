package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/version"
)

// fakeSecretLooker is a test double for secretLooker: a plain map from
// session ID to secret, standing in for supervisor.Registry.SecretFor
// without needing a real live (spawned) session.
type fakeSecretLooker map[string]string

func (f fakeSecretLooker) SecretFor(sessionID string) (string, bool) {
	s, ok := f[sessionID]
	return s, ok
}

// stubEngine is a minimal state.Engine test double: OnHookEvent returns
// whatever Err is set to (nil by default) and counts its own calls, so
// tests can assert the engine was (or wasn't) invoked.
type stubEngine struct {
	Err       error
	CallCount int
}

func (e *stubEngine) OnHookEvent(ctx context.Context, sessionID string, ev state.HookEvent) error {
	e.CallCount++
	return e.Err
}

func (e *stubEngine) OnLifecycle(ctx context.Context, sessionID string, kind session.EventKind, data any) error {
	return nil
}

func (e *stubEngine) State(sessionID string) session.AgentState {
	return session.AgentRunning
}

func openHooksTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             "sess-known",
		Name:           "sess-known",
		Mode:           session.ModeInteractive,
		Cwd:            "/tmp/work",
		ClaudeBin:      "/usr/local/bin/claude",
		Argv:           []string{"/usr/local/bin/claude"},
		EnvKeys:        []string{"HOME"},
		SettingsPath:   "/tmp/state/sessions/sess-known/settings.json",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusRunning,
		PID:            1234,
		PGID:           1234,
		ProcStartNs:    1,
		Rows:           40,
		Cols:           120,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	return st
}

func newHooksTestServer(deps HooksDeps) *Server {
	s := New()
	s.RegisterHooks(deps)
	return s
}

func doHookRequest(t *testing.T, h http.Handler, sessionID, secret, eventName string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/events", bytes.NewReader(body))
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	if sessionID != "" {
		req.Header.Set("Corral-Session-Id", sessionID)
	}
	if secret != "" {
		req.Header.Set("Corral-Session-Secret", secret)
	}
	if eventName != "" {
		req.Header.Set("Corral-Hook-Event", eventName)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- auth ------------------------------------------------------------------

func TestHandleHookEvent_UnknownSession_403(t *testing.T) {
	st := openHooksTestStore(t)
	deps := HooksDeps{Store: st, Engine: &stubEngine{}, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	rec := doHookRequest(t, srv.Handler(), "no-such-session", "whatever", "SessionStart", []byte(`{}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeUnauthorized {
		t.Errorf("error.code = %q, want %q", env.Error.Code, CodeUnauthorized)
	}
}

func TestHandleHookEvent_WrongSecret_403(t *testing.T) {
	st := openHooksTestStore(t)
	deps := HooksDeps{Store: st, Engine: &stubEngine{}, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	rec := doHookRequest(t, srv.Handler(), "sess-known", "wrong-secret", "SessionStart", []byte(`{}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestHandleHookEvent_403BodyIsIdenticalForUnknownAndWrongSecret asserts the
// 403 response body is byte-identical whether the session is unknown or the
// secret is simply wrong — the endpoint must never leak which failure mode
// occurred.
func TestHandleHookEvent_403BodyIsIdenticalForUnknownAndWrongSecret(t *testing.T) {
	st := openHooksTestStore(t)
	deps := HooksDeps{Store: st, Engine: &stubEngine{}, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	recUnknown := doHookRequest(t, srv.Handler(), "no-such-session", "whatever", "SessionStart", []byte(`{}`))
	recWrong := doHookRequest(t, srv.Handler(), "sess-known", "wrong-secret", "SessionStart", []byte(`{}`))

	if recUnknown.Code != recWrong.Code {
		t.Fatalf("status codes differ: unknown=%d wrong=%d", recUnknown.Code, recWrong.Code)
	}
	if recUnknown.Body.String() != recWrong.Body.String() {
		t.Fatalf("403 bodies differ:\nunknown=%s\nwrong=%s", recUnknown.Body.String(), recWrong.Body.String())
	}
}

// --- happy path / engine outcomes ------------------------------------------

func TestHandleHookEvent_GoodPayload_200AndReceivedRecorded(t *testing.T) {
	st := openHooksTestStore(t)
	deps := HooksDeps{Store: st, Engine: &stubEngine{}, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	rec := doHookRequest(t, srv.Handler(), "sess-known", "correct-secret", "SessionStart", []byte(`{"hook_event_name":"SessionStart","session_id":"sess-known"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	events, err := st.ListEvents(context.Background(), "sess-known")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventHookReceived) {
		t.Fatalf("events = %v, want a %q event", eventKindsHooks(events), session.EventHookReceived)
	}
}

func TestHandleHookEvent_QueueFull_200AndDroppedRecorded(t *testing.T) {
	st := openHooksTestStore(t)
	eng := &stubEngine{Err: state.ErrQueueFull}
	deps := HooksDeps{Store: st, Engine: eng, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	rec := doHookRequest(t, srv.Handler(), "sess-known", "correct-secret", "PreToolUse", []byte(`{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	events, err := st.ListEvents(context.Background(), "sess-known")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventHookDropped) {
		t.Fatalf("events = %v, want a %q event", eventKindsHooks(events), session.EventHookDropped)
	}
	if eng.CallCount != 1 {
		t.Errorf("engine CallCount = %d, want 1", eng.CallCount)
	}
}

// --- body cap ---------------------------------------------------------------

func TestHandleHookEvent_BodyTooLarge_413(t *testing.T) {
	st := openHooksTestStore(t)
	deps := HooksDeps{Store: st, Engine: &stubEngine{}, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	big := bytes.Repeat([]byte("a"), (1<<20)+1)
	rec := doHookRequest(t, srv.Handler(), "sess-known", "correct-secret", "SessionStart", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// --- undecodable -------------------------------------------------------------

func TestHandleHookEvent_Undecodable_200AndEngineNotCalled(t *testing.T) {
	st := openHooksTestStore(t)
	eng := &stubEngine{}
	deps := HooksDeps{Store: st, Engine: eng, Secrets: fakeSecretLooker{"sess-known": "correct-secret"}}
	srv := newHooksTestServer(deps)

	rec := doHookRequest(t, srv.Handler(), "sess-known", "correct-secret", "SessionStart", []byte(`{`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if eng.CallCount != 0 {
		t.Errorf("engine CallCount = %d, want 0 (undecodable payload must never reach the engine)", eng.CallCount)
	}

	events, err := st.ListEvents(context.Background(), "sess-known")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventHookUndecodable) {
		t.Fatalf("events = %v, want a %q event", eventKindsHooks(events), session.EventHookUndecodable)
	}
}

// --- helpers shared only within this file -----------------------------------

func hasEventKind(events []*session.Event, kind session.EventKind) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func eventKindsHooks(events []*session.Event) []session.EventKind {
	kinds := make([]session.EventKind, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	return kinds
}
