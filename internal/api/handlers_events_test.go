package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
)

// --- test doubles -----------------------------------------------------------

// signalingClock wraps a *clocktest.FakeClock and signals created the
// moment NewTicker is called. handleStream calls Broker.Subscribe() before
// Clock.NewTicker(), so a receive on created is proof the handler has
// already subscribed — the synchronization point every test below needs
// before it can safely publish (or Advance the fake clock) without racing
// the handler's own startup.
type signalingClock struct {
	*clocktest.FakeClock
	created chan struct{}
}

func newSignalingClock(fc *clocktest.FakeClock) *signalingClock {
	return &signalingClock{FakeClock: fc, created: make(chan struct{}, 1)}
}

func (c *signalingClock) NewTicker(d time.Duration) clock.Ticker {
	t := c.FakeClock.NewTicker(d)
	select {
	case c.created <- struct{}{}:
	default:
	}
	return t
}

func waitSubscribed(t *testing.T, created <-chan struct{}) {
	t.Helper()
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not subscribe (create its heartbeat ticker) in time")
	}
}

// pipeResponseWriter is a minimal http.ResponseWriter over an io.Writer
// (an *io.PipeWriter in every test below), so handleStream can be invoked
// directly — no real listener needed — while still giving the test genuine
// backpressure: an io.PipeWriter.Write blocks until something reads the
// other end, exactly like a real, unread network connection would. Flush
// mirrors the real net/http *response's behavior of implicitly writing the
// header on first successful flush, since handleStream deliberately never
// calls WriteHeader itself (see its doc comment).
type pipeResponseWriter struct {
	header      http.Header
	w           io.Writer
	wroteHeader bool
	code        int
}

func newPipeResponseWriter(w io.Writer) *pipeResponseWriter {
	return &pipeResponseWriter{header: make(http.Header), w: w}
}

func (p *pipeResponseWriter) Header() http.Header { return p.header }

func (p *pipeResponseWriter) WriteHeader(code int) {
	if p.wroteHeader {
		return
	}
	p.wroteHeader = true
	p.code = code
}

func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if !p.wroteHeader {
		p.WriteHeader(http.StatusOK)
	}
	return p.w.Write(b)
}

func (p *pipeResponseWriter) Flush() {
	if !p.wroteHeader {
		p.WriteHeader(http.StatusOK)
	}
}

// readFrame reads one SSE frame (one or more non-empty lines terminated by
// a blank line) from r, or fails the test if none arrives within a few
// seconds. Each returned line has its trailing "\n" (and, for a "\r\n"
// stream, "\r") stripped.
func readFrame(t *testing.T, r *bufio.Reader) []string {
	t.Helper()
	type result struct {
		lines []string
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				resCh <- result{lines, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				resCh <- result{lines, nil}
				return
			}
			lines = append(lines, line)
		}
	}()
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("reading SSE frame: %v", res.err)
		}
		return res.lines
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an SSE frame")
		return nil
	}
}

// --- 1. event delivery + context cancellation -------------------------------

func TestHandleStream_EventDeliveryAndContextCancellation(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sc := newSignalingClock(fc)
	broker := NewBroker()
	deps := EventsDeps{Broker: broker, Clock: sc}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil).WithContext(ctx)

	handlerDone := make(chan struct{})
	go func() {
		deps.handleStream(w, req)
		close(handlerDone)
	}()

	waitSubscribed(t, sc.created)

	ev := session.Event{Seq: 1, SessionID: "s1", TsMs: 123, Kind: session.EventSessionCreated, DataJSON: `{"a":1}`}
	broker.PublishEvent(ev)

	r := bufio.NewReader(pr)
	lines := readFrame(t, r)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("unexpected frame: %v", lines)
	}
	var got frameJSON
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "data: ")), &got); err != nil {
		t.Fatalf("unmarshal frame JSON: %v (line=%q)", err, lines[0])
	}
	if got.Seq != ev.Seq || got.SessionID != ev.SessionID || got.TsMs != ev.TsMs ||
		got.Kind != string(ev.Kind) || string(got.Data) != ev.DataJSON {
		t.Fatalf("frame = %+v, want to encode %+v", got, ev)
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return promptly after context cancellation")
	}
}

