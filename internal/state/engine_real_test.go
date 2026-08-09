package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/hookrelay"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

// --- test infra -------------------------------------------------------

// openRealTestStore opens a Store on a short-lived temp DB via
// os.MkdirTemp (not t.TempDir(), per the anti-stall instructions guarding
// against long temp paths) and registers cleanup. The returned cleanup
// closes the store; callers that also start session loops against this
// store MUST register their own t.Cleanup to stop those loops BEFORE this
// one runs (t.Cleanup is LIFO), or a loop can still be mid-write when the
// store closes underneath it.
func openRealTestStore(t *testing.T, clk *clocktest.FakeClock) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "corral-state-")
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

// waitStep receives one signal from stepCh, failing the test on timeout.
func waitStep(t *testing.T, stepCh <-chan struct{}) {
	t.Helper()
	select {
	case <-stepCh:
	case <-time.After(2 * time.Second):
		t.Fatal("waitStep: timed out waiting for loop iteration")
	}
}

// deliver enqueues ev via OnHookEvent and waits for exactly one loop
// iteration to process it.
func deliver(t *testing.T, e *realEngine, sessionID string, stepCh <-chan struct{}, ev hookrelay.HookPayload) {
	t.Helper()
	if err := e.OnHookEvent(context.Background(), sessionID, ev); err != nil {
		t.Fatalf("OnHookEvent: %v", err)
	}
	waitStep(t, stepCh)
}

// pollAgentState polls the store for sessionID's agent_state to reach
// want, up to a short deadline. Timer-driven assertions poll rather than
// count steps, since a stale FakeClock timer fire can make the exact
// number of loop iterations after Advance nondeterministic.
func pollAgentState(t *testing.T, st *store.Store, sessionID string, want session.AgentState) *session.Session {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last *session.Session
	for time.Now().Before(deadline) {
		sess, err := st.GetSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		last = sess
		if sess.AgentState == want {
			return sess
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("agent_state for %s never reached %q, last seen %q (blocked_reason_json=%q)", sessionID, want, last.AgentState, last.BlockedReasonJSON)
	return nil
}

// spyNotifier records NotifyBlocked calls under a mutex (the loop
// goroutine calls it; assertions read from the test goroutine — this must
// be race-safe).
type spyNotifier struct {
	mu    sync.Mutex
	calls []BlockedReason
}

func (s *spyNotifier) NotifyBlocked(sessionID string, reason BlockedReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, reason)
	return nil
}

func (s *spyNotifier) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *spyNotifier) first() BlockedReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[0]
}

// listEventKinds returns the EventKind of every recorded event for
// sessionID, in append order.
func listEventKinds(t *testing.T, st *store.Store, sessionID string) []session.EventKind {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var out []session.EventKind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func countEventKind(kinds []session.EventKind, want session.EventKind) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
}

func containsEventKind(kinds []session.EventKind, want session.EventKind) bool {
	return countEventKind(kinds, want) > 0
}

