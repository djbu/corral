package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djbu/corral/internal/answer"
	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

// --- reply test infra --------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeCall records one InputWriter.WriteInput call.
type writeCall struct {
	id string
	b  []byte
}

// fakeInputWriter stands in for *supervisor.Registry. It records every
// delivery and (optionally) returns a per-call error to exercise the not-live
// half of gate #3 without importing the supervisor package.
type fakeInputWriter struct {
	mu       sync.Mutex
	calls    []writeCall
	ch       chan writeCall
	attempts chan string // fires on every call (before errFor); nil unless a test opts in
	errFor   func(id string) error
}

func newFakeWriter() *fakeInputWriter {
	return &fakeInputWriter{ch: make(chan writeCall, 32)}
}

func (w *fakeInputWriter) WriteInput(_ context.Context, id string, b []byte) error {
	if w.attempts != nil {
		w.attempts <- id
	}
	if w.errFor != nil {
		if err := w.errFor(id); err != nil {
			return err
		}
	}
	c := writeCall{id: id, b: append([]byte(nil), b...)}
	w.mu.Lock()
	w.calls = append(w.calls, c)
	w.mu.Unlock()
	w.ch <- c
	return nil
}

func (w *fakeInputWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

// setAgentState forces a session's durable agent_state, so a test can put it
// in "blocked" (or not) without running the engine.
func setAgentState(t *testing.T, st *store.Store, id string, s session.AgentState) {
	t.Helper()
	if _, err := st.UpdateSession(context.Background(), id, func(sess *session.Session) {
		sess.AgentState = s
	}); err != nil {
		t.Fatalf("UpdateSession(%s): %v", id, err)
	}
}

// streamServer serves one ntfy-style /json long-poll: it emits an "open"
// control event, then one "message" event per msg, flushes, and holds the
// connection open until the request is cancelled (as a real long-poll does).
// It optionally requires a Bearer token, and returns status if non-zero
// instead of streaming. reqs counts connections (to detect reconnect loops).
func streamServer(t *testing.T, wantToken string, status int, msgs []string) (*httptest.Server, *int32) {
	t.Helper()
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"event":"open"}`+"\n")
		for _, m := range msgs {
			line, _ := json.Marshal(map[string]string{"event": "message", "message": m})
			w.Write(line)
			io.WriteString(w, "\n")
		}
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func answeredEvents(t *testing.T, st *store.Store, id string) []map[string]any {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("ListEvents(%s): %v", id, err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Kind != session.EventSessionAnswered {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(e.DataJSON), &m); err != nil {
			t.Fatalf("answered data %q: %v", e.DataJSON, err)
		}
		out = append(out, m)
	}
	return out
}

// waitAnswered polls for at least want session.answered events on id. The
// accept path writes the audit event after WriteInput returns, so a test that
// syncs on WriteInput (via writer.ch) can observe the store a beat before the
// append lands; polling closes that window without a fixed sleep.
func waitAnswered(t *testing.T, st *store.Store, id string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := answeredEvents(t, st, id)
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- tests -------------------------------------------------------------

// shellPayload is the security case: text with shell metacharacters that must
// reach the PTY byte-for-byte, proving the reply path never touches a shell.
const shellPayload = "`; rm -rf / #$(whoami)`&& echo pwned\""

// TestReplySubscriberGates drives every gate through one stream: malformed
// input, an unknown session, and a non-blocked session are all dropped, while
// a message to a live+blocked session is delivered verbatim and audited. The
// three rejects precede the single accept, so the accept's WriteInput is a
// reliable sync point: when it fires, the rejects have already been processed.
func TestReplySubscriberGates(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, clk)
	makeSession(t, st, "id-blocked", "blocked-sess", "/tmp/b")
	makeSession(t, st, "id-working", "working-sess", "/tmp/w")
	setAgentState(t, st, "id-blocked", session.AgentBlocked)
	setAgentState(t, st, "id-working", session.AgentWorking)
	if _, err := st.UpdateSession(context.Background(), "id-blocked", func(s *session.Session) {
		s.BlockedReasonJSON = `{"kind":"permission","hook_seq":41}`
	}); err != nil {
		t.Fatalf("UpdateSession blocked reason: %v", err)
	}

	msgs := []string{
		"onlyname",                     // malformed: no text
		"ghost-sess hello",             // unknown session
		"working-sess do the thing",    // exists+live but not blocked
		"blocked-sess " + shellPayload, // accepted
	}
	srv, reqs := streamServer(t, "tok", 0, msgs)

	writer := newFakeWriter()
	sub := NewReplySubscriber(ReplyConfig{Server: srv.URL, Topic: "reply", Token: "tok"}, st, writer, clk, discardLogger())
	sub.Start()
	defer sub.Close()

	select {
	case c := <-writer.ch:
		if c.id != "id-blocked" {
			t.Fatalf("delivered to %q, want id-blocked", c.id)
		}
		want, _ := answer.Encode(shellPayload, "", true)
		if !bytes.Equal(c.b, want) {
			t.Fatalf("delivered bytes = %q, want %q (shell metachars must pass through verbatim)", c.b, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the accepted reply to be delivered")
	}

	// The rejects produced no delivery: exactly one WriteInput total.
	if n := writer.count(); n != 1 {
		t.Fatalf("WriteInput called %d times, want 1 (rejects must not deliver)", n)
	}
	if r := atomic.LoadInt32(reqs); r != 1 {
		t.Fatalf("server saw %d connections, want 1", r)
	}

	// Only the accepted reply is audited, and it carries via:"ntfy" + the raw
	// text length (not the redacted length).
	if got := answeredEvents(t, st, "id-working"); len(got) != 0 {
		t.Fatalf("working-sess has %d session.answered events, want 0", len(got))
	}
	ans := waitAnswered(t, st, "id-blocked", 1)
	if len(ans) != 1 {
		t.Fatalf("blocked-sess has %d session.answered events, want 1", len(ans))
	}
	if ans[0]["via"] != "ntfy" {
		t.Fatalf("session.answered via = %v, want ntfy", ans[0]["via"])
	}
	if got, want := ans[0]["len"], float64(len(shellPayload)); got != want {
		t.Fatalf("session.answered len = %v, want %v", got, want)
	}
	if got, want := ans[0]["permission_request_seq"], float64(41); got != want {
		t.Fatalf("session.answered permission_request_seq = %v, want %v", got, want)
	}
}

// TestReplySubscriberNotLiveDropped covers the live half of gate #3: the
// session row says blocked, but WriteInput reports it is not live (a race lost
// to teardown). No audit event is written.
func TestReplySubscriberNotLiveDropped(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, clk)
	makeSession(t, st, "id-blocked", "blocked-sess", "/tmp/b")
	setAgentState(t, st, "id-blocked", session.AgentBlocked)

	srv, _ := streamServer(t, "tok", 0, []string{"blocked-sess hi", "blocked-sess again"})
	writer := newFakeWriter()
	writer.attempts = make(chan string, 8)
	writer.errFor = func(string) error { return errors.New("not live") }

	sub := NewReplySubscriber(ReplyConfig{Server: srv.URL, Topic: "reply", Token: "tok"}, st, writer, clk, discardLogger())
	sub.Start()
	defer sub.Close()

	// Both messages pass gate #3's row check and reach WriteInput, which reports
	// each as not-live. Waiting on the attempts signal (not a fixed sleep) proves
	// both were processed; the not-live error path must audit neither.
	for i := 0; i < 2; i++ {
		select {
		case <-writer.attempts:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d delivery attempts, want 2", i)
		}
	}
	if got := answeredEvents(t, st, "id-blocked"); len(got) != 0 {
		t.Fatalf("failed delivery audited %d times, want 0", len(got))
	}
}

// TestReplySubscriberAuthStops is the blocking security property: a
// non-retryable auth rejection (403) on the reply topic must STOP the
// subscriber, not feed the reconnect loop. A subscriber that looped on 403
// would report itself armed while doing nothing.
func TestReplySubscriberAuthStops(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, clk)

	srv, reqs := streamServer(t, "", http.StatusForbidden, nil)
	writer := newFakeWriter()
	sub := NewReplySubscriber(ReplyConfig{Server: srv.URL, Topic: "reply", Token: "tok"}, st, writer, clk, discardLogger())

	stopped := make(chan struct{})
	sub.afterStop = func() { close(stopped) }
	sub.Start()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber did not stop on 403 (it is looping on a non-retryable error)")
	}
	if r := atomic.LoadInt32(reqs); r != 1 {
		t.Fatalf("server saw %d connections after a 403, want 1 (no reconnect)", r)
	}
	sub.Close()
}