// --- 2. heartbeat (FakeClock-driven) ----------------------------------------

func TestHandleStream_Heartbeat(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sc := newSignalingClock(fc)
	broker := NewBroker()
	deps := EventsDeps{Broker: broker, Clock: sc}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil).WithContext(ctx)

	go deps.handleStream(w, req)

	waitSubscribed(t, sc.created)

	// Advancing by exactly the heartbeat interval must fire the ticker;
	// clocktest.FakeClock only ever fires tickers already registered by the
	// time Advance runs, which waitSubscribed above guarantees.
	fc.Advance(heartbeatInterval)

	r := bufio.NewReader(pr)
	lines := readFrame(t, r)
	if len(lines) != 1 || lines[0] != ": ping" {
		t.Fatalf("heartbeat frame = %v, want [\": ping\"]", lines)
	}
}

// --- 3. resync frame rendering ----------------------------------------------

func TestHandleStream_ResyncFrameRendering(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sc := newSignalingClock(fc)
	broker := NewBroker()
	deps := EventsDeps{Broker: broker, Clock: sc}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil).WithContext(ctx)

	go deps.handleStream(w, req)
	waitSubscribed(t, sc.created)

	r := bufio.NewReader(pr)

	// Fill the subscriber's channel (subBufferSize=64 in broker.go) well
	// past capacity without draining, so it overflows and the subscriber
	// is marked stale. Nothing here is read yet, so however many of these
	// land in the buffer vs. get dropped is scheduler-dependent — the only
	// thing this loop needs to guarantee is "well past 64", which 200
	// comfortably is.
	for i := int64(0); i < 200; i++ {
		broker.PublishEvent(session.Event{Seq: i, Kind: session.EventSessionCreated, DataJSON: "{}"})
	}

	// Drain the batch of normal frames guaranteed to already be sitting
	// ahead of anything else once the flood above has overflowed the
	// subscriber. This count (64, the buffer's capacity per broker.go) is
	// safe to block on unconditionally regardless of scheduling: Go's
	// chansend direct-handoff-to-parked-receiver optimization
	// (runtime/chan.go) may or may not have delivered the very first
	// flood event straight to the handler ahead of ever touching the
	// buffer, depending on whether the handler had already registered as
	// a waiting receiver at that instant — a race this test does not,
	// and need not, control — but either way the buffer itself holds at
	// least these 64 normal frames in FIFO order before anything else.
	for i := 0; i < 64; i++ {
		lines := readFrame(t, r)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
			t.Fatalf("frame %d: expected a normal data frame, got %v", i, lines)
		}
	}

	// Trigger delivery of the subscriber's owed resync: PublishEvent only
	// ever attempts this from within a fresh publish call, never
	// automatically when a slot frees up (see broker.go).
	broker.PublishEvent(session.Event{Seq: 9000, Kind: session.EventSessionCreated, DataJSON: "{}"})

	// If the direct-handoff race above went the other way, exactly one
	// more normal frame (the 65th) was already queued ahead of the
	// resync we just triggered; otherwise the resync is the very next
	// frame. Either is correct — what must NOT happen is more than one
	// extra normal frame, or the resync failing to appear at all.
	lines := readFrame(t, r)
	if len(lines) == 1 && strings.HasPrefix(lines[0], "data: ") {
		lines = readFrame(t, r)
	}
	if len(lines) != 2 || lines[0] != "event: resync" || lines[1] != "data: {}" {
		t.Fatalf("resync frame = %v, want [\"event: resync\" \"data: {}\"]", lines)
	}

	// Normal delivery must resume immediately after the resync.
	broker.PublishEvent(session.Event{Seq: 9001, Kind: session.EventSessionCreated, DataJSON: "{}"})
	lines = readFrame(t, r)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("post-resync frame = %v, want a normal data frame", lines)
	}
}

// --- 4. auth: no token / valid ?token= / header wins over bogus ?token= -----