// bashPayload builds a minimal Bash-tool hook payload.
func bashPayload(hookEvent, sessionID, promptID, command string) hookrelay.HookPayload {
	return hookrelay.HookPayload{
		Common: hookrelay.Common{
			HookEventName:  hookEvent,
			SessionID:      sessionID,
			PermissionMode: "default",
		},
		PromptID:  promptID,
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"` + command + `"}`),
	}
}

// --- tests --------------------------------------------------------------

var fixedStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// stopEngineLoop registers a t.Cleanup that stops sessionID's loop.
// Registered AFTER the store's own cleanup (openRealTestStore), so it runs
// FIRST (t.Cleanup is LIFO) — the loop is stopped before the store closes
// underneath it.
func stopEngineLoop(t *testing.T, e *realEngine, sessionID string) {
	t.Helper()
	t.Cleanup(func() {
		_ = e.OnLifecycle(context.Background(), sessionID, session.EventSessionExited, nil)
	})
}

// TestAutoModeRegression_NoBlockedNoNotify is the single most important
// test in M2 (Amendment A.3). A permission request that settles within
// the timeout because the tool call it corresponds to has already been
// approved must NEVER be reported as blocked and must NEVER fire a
// notification — a false "blocked" here is the worst failure this product
// can have: an operator paged for an agent that was never stuck.
func TestAutoModeRegression_NoBlockedNoNotify(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	notifier := &spyNotifier{}
	e := NewEngine(st, fc, EngineConfig{}, notifier, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-automode"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"rm -rf ./victim"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "rm -rf ./victim"))

	fc.Advance(6 * time.Second) // well under the 15s settle default

	deliver(t, e, id, stepCh, bashPayload("PostToolUse", id, "p1", "rm -rf ./victim")) // approves it

	fc.Advance(20 * time.Second) // well past the original settle deadline

	// Give any stray timer fire a moment to (harmlessly) run and settle.
	sess := pollAgentState(t, st, id, session.AgentWorking)

	if got := notifier.count(); got != 0 {
		t.Fatalf("notifier.NotifyBlocked called %d times, want 0", got)
	}
	kinds := listEventKinds(t, st, id)
	if containsEventKind(kinds, session.EventPermissionBlocked) {
		t.Fatalf("permission.blocked event recorded, want none; events=%v", kinds)
	}
	if sess.AgentState == session.AgentBlocked {
		t.Fatalf("agent_state = blocked, want not-blocked")
	}
	if sess.AgentState != session.AgentWorking {
		t.Fatalf("agent_state = %q, want %q", sess.AgentState, session.AgentWorking)
	}
	if sess.HookCount != 5 {
		t.Fatalf("HookCount = %d, want 5 (one per delivered event: SessionStart, UserPromptSubmit, PreToolUse, PermissionRequest, PostToolUse)", sess.HookCount)
	}
}

func TestSettleFiresBlocked(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	notifier := &spyNotifier{}
	e := NewEngine(st, fc, EngineConfig{}, notifier, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-blocked"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(16 * time.Second) // past the 15s settle default; never resolved

	sess := pollAgentState(t, st, id, session.AgentBlocked)

	var reason BlockedReason
	if err := json.Unmarshal([]byte(sess.BlockedReasonJSON), &reason); err != nil {
		t.Fatalf("unmarshaling blocked_reason_json %q: %v", sess.BlockedReasonJSON, err)
	}
	if reason.Kind != "permission" {
		t.Errorf("reason.Kind = %q, want %q", reason.Kind, "permission")
	}
	if reason.ToolName != "Bash" {
		t.Errorf("reason.ToolName = %q, want %q", reason.ToolName, "Bash")
	}
	if reason.PromptID != "p1" {
		t.Errorf("reason.PromptID = %q, want %q", reason.PromptID, "p1")
	}
	wantBlockedAt := fixedStart.Add(16 * time.Second).UTC().Format(time.RFC3339)
	if reason.BlockedAt != wantBlockedAt {
		t.Errorf("reason.BlockedAt = %q, want %q", reason.BlockedAt, wantBlockedAt)
	}
	if reason.HookSeq == 0 {
		t.Errorf("reason.HookSeq = 0, want the permission.requested event's assigned seq")
	}

	if got := notifier.count(); got != 1 {
		t.Fatalf("notifier.NotifyBlocked called %d times, want exactly 1", got)
	}

	kinds := listEventKinds(t, st, id)
	if !containsEventKind(kinds, session.EventPermissionBlocked) {
		t.Fatalf("no permission.blocked event recorded; events=%v", kinds)
	}

	// Regression: blocked_at must be frozen at the moment the request first
	// crossed its settle deadline, not rewritten on every subsequent
	// recompute. Advance the clock further and deliver one more (unrelated)
	// event, which still runs recomputeAndPersist for this still-blocked
	// request.
	fc.Advance(5 * time.Minute)
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "Notification", SessionID: id}, PromptID: "p-other", NotificationType: "permission_prompt"})

	sess = pollAgentState(t, st, id, session.AgentBlocked)
	var reason2 BlockedReason
	if err := json.Unmarshal([]byte(sess.BlockedReasonJSON), &reason2); err != nil {
		t.Fatalf("unmarshaling blocked_reason_json %q: %v", sess.BlockedReasonJSON, err)
	}
	if reason2.BlockedAt != wantBlockedAt {
		t.Errorf("after further activity, reason.BlockedAt = %q, want unchanged %q", reason2.BlockedAt, wantBlockedAt)
	}
	if reason2.HookSeq != reason.HookSeq {
		t.Errorf("reason.HookSeq changed from %d to %d across recomputes, want stable", reason.HookSeq, reason2.HookSeq)
	}
}

func TestPermissionEvidenceIsRedactedCorrelatedAndCapped(t *testing.T) {
	t.Run("redaction and request correlation", func(t *testing.T) {
		fc := clocktest.NewFake(fixedStart)
		st := openRealTestStore(t, fc)
		e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)
		stepCh := make(chan struct{}, 64)
		e.afterStep = func() { stepCh <- struct{}{} }

		const id = "sess-evidence-redacted"
		const secret = "sk-ant-abcdefghijklmnopqrstuvwxyz123456"
		createTestSession(t, st, id, session.StatusRunning)
		stopEngineLoop(t, e, id)

		deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"deploy ` + secret + `"}`)})
		deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "deploy "+secret))

		var requested *session.Event
		for _, ev := range mustListEvents(t, st, id) {
			if ev.Kind == session.EventPermissionRequested {
				requested = ev
				break
			}
		}
		if requested == nil {
			t.Fatal("permission.requested event missing")
		}
		if strings.Contains(requested.DataJSON, secret) {
			t.Fatalf("permission.requested leaked secret: %s", requested.DataJSON)
		}
		if !strings.Contains(requested.DataJSON, "redacted:anthropic_key") {
			t.Fatalf("permission.requested missing redaction marker: %s", requested.DataJSON)
		}

		fc.Advance(16 * time.Second)
		sess := pollAgentState(t, st, id, session.AgentBlocked)
		if strings.Contains(sess.BlockedReasonJSON, secret) {
			t.Fatalf("blocked reason leaked secret: %s", sess.BlockedReasonJSON)
		}
		var reason BlockedReason
		if err := json.Unmarshal([]byte(sess.BlockedReasonJSON), &reason); err != nil {
			t.Fatalf("decode blocked reason: %v", err)
		}
		if reason.HookSeq != requested.Seq {
			t.Fatalf("blocked reason hook_seq = %d, want request seq %d", reason.HookSeq, requested.Seq)
		}
		if !slices.Contains(reason.Redactions, "anthropic_key") {
			t.Fatalf("blocked reason redactions = %v, want anthropic_key", reason.Redactions)
		}

		resolvedPayload := bashPayload("PostToolUse", id, "p1", "deploy "+secret)
		resolvedPayload.ToolUseID = "tu1"
		deliver(t, e, id, stepCh, resolvedPayload)
		var resolved map[string]any
		var resolvedRaw string
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && resolvedRaw == "" {
			for _, ev := range mustListEvents(t, st, id) {
				if ev.Kind == session.EventPermissionResolved {
					resolvedRaw = ev.DataJSON
					if err := json.Unmarshal([]byte(ev.DataJSON), &resolved); err != nil {
						t.Fatalf("decode permission.resolved: %v", err)
					}
				}
			}
			if resolvedRaw == "" {
				time.Sleep(2 * time.Millisecond)
			}
		}
		seqValue, ok := resolved["request_seq"].(float64)
		if !ok {
			t.Fatalf("permission.resolved missing numeric request_seq: raw=%s resolved=%v events=%v", resolvedRaw, resolved, listEventKinds(t, st, id))
		}
		if got := int64(seqValue); got != requested.Seq {
			t.Fatalf("permission.resolved request_seq = %d, want %d", got, requested.Seq)
		}
	})

	t.Run("cap keeps enclosing event valid", func(t *testing.T) {
		fc := clocktest.NewFake(fixedStart)
		st := openRealTestStore(t, fc)
		e := NewEngine(st, fc, EngineConfig{MaxEventPayloadBytes: 24}, nil, nil)
		stepCh := make(chan struct{}, 64)
		e.afterStep = func() { stepCh <- struct{}{} }

		const id = "sess-evidence-capped"
		createTestSession(t, st, id, session.StatusRunning)
		stopEngineLoop(t, e, id)
		deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", strings.Repeat("x", 100)))

		for _, ev := range mustListEvents(t, st, id) {
			if ev.Kind != session.EventPermissionRequested {
				continue
			}
			var data struct {
				ToolInput any  `json:"tool_input"`
				Truncated bool `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(ev.DataJSON), &data); err != nil {
				t.Fatalf("permission.requested is invalid JSON: %v, data=%s", err, ev.DataJSON)
			}
			if !data.Truncated {
				t.Fatalf("truncated = false, want true: %s", ev.DataJSON)
			}
			if _, ok := data.ToolInput.(string); !ok {
				t.Fatalf("capped tool_input type = %T, want JSON string", data.ToolInput)
			}
			return
		}
		t.Fatal("permission.requested event missing")
	})
}

func TestResolveAfterBlocked_BackToWorking(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-resolve-after-blocked"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(16 * time.Second)
	pollAgentState(t, st, id, session.AgentBlocked)

	deliver(t, e, id, stepCh, bashPayload("PostToolUse", id, "p1", "echo hi"))

	sess := pollAgentState(t, st, id, session.AgentWorking)
	if sess.BlockedReasonJSON != "" {
		t.Fatalf("blocked_reason_json = %q, want empty after resolution", sess.BlockedReasonJSON)
	}
}

func TestTTLAbandonment(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{
		PermissionSettle: 15 * time.Second,
		PermissionTTL:    30 * time.Second,
	}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-ttl"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(16 * time.Second) // past the 15s settle deadline (measured from open)
	pollAgentState(t, st, id, session.AgentBlocked)

	fc.Advance(15 * time.Second) // total 31s from open, past the 30s TTL deadline (also measured from open, not stacked on settle)

	// lastActivity was set to AgentWorking by PreToolUse and never
	// changed again (PermissionRequest carries TargetNone); once the
	// request is abandoned and no requests remain open, agent_state
	// falls back to that lastActivity level.
	pollAgentState(t, st, id, session.AgentWorking)

	found := false
	for _, ev := range mustListEvents(t, st, id) {
		if ev.Kind == session.EventPermissionResolved && strings.Contains(ev.DataJSON, `"outcome":"abandoned"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no permission.resolved outcome=abandoned event recorded; events=%v", listEventKinds(t, st, id))
	}
}

func mustListEvents(t *testing.T, st *store.Store, sessionID string) []*session.Event {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return evs
}

func TestMultipleOpen_OldestWins_BlockedCount(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-multi-open"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"cmd-a"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "cmd-a")) // opened at t=0

	fc.Advance(3 * time.Second)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu2", ToolInput: json.RawMessage(`{"command":"cmd-b"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "cmd-b")) // opened at t=3s

	fc.Advance(16 * time.Second) // both past 15s settle (t=16s and t=19s respectively from their own opens... first opened at 0, settle at 15; second opened at 3, settle at 18)

	sess := pollAgentState(t, st, id, session.AgentBlocked)

	var reason BlockedReason
	if err := json.Unmarshal([]byte(sess.BlockedReasonJSON), &reason); err != nil {
		t.Fatalf("unmarshaling blocked_reason_json: %v", err)
	}
	// The oldest request's tool_input should be reflected in blocked_reason_json.
	if !strings.Contains(string(reason.ToolInput), "cmd-a") {
		t.Fatalf("blocked_reason_json does not reflect oldest request (cmd-a); got %+v", reason)
	}

	// blocked_count itself is NOT a DB column in migration 0002 (deferred
	// to 6c's API surfacing); assert instead that BOTH requests produced a
	// permission.blocked event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countEventKind(listEventKinds(t, st, id), session.EventPermissionBlocked) >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := countEventKind(listEventKinds(t, st, id), session.EventPermissionBlocked); got != 2 {
		t.Fatalf("permission.blocked event count = %d, want 2", got)
	}
}

func TestEnqueueFull_ReturnsErrQueueFull(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	// gate stalls the loop at the END of each iteration (after
	// recomputeAndPersist has committed and the store connection is
	// released — store.Open sets SetMaxOpenConns(1), so gating mid-write
	// would wedge every other store caller too, including polling
	// helpers elsewhere in the package). Closing gate lets every blocked
	// and future afterStep call return immediately, so the loop drains
	// normally once released.
	gate := make(chan struct{})
	var processed int64
	e.afterStep = func() {
		<-gate
		atomic.AddInt64(&processed, 1)
	}

	const id = "sess-queue-full"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	// The first event is consumed by the loop and then stalls in
	// afterStep (holding the gate), so nothing drains the ingest channel
	// for the rest of this test.
	if err := e.OnHookEvent(context.Background(), id, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}}); err != nil {
		t.Fatalf("first OnHookEvent: %v", err)
	}

	var sawFull bool
	for i := 0; i < ingestBufferSize+50; i++ {
		err := e.OnHookEvent(context.Background(), id, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreCompact", SessionID: id}})
		if err == ErrQueueFull {
			sawFull = true
			break
		}
	}
	if !sawFull {
		t.Fatal("expected at least one OnHookEvent to return ErrQueueFull")
	}

	// Release the gate and let the loop drain its backlog before the
	// store closes underneath it (t.Cleanup order): wait until the
	// processed count quiesces rather than sleeping a fixed guess.
	close(gate)
	var last int64
	quietSince := time.Now()
	for time.Since(quietSince) < 200*time.Millisecond {
		cur := atomic.LoadInt64(&processed)
		if cur != last {
			last = cur
			quietSince = time.Now()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUnknownEvent_HeartbeatNoTransition(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-unknown-event"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})

	before, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "FutureEventV2", SessionID: id}})

	after, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if after.AgentState != before.AgentState {
		t.Fatalf("agent_state changed on unknown event: %q -> %q", before.AgentState, after.AgentState)
	}
	if after.HookCount != before.HookCount+1 {
		t.Fatalf("HookCount = %d, want %d", after.HookCount, before.HookCount+1)
	}

	kinds := listEventKinds(t, st, id)
	if !containsEventKind(kinds, session.EventHookUnknownEvent) {
		t.Fatalf("no hook.unknown_event recorded; events=%v", kinds)
	}
}

