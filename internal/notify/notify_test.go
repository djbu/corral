package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// --- infra ------------------------------------------------------------

func openTestStore(t *testing.T, clk *clocktest.FakeClock) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "corral-notify-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "corral.db"), clk)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func makeSession(t *testing.T, st *store.Store, id, name, cwd string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             id,
		Name:           name,
		Mode:           session.ModeInteractive,
		Cwd:            cwd,
		ClaudeBin:      "/usr/local/bin/claude",
		Argv:           []string{"/usr/local/bin/claude"},
		EnvKeys:        []string{"HOME"},
		SettingsPath:   "/tmp/s/" + id + "/settings.json",
		SettingSources: "user",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusRunning,
		PID:            1234,
		PGID:           1234,
		ProcStartNs:    1,
		Rows:           40,
		Cols:           120,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func blockReason(summary string) state.BlockedReason {
	return state.BlockedReason{
		Kind:     "permission",
		Summary:  summary,
		ToolName: "Bash",
	}
}

func eventKinds(t *testing.T, st *store.Store, sessionID string) []session.EventKind {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	out := make([]session.EventKind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func findEvent(t *testing.T, st *store.Store, sessionID string, kind session.EventKind) map[string]any {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, e := range evs {
		if e.Kind == kind {
			var m map[string]any
			if err := json.Unmarshal([]byte(e.DataJSON), &m); err != nil {
				t.Fatalf("event %s data not JSON: %v", kind, err)
			}
			return m
		}
	}
	t.Fatalf("no %s event for %s; got %v", kind, sessionID, eventKinds(t, st, sessionID))
	return nil
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// recordingBackend captures every Notification Send received and returns a
// configurable error.
type recordingBackend struct {
	name string
	err  error
	mu   sync.Mutex
	got  []Notification
}

func (b *recordingBackend) Name() string { return b.name }
func (b *recordingBackend) Send(_ context.Context, n Notification) error {
	b.mu.Lock()
	b.got = append(b.got, n)
	b.mu.Unlock()
	return b.err
}
func (b *recordingBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.got)
}

// --- tests ------------------------------------------------------------

func TestDispatcherHappyPathBothBackends(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "api-refactor", "/home/x/code/x")

	// Real ntfy + webhook mock servers.
	var ntfyBody string
	var ntfyTitle, ntfyTags string
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ntfyBody = string(b)
		ntfyTitle = r.Header.Get("Title")
		ntfyTags = r.Header.Get("Tags")
		w.WriteHeader(200)
	}))
	defer ntfy.Close()

	var hookPayload Notification
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&hookPayload)
		w.WriteHeader(204)
	}))
	defer webhook.Close()

	client := NewHTTPClient(5 * time.Second)
	backends := []Backend{
		NewNtfyBackend(ntfy.URL, "mytopic", "", "high", client),
		NewWebhookBackend(webhook.URL, map[string]string{"X-Corral": "1"}, client),
	}
	d := New(backends, Options{On: []string{"blocked"}, Timeout: 5 * time.Second, Retries: 3}, fc, st, nil)
	jobDone := make(chan struct{}, 4)
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()
	defer d.Close()

	if err := d.NotifyBlocked("s1", blockReason("permission to run npm test")); err != nil {
		t.Fatalf("NotifyBlocked: %v", err)
	}
	recv(t, jobDone, "job completion")

	if ntfyTitle != "corral/api-refactor blocked" {
		t.Errorf("ntfy Title = %q, want %q", ntfyTitle, "corral/api-refactor blocked")
	}
	if ntfyTags != "warning" {
		t.Errorf("ntfy Tags = %q, want warning", ntfyTags)
	}
	if ntfyBody == "" || !strings.Contains(ntfyBody, "permission to run npm test") {
		t.Errorf("ntfy body %q missing summary", ntfyBody)
	}
	if !strings.Contains(ntfyBody, "answer: corral answer api-refactor") {
		t.Errorf("ntfy body %q missing answer hint", ntfyBody)
	}
	if hookPayload.SessionName != "api-refactor" || hookPayload.Event != "blocked" {
		t.Errorf("webhook payload = %+v, want name=api-refactor event=blocked", hookPayload)
	}
	if hookPayload.Cwd != "/home/x/code/x" {
		t.Errorf("webhook Cwd = %q, want /home/x/code/x", hookPayload.Cwd)
	}

	// Two notify.sent events (one per backend), each attempts=1.
	kinds := eventKinds(t, st, "s1")
	if n := countKind(kinds, session.EventNotifySent); n != 2 {
		t.Fatalf("notify.sent count = %d, want 2 (kinds=%v)", n, kinds)
	}
	sent := findEvent(t, st, "s1", session.EventNotifySent)
	if sent["attempts"].(float64) != 1 {
		t.Errorf("attempts = %v, want 1", sent["attempts"])
	}
}