func TestHandleStream_Auth_QueryTokenFallback(t *testing.T) {
	f := newFakeAuthStore(t)
	plaintext := mintToken(t, f)

	broker := NewBroker()
	fc := clocktest.NewFake(time.Now())
	s := New()
	s.RegisterEvents(EventsDeps{Broker: broker, Clock: fc})
	srv := httptest.NewServer(s.AuthenticatedHandler(f, nil, nil))
	defer srv.Close()

	client := &http.Client{}

	// No credential at all: 401, never reaches the stream.
	resp, err := client.Get(srv.URL + "/v1/events/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-credential status = %d, want 401", resp.StatusCode)
	}

	// A valid ?token=... (EventSource can't set headers) authenticates.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, srv.URL+"/v1/events/stream?token="+plaintext, nil)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("Do (query token): %v", err)
	}
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("?token= status = %d, want 200", resp1.StatusCode)
	}
	resp1.Body.Close()
	cancel1()

	// A valid Authorization header wins even over a bogus ?token=.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, srv.URL+"/v1/events/stream?token=bogus-token-value", nil)
	req2.Header.Set("Authorization", "Bearer "+plaintext)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("Do (header + bogus query): %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("header-wins status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()
	cancel2()
}

// --- 4b. a bogus ?token= is rejected, and never durably persisted -----------

// TestHandleStream_Auth_BogusQueryTokenNotLogged closes the gap
// TestHandleStream_Auth_QueryTokenFallback leaves open: that test's only
// failing case sends no credential at all, and its query-only case sends a
// genuinely valid token, so neither exercises "?token= is present, wrong,
// and the ONLY credential offered" — the one case where a failed request's
// full URL (query string and all) is most likely to end up somewhere it
// shouldn't. bearerAuth's doc comment (middleware_auth.go) promises the
// durable token.unauthorized event it appends on every failure "never
// carrying token material, only the fact of the attempt" — this asserts
// that promise holds for query-param auth specifically, not just the
// Authorization-header path TestBearerAuth_UnauthorizedEventCarriesNoTokenMaterial
// already covers.
func TestHandleStream_Auth_BogusQueryTokenNotLogged(t *testing.T) {
	f := newFakeAuthStore(t)

	broker := NewBroker()
	fc := clocktest.NewFake(time.Now())
	s := New()
	s.RegisterEvents(EventsDeps{Broker: broker, Clock: fc})
	srv := httptest.NewServer(s.AuthenticatedHandler(f, nil, nil))
	defer srv.Close()

	const bogus = "crl_definitely-not-a-real-token"
	resp, err := http.Get(srv.URL + "/v1/events/stream?token=" + bogus)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus ?token=-only status = %d, want 401", resp.StatusCode)
	}

	if len(f.events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(f.events))
	}
	ev := f.events[0]
	if ev.Kind != session.EventTokenUnauthorized {
		t.Fatalf("kind = %q, want %q", ev.Kind, session.EventTokenUnauthorized)
	}
	if strings.Contains(ev.DataJSON, bogus) {
		t.Fatalf("event data JSON contains the attempted token: %s", ev.DataJSON)
	}
	if strings.Contains(ev.DataJSON, "/v1/events/stream") || strings.Contains(ev.DataJSON, "token=") {
		t.Fatalf("event data JSON leaks the request URI/query string: %s", ev.DataJSON)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(ev.DataJSON), &data); err != nil {
		t.Fatalf("event data JSON does not parse: %v (%s)", err, ev.DataJSON)
	}
	if _, ok := data["remote_addr"]; !ok {
		t.Fatalf("event data JSON missing remote_addr: %s", ev.DataJSON)
	}
	if len(data) != 1 {
		t.Fatalf("event data JSON has extra fields beyond remote_addr: %s", ev.DataJSON)
	}
}

// --- 5. version-handshake exemption is scoped to this one route ------------

func TestEventsStream_VersionHandshakeExemption(t *testing.T) {
	broker := NewBroker()
	fc := clocktest.NewFake(time.Now())
	s := New()
	s.RegisterEvents(EventsDeps{Broker: broker, Clock: fc})
	s.Handle("/v1/version", authProbe)

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	client := &http.Client{}

	// /v1/events/stream: no Corral-Api-Version header, still 200 (the one
	// exempted route — EventSource cannot set custom headers).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events/stream", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events/stream without version header: status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	cancel()

	// Every other route is unaffected: still 400 without the header.
	resp2, err := client.Get(srv.URL + "/v1/version")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("/v1/version without version header: status = %d, want 400", resp2.StatusCode)
	}
}