func TestSubagentDoesNotPolluteParentTurn(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-subagent"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "parent-tu1", ToolInput: json.RawMessage(`{"command":"parent-cmd"}`)})

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SubagentStart", SessionID: id}, AgentID: "sub1", AgentType: "explore"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Grep", ToolUseID: "sub-tu1", AgentID: "sub1", ToolInput: json.RawMessage(`{"pattern":"x"}`)})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Grep", ToolUseID: "sub-tu2", AgentID: "sub1", ToolInput: json.RawMessage(`{"pattern":"y"}`)})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SubagentStop", SessionID: id}, AgentID: "sub1", AgentType: "explore"})

	// Parent's own tool call still resolves cleanly despite the subagent's
	// tool opens in between (namespaced by agent_id in pending.go).
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PostToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "parent-tu1", ToolInput: json.RawMessage(`{"command":"parent-cmd"}`)})

	kinds := listEventKinds(t, st, id)
	if !containsEventKind(kinds, session.EventSubagentStarted) {
		t.Fatalf("no subagent.started recorded; events=%v", kinds)
	}
	if !containsEventKind(kinds, session.EventSubagentStopped) {
		t.Fatalf("no subagent.stopped recorded; events=%v", kinds)
	}

	// Limitation (noted in report): the tracker's internal per-agent
	// namespacing isn't directly observable from outside package state in
	// this test; this asserts only the behavioral surface (events
	// recorded, no panic, loop still healthy enough to keep processing).
	sess, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.AgentState != session.AgentWorking {
		t.Fatalf("agent_state = %q, want %q", sess.AgentState, session.AgentWorking)
	}
}

