package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/version"
)

func newTestServer(t *testing.T, deps MetaDeps) *Server {
	t.Helper()
	s := New()
	s.RegisterMeta(deps)
	return s
}

func doVersioned(t *testing.T, h http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func openMetaTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestHandleVersion(t *testing.T) {
	st := openMetaTestStore(t)
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC))
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	deps := MetaDeps{Store: st, Clock: fc, StartedAt: startedAt, DefaultGrace: time.Second}
	srv := newTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/version", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		DaemonVersion string `json:"daemon_version"`
		APIVersion    int    `json:"api_version"`
		SchemaVersion int    `json:"schema_version"`
		PID           int    `json:"pid"`
		UptimeMs      int64  `json:"uptime_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.APIVersion != version.APIVersion {
		t.Fatalf("api_version = %d, want %d", got.APIVersion, version.APIVersion)
	}
	if got.UptimeMs != 5000 {
		t.Fatalf("uptime_ms = %d, want 5000", got.UptimeMs)
	}
	if got.PID <= 0 {
		t.Fatalf("pid = %d, want positive", got.PID)
	}
}

func TestHandleVersion_WrongMethod(t *testing.T) {
	st := openMetaTestStore(t)
	deps := MetaDeps{Store: st, Clock: clocktest.NewFake(time.Now())}
	srv := newTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/version", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleConfig(t *testing.T) {
	st := openMetaTestStore(t)
	deps := MetaDeps{Store: st, Clock: clocktest.NewFake(time.Now())}
	srv := newTestServer(t, deps)

	dir := t.TempDir()
	rec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/config?cwd="+dir, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
}

func TestHandleShutdown(t *testing.T) {
	st := openMetaTestStore(t)

	var gotGrace time.Duration
	called := make(chan struct{})
	deps := MetaDeps{
		Store:        st,
		Clock:        clocktest.NewFake(time.Now()),
		DefaultGrace: 7 * time.Second,
		RequestShutdown: func(grace time.Duration) {
			gotGrace = grace
			close(called)
		},
	}
	srv := newTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/daemon/shutdown", []byte(`{"grace":"3s"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatalf("RequestShutdown was never called")
	}
	if gotGrace != 3*time.Second {
		t.Fatalf("grace passed to RequestShutdown = %v, want 3s", gotGrace)
	}
}

func TestHandleShutdown_DefaultGraceWhenOmitted(t *testing.T) {
	st := openMetaTestStore(t)

	var gotGrace time.Duration
	called := make(chan struct{})
	deps := MetaDeps{
		Store:        st,
		Clock:        clocktest.NewFake(time.Now()),
		DefaultGrace: 7 * time.Second,
		RequestShutdown: func(grace time.Duration) {
			gotGrace = grace
			close(called)
		},
	}
	srv := newTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/daemon/shutdown", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatalf("RequestShutdown was never called")
	}
	if gotGrace != 7*time.Second {
		t.Fatalf("grace passed to RequestShutdown = %v, want default 7s", gotGrace)
	}
}

func TestHandleShutdown_InvalidGrace(t *testing.T) {
	st := openMetaTestStore(t)
	deps := MetaDeps{Store: st, Clock: clocktest.NewFake(time.Now()), DefaultGrace: time.Second}
	srv := newTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/daemon/shutdown", []byte(`{"grace":"not-a-duration"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}