// TestReplyAllow checks the received-message rate limit in isolation: the
// first replyRateLimit messages in a window are admitted, the next is denied,
// and the window slides once time advances past it.
func TestReplyAllow(t *testing.T) {
	clk := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	s := &ReplySubscriber{clk: clk}
	for i := 0; i < replyRateLimit; i++ {
		if !s.allow() {
			t.Fatalf("message %d denied within limit", i)
		}
	}
	if s.allow() {
		t.Fatal("message over the limit was admitted")
	}
	clk.Advance(replyRateWindow + time.Second)
	if !s.allow() {
		t.Fatal("message denied after the window slid")
	}
}

func TestSplitReply(t *testing.T) {
	cases := []struct {
		raw        string
		name, text string
		ok         bool
	}{
		{"api hello world", "api", "hello world", true}, // text may contain spaces
		{"  api later", "api", "later", true},           // leading whitespace trimmed
		{"api\ttabbed", "api", "tabbed", true},          // tab separates name from text
		{"api trailing\n", "api", "trailing", true},     // trailing newline trimmed
		{"api multi\nline", "api", "multi\nline", true}, // internal newline preserved
		{"onlyname", "", "", false},                     // no text
		{"", "", "", false},                             // empty
		{"   ", "", "", false},                          // whitespace only
		{"api ", "", "", false},                         // empty text after name
	}
	for _, tc := range cases {
		name, text, ok := splitReply(tc.raw)
		if ok != tc.ok {
			t.Fatalf("splitReply(%q) ok=%v, want %v", tc.raw, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if name != tc.name || text != tc.text {
			t.Fatalf("splitReply(%q) = (%q,%q), want (%q,%q)", tc.raw, name, text, tc.name, tc.text)
		}
	}
}