// TestResolveByToolUseID_CosmeticInputMismatch is the discriminator the
// keystone (TestAutoModeRegression) cannot be: it proves resolution
// survives a PostToolUse whose tool_input differs BYTE-FOR-BYTE from the
// PermissionRequest's. Real Claude Code may re-serialize tool_input
// (key order, added fields, whitespace) between the two events; if the
// join relied solely on bytes.Equal(tool_input) the request would fail to
// resolve, settle at 15s, and fire a FALSE blocked — the exact Auto-Mode
// failure A.3 exists to prevent. matchOpen prefers tool_use_id when both
// sides carry one (the PreToolUse supplies it to the open request via the
// tracker; PostToolUse carries it directly), so the cosmetic drift is
// irrelevant. A triple-only join would FAIL this test.
func TestResolveByToolUseID_CosmeticInputMismatch(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	notifier := &spyNotifier{}
	e := NewEngine(st, fc, EngineConfig{}, notifier, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-tooluseid-join"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	// PreToolUse's tool_input is byte-identical to the PermissionRequest's,
	// so the tracker matches the triple and supplies tool_use_id "tu1" to
	// the open request.
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(6 * time.Second) // under the 15s settle

	// PostToolUse carries the same tool_use_id but a cosmetically DIFFERENT
	// tool_input (extra field, reordered) — bytes.Equal would be false here.
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PostToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"note":"reordered","command":"echo hi"}`)})

	sess := pollAgentState(t, st, id, session.AgentWorking)
	if sess.BlockedReasonJSON != "" {
		t.Fatalf("blocked_reason_json = %q, want empty (request resolved via tool_use_id)", sess.BlockedReasonJSON)
	}

	fc.Advance(20 * time.Second) // well past the original settle deadline

	// Nothing should have blocked: give any stray timer fire a moment.
	sess = pollAgentState(t, st, id, session.AgentWorking)
	if got := notifier.count(); got != 0 {
		t.Fatalf("notifier.NotifyBlocked called %d times, want 0 (cosmetic mismatch must still resolve)", got)
	}
	kinds := listEventKinds(t, st, id)
	if containsEventKind(kinds, session.EventPermissionBlocked) {
		t.Fatalf("permission.blocked recorded despite tool_use_id match; events=%v", kinds)
	}
	found := false
	for _, ev := range mustListEvents(t, st, id) {
		if ev.Kind == session.EventPermissionResolved && strings.Contains(ev.DataJSON, `"outcome":"approved"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no permission.resolved outcome=approved recorded; events=%v", kinds)
	}
}