func TestDispatcherRedactsBeforeEgress(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "leaky", "/tmp/x")

	rec := &recordingBackend{name: "ntfy"}
	d := New([]Backend{rec}, Options{Timeout: time.Second, Retries: 0}, fc, st, nil)
	jobDone := make(chan struct{}, 1)
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()
	defer d.Close()

	// A secret-shaped assignment must be redacted in the egressed body.
	d.NotifyBlocked("s1", blockReason("permission to run export token=abcdef0123456789abcd"))
	recv(t, jobDone, "job")

	body := rec.got[0].Detail
	if strings.Contains(body, "abcdef0123456789abcd") {
		t.Errorf("Detail leaked the secret: %q", body)
	}
	if !strings.Contains(body, "«redacted:assignment»") {
		t.Errorf("Detail missing redaction marker: %q", body)
	}
	// The redaction must be recorded in notify.sent (never silent).
	sent := findEvent(t, st, "s1", session.EventNotifySent)
	reds, _ := sent["redactions"].([]any)
	if len(reds) == 0 || reds[0] != "assignment" {
		t.Errorf("notify.sent redactions = %v, want [assignment]", sent["redactions"])
	}
}

// TestDispatcherBackoffDrivenByClock is the concurrency crux: an always-500
// backend must be retried exactly maxAttempts times with the backoff schedule
// advanced by the FakeClock, never a real sleep. The afterArm barrier is
// load-bearing — it guarantees the backoff timer's waiter is registered before
// the test advances the clock.
func TestDispatcherBackoffDrivenByClock(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "flaky", "/tmp/x")

	var count int32
	reqCh := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		reqCh <- struct{}{}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	d := New([]Backend{NewNtfyBackend(srv.URL, "t", "", "", client)},
		Options{Timeout: 5 * time.Second, Retries: 3}, fc, st, nil)
	armCh := make(chan struct{})
	jobDone := make(chan struct{}, 1)
	d.afterArm = func() { armCh <- struct{}{} }
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()
	defer d.Close()

	d.NotifyBlocked("s1", blockReason("permission to run npm test"))

	// 4 sends total (1 initial + 3 retries); backoff 1s/4s/16s between them.
	backoffs := []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}
	recv(t, reqCh, "request 1")
	for i, b := range backoffs {
		recv(t, armCh, "backoff armed")
		fc.Advance(b)
		recv(t, reqCh, "request after backoff")
		_ = i
	}
	recv(t, jobDone, "job completion")

	if got := atomic.LoadInt32(&count); got != 4 {
		t.Fatalf("server saw %d requests, want 4", got)
	}
	failed := findEvent(t, st, "s1", session.EventNotifyFailed)
	if failed["attempts"].(float64) != 4 {
		t.Errorf("notify.failed attempts = %v, want 4", failed["attempts"])
	}
}

func TestDispatcher4xxNotRetried(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "bad", "/tmp/x")

	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(400)
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	d := New([]Backend{NewNtfyBackend(srv.URL, "t", "", "", client)},
		Options{Timeout: 5 * time.Second, Retries: 3}, fc, st, nil)
	jobDone := make(chan struct{}, 1)
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()
	defer d.Close()

	d.NotifyBlocked("s1", blockReason("x"))
	recv(t, jobDone, "job")

	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (4xx must not retry)", got)
	}
	failed := findEvent(t, st, "s1", session.EventNotifyFailed)
	if failed["attempts"].(float64) != 1 {
		t.Errorf("attempts = %v, want 1", failed["attempts"])
	}
}

func TestDispatcherDebounce(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "dup", "/tmp/x")

	rec := &recordingBackend{name: "ntfy"}
	d := New([]Backend{rec}, Options{Timeout: time.Second, Retries: 0, Debounce: 30 * time.Second}, fc, st, nil)
	jobDone := make(chan struct{}, 2)
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()
	defer d.Close()

	// Two identical blocks within the debounce window (clock not advanced).
	d.NotifyBlocked("s1", blockReason("permission to run npm test"))
	d.NotifyBlocked("s1", blockReason("permission to run npm test"))
	recv(t, jobDone, "job 1")
	recv(t, jobDone, "job 2")

	if rec.calls() != 1 {
		t.Fatalf("backend Send calls = %d, want 1 (second debounced)", rec.calls())
	}
	kinds := eventKinds(t, st, "s1")
	if countKind(kinds, session.EventNotifySent) != 1 {
		t.Errorf("notify.sent count = %d, want 1 (kinds=%v)", countKind(kinds, session.EventNotifySent), kinds)
	}
	if countKind(kinds, session.EventNotifySuppressed) != 1 {
		t.Errorf("notify.suppressed count = %d, want 1 (kinds=%v)", countKind(kinds, session.EventNotifySuppressed), kinds)
	}
}