// TestStopResolvesOpenPermission covers the PermResolveTurnEnded path,
// which had zero tests. Crucially it exercises the backstop: a Stop with
// NO prompt_id. If real Claude Code omits prompt_id on Stop (unverified
// until step 7/10), resolveAllForPrompt("") would match nothing and the
// turn-end safety net would be dead code — so an empty prompt_id falls
// back to resolving every open request. The open request must be resolved
// (turn_ended) and must NOT settle to blocked after the settle window.
func TestStopResolvesOpenPermission(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	notifier := &spyNotifier{}
	e := NewEngine(st, fc, EngineConfig{}, notifier, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-stop-resolves"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(6 * time.Second) // still open, under settle

	// Stop with NO prompt_id — exercises the resolveAll backstop.
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "Stop", SessionID: id}})

	sess := pollAgentState(t, st, id, session.AgentIdle)
	if sess.BlockedReasonJSON != "" {
		t.Fatalf("blocked_reason_json = %q, want empty after turn end", sess.BlockedReasonJSON)
	}

	fc.Advance(20 * time.Second) // past the original settle deadline

	sess = pollAgentState(t, st, id, session.AgentIdle)
	if got := notifier.count(); got != 0 {
		t.Fatalf("notifier.NotifyBlocked called %d times, want 0 (turn ended before settle)", got)
	}
	kinds := listEventKinds(t, st, id)
	if containsEventKind(kinds, session.EventPermissionBlocked) {
		t.Fatalf("permission.blocked recorded after Stop; events=%v", kinds)
	}
	found := false
	for _, ev := range mustListEvents(t, st, id) {
		if ev.Kind == session.EventPermissionResolved && strings.Contains(ev.DataJSON, `"outcome":"turn_ended"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no permission.resolved outcome=turn_ended recorded; events=%v", kinds)
	}
}

// TestUserPromptSubmitSupersedesOpenPermission covers the
// PermResolveSuperseded path (also previously untested): a new user turn
// opening while a prior request is still unresolved must resolve it
// (superseded), not leave it to settle into a false blocked.
func TestUserPromptSubmitSupersedesOpenPermission(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	notifier := &spyNotifier{}
	e := NewEngine(st, fc, EngineConfig{}, notifier, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-superseded"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(6 * time.Second) // still open, under settle

	// New turn supersedes the prior one.
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p2"})

	sess := pollAgentState(t, st, id, session.AgentWorking)
	if sess.BlockedReasonJSON != "" {
		t.Fatalf("blocked_reason_json = %q, want empty after supersede", sess.BlockedReasonJSON)
	}

	fc.Advance(20 * time.Second) // past the original settle deadline

	sess = pollAgentState(t, st, id, session.AgentWorking)
	if got := notifier.count(); got != 0 {
		t.Fatalf("notifier.NotifyBlocked called %d times, want 0 (superseded before settle)", got)
	}
	kinds := listEventKinds(t, st, id)
	if containsEventKind(kinds, session.EventPermissionBlocked) {
		t.Fatalf("permission.blocked recorded after supersede; events=%v", kinds)
	}
	found := false
	for _, ev := range mustListEvents(t, st, id) {
		if ev.Kind == session.EventPermissionResolved && strings.Contains(ev.DataJSON, `"outcome":"superseded"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no permission.resolved outcome=superseded recorded; events=%v", kinds)
	}
}

// TestState_NeverHooked_RendersUnknown covers §4.5's timer-free
// never-hooked derivation: a live session that has produced zero hooks
// past FirstHookGrace renders unknown (the "hooks not firing?" failure),
// derived at read time from persisted timestamps against the injected
// clock — no goroutine, no timer.
func TestState_NeverHooked_RendersUnknown(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, nil, nil) // FirstHookGrace defaults to 60s

	const id = "sess-never-hooked"
	createTestSession(t, st, id, session.StatusRunning)
	startMs := fixedStart.UnixMilli()
	if _, err := st.UpdateSession(context.Background(), id, func(s *session.Session) {
		s.StartedAtMs = &startMs
	}); err != nil {
		t.Fatalf("UpdateSession(started): %v", err)
	}

	// Within the grace window: not yet unknown — renders its persisted state.
	if got := e.State(id); got != session.AgentStarting {
		t.Fatalf("within grace: State = %q, want %q", got, session.AgentStarting)
	}

	fc.Advance(61 * time.Second) // past the 60s grace, still zero hooks
	if got := e.State(id); got != session.AgentUnknown {
		t.Fatalf("after grace, no hooks: State = %q, want %q", got, session.AgentUnknown)
	}

	// Any hook history at all disqualifies unknown: the derivation keys on
	// HookCount==0, so a single hook flips it back to the persisted state.
	if _, err := st.UpdateSession(context.Background(), id, func(s *session.Session) {
		s.HookCount = 1
	}); err != nil {
		t.Fatalf("UpdateSession(hookcount): %v", err)
	}
	if got := e.State(id); got == session.AgentUnknown {
		t.Fatalf("with hook history: State = %q, want non-unknown", got)
	}
}