func TestDispatcherQueueFullDrops(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "flood", "/tmp/x")

	// Do NOT Start the worker, so nothing drains the queue: the 129th enqueue
	// hits the bounded channel's default branch and drops.
	d := New(nil, Options{Timeout: time.Second}, fc, st, nil)
	for i := 0; i < queueCap; i++ {
		d.NotifyBlocked("s1", blockReason("x"))
	}
	// One more must drop.
	d.NotifyBlocked("s1", blockReason("x"))

	kinds := eventKinds(t, st, "s1")
	if n := countKind(kinds, session.EventNotifyDropped); n != 1 {
		t.Fatalf("notify.dropped count = %d, want 1 (kinds=%v)", n, kinds)
	}
}

// TestDispatcherNeverBlocksOnSlowBackend proves the structural guarantee:
// NotifyBlocked returns immediately even while a backend hangs mid-Send.
func TestDispatcherNeverBlocksOnSlowBackend(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "slow", "/tmp/x")

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	slow := backendFunc{name: "slow", send: func() error {
		entered <- struct{}{}
		<-release // hang until the test releases
		return nil
	}}
	d := New([]Backend{slow}, Options{Timeout: time.Second, Retries: 0}, fc, st, nil)
	jobDone := make(chan struct{}, 1)
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()

	start := time.Now()
	d.NotifyBlocked("s1", blockReason("x"))
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("NotifyBlocked took %v; it must not block on delivery", elapsed)
	}
	recv(t, entered, "backend entered Send")
	// Backend is now hung inside Send; NotifyBlocked already returned. Release
	// so the worker can finish and Close can join cleanly.
	close(release)
	recv(t, jobDone, "job")
	d.Close()
}

// TestDispatcherCloseCancelsInflightSend proves the production shutdown fix:
// a backend blocked inside Send that honors its context (as every real
// http.Client-backed backend does) is aborted by Close cancelling d.ctx.
// Unlike the slow-backend test above, nothing releases the backend — if Close
// did not cancel, wg.Wait would block forever and this test would time out.
func TestDispatcherCloseCancelsInflightSend(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	makeSession(t, st, "s1", "hang", "/tmp/x")

	entered := make(chan struct{}, 1)
	// Blocks until its context is cancelled — models a black-holed host held
	// open by the http.Client until the request context is cancelled.
	ctxAware := backendFunc{name: "hang", send: nil}
	ctxAware.sendCtx = func(ctx context.Context) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	d := New([]Backend{ctxAware}, Options{Timeout: time.Hour, Retries: 0}, fc, st, nil)
	d.Start()

	d.NotifyBlocked("s1", blockReason("x"))
	recv(t, entered, "backend entered Send")

	// Backend is hung inside Send with a 1h per-attempt timeout. Close must
	// cancel d.ctx and return promptly; a hang here fails the test via the
	// package test timeout.
	done := make(chan struct{})
	go func() { d.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return; in-flight Send was not cancelled")
	}
}

func TestDispatcherSessionNotFoundDegrades(t *testing.T) {
	fc := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, fc)
	// No session row created for "ghost".

	rec := &recordingBackend{name: "ntfy"}
	d := New([]Backend{rec}, Options{Timeout: time.Second, Retries: 0}, fc, st, nil)
	jobDone := make(chan struct{}, 1)
	d.afterJob = func() { jobDone <- struct{}{} }
	d.Start()
	defer d.Close()

	d.NotifyBlocked("ghost", blockReason("permission to run npm test"))
	recv(t, jobDone, "job")

	if rec.calls() != 1 {
		t.Fatalf("backend Send calls = %d, want 1 (must still send degraded)", rec.calls())
	}
	if got := rec.got[0].SessionName; got != "ghost" {
		t.Errorf("degraded SessionName = %q, want the session ID %q", got, "ghost")
	}
}

// --- small helpers ----------------------------------------------------

type backendFunc struct {
	name string
	send func() error // ctx-oblivious variant
	// sendCtx, when set, takes precedence over send and receives the per-attempt
	// context — used to model a backend that only unblocks on cancellation.
	sendCtx func(context.Context) error
}

func (b backendFunc) Name() string { return b.name }
func (b backendFunc) Send(ctx context.Context, _ Notification) error {
	if b.sendCtx != nil {
		return b.sendCtx(ctx)
	}
	return b.send()
}

func countKind(kinds []session.EventKind, want session.EventKind) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
}