// TestStale_WorkingPastStaleAfter covers §4.5 render-time staleness: a
// working session whose last hook is older than StaleAfter reports Stale
// (rendered "working?"), while blocked/idle/terminal never go stale.
func TestStale_WorkingPastStaleAfter(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{StaleAfter: 15 * time.Minute}, nil, nil)

	const id = "sess-stale"
	createTestSession(t, st, id, session.StatusRunning)
	hookMs := fixedStart.UnixMilli()
	if _, err := st.UpdateSession(context.Background(), id, func(s *session.Session) {
		s.AgentState = session.AgentWorking
		s.HookCount = 3
		s.LastHookAtMs = &hookMs
	}); err != nil {
		t.Fatalf("UpdateSession(working): %v", err)
	}

	// Fresh hook: working, not stale.
	if e.Stale(id) {
		t.Fatal("fresh working session reported stale")
	}

	fc.Advance(16 * time.Minute) // past the 15m StaleAfter
	if !e.Stale(id) {
		t.Fatal("working session past stale_after not reported stale")
	}

	// Blocked never goes stale even with an old hook: a human is the
	// bottleneck, not a wedged agent.
	if _, err := st.UpdateSession(context.Background(), id, func(s *session.Session) {
		s.AgentState = session.AgentBlocked
	}); err != nil {
		t.Fatalf("UpdateSession(blocked): %v", err)
	}
	if e.Stale(id) {
		t.Fatal("blocked session reported stale")
	}
}

// TestPermissionModeRecordedFromPayload covers A.8's "last observed, from
// hook payloads": the mode riding on a hook payload is persisted onto the
// session row so `corral ls --json` can surface it.
func TestPermissionModeRecordedFromPayload(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-permmode"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{
		Common:   hookrelay.Common{HookEventName: "PreToolUse", SessionID: id, PermissionMode: "acceptEdits"},
		PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`),
	})

	sess, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.PermissionMode != "acceptEdits" {
		t.Fatalf("PermissionMode = %q, want %q", sess.PermissionMode, "acceptEdits")
	}
}

// TestAnswerDoesNotForceTransition proves design doc §7's central invariant:
// `corral answer` never changes agent_state itself. It only writes bytes to
// the PTY (simulated here by the store side effect corral answer's handler
// performs: appending a session.answered event) — a session leaves
// AgentBlocked only when a later hook proves it, not because an answer was
// recorded.
func TestAnswerDoesNotForceTransition(t *testing.T) {
	fc := clocktest.NewFake(fixedStart)
	st := openRealTestStore(t, fc)
	e := NewEngine(st, fc, EngineConfig{}, &spyNotifier{}, nil)

	stepCh := make(chan struct{}, 64)
	e.afterStep = func() { stepCh <- struct{}{} }

	const id = "sess-answer-no-transition"
	createTestSession(t, st, id, session.StatusRunning)
	stopEngineLoop(t, e, id)

	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SessionStart", SessionID: id}})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "UserPromptSubmit", SessionID: id}, PromptID: "p1"})
	deliver(t, e, id, stepCh, hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "PreToolUse", SessionID: id}, PromptID: "p1", ToolName: "Bash", ToolUseID: "tu1", ToolInput: json.RawMessage(`{"command":"echo hi"}`)})
	deliver(t, e, id, stepCh, bashPayload("PermissionRequest", id, "p1", "echo hi"))

	fc.Advance(16 * time.Second) // past the 15s settle default; never resolved
	pollAgentState(t, st, id, session.AgentBlocked)

	// Simulate corral answer's own store side effect directly (bypassing the
	// PTY write, which this package has no business exercising): appending
	// the event is the ONLY thing the answer path does to durable state.
	if _, err := st.AppendEvent(context.Background(), id, session.EventSessionAnswered, `{"len":1}`); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// The engine never observed a hook here, so agent_state must still be
	// AgentBlocked — an answer alone provably cannot force a transition.
	sess := pollAgentState(t, st, id, session.AgentBlocked)
	if sess.BlockedReasonJSON == "" {
		t.Fatalf("blocked_reason_json is empty after answer; want the permission block still recorded")
	}
	kinds := listEventKinds(t, st, id)
	if !containsEventKind(kinds, session.EventSessionAnswered) {
		t.Fatalf("no session.answered event recorded; events=%v", kinds)
	}

	// Only a real hook proving the tool resolved moves the session out of
	// blocked.
	deliver(t, e, id, stepCh, bashPayload("PostToolUse", id, "p1", "echo hi"))
	sess = pollAgentState(t, st, id, session.AgentWorking)
	if sess.BlockedReasonJSON != "" {
		t.Fatalf("blocked_reason_json = %q, want empty after resolution", sess.BlockedReasonJSON)
	}
}
