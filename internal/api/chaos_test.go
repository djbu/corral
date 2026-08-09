package api

// Chaos tests: M3's exit criteria (docs/design/m3.md §6, mapped from
// RUNBOOK §5) is 8 scenarios. This file closes the genuine gaps — the
// scenarios that need a real HTTP surface plus the real
// checkpoint.ResumeCheckpointer, not a test double — and cross-references
// where every other scenario is already covered so this list can be read
// as the complete picture without re-deriving it:
//
//  1. idle-reap round-trip                     -> THIS FILE (TestChaos_IdleReapRoundTrip)
//  2. wake round-trip                           -> THIS FILE (TestChaos_WakeRoundTrip)
//  3. attached session immune to idle reap      -> internal/reaper/reaper_test.go
//  4. working/blocked session immune            -> internal/reaper/reaper_test.go
//  5. reaped session not resurrected on restart -> internal/supervisor TestRecover_SkipsSessionsNotDesiredRunning
//  6. running-session restart (M1 behavior)     -> internal/supervisor restart_resume_test.go
//     (TestGracefulRestartResumes) + TestRecover_LiveOrphanIsReapedNeverReadopted
//  7. answer to reaped session -> 409 -> wake -> answer succeeds -> THIS FILE (TestChaos_AnswerReapedThenWake)
//  8. wake name-collision                       -> internal/supervisor wake_test.go (TestWake_NameCollisionErrors)

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/checkpoint"
	"github.com/danielbecerra/corral/internal/claude/sessions"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// chaosCheckpointer adapts *checkpoint.ResumeCheckpointer (whose Checkpoint
// returns checkpoint.Token) to supervisor.Checkpointer (whose Checkpoint
// returns a bare bool for TurnBoundaryVerified). Embedding promotes
// Restore/Resumable verbatim; only Checkpoint is overridden.
type chaosCheckpointer struct {
	*checkpoint.ResumeCheckpointer
}

func (c chaosCheckpointer) Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) (bool, error) {
	tok, err := c.ResumeCheckpointer.Checkpoint(ctx, s, reason)
	return tok.TurnBoundaryVerified, err
}

// chaosScenario is shared by every test below: the child spawns, waits for
// one line on stdin (an answer's payload), relays it into a hook so the
// test can observe it, then exits cleanly. None of the three tests here
// actually need the fire_hook/relay step's content — they only need a
// child that (a) starts, (b) stays alive on stdin until told to answer,
// and (c) exits promptly once it is — but reusing answer_e2e_test.go's
// proven scenario shape rather than inventing a bespoke one keeps this
// file's scenario behavior identical to a test already known to work.
const chaosScenario = `{
	"name": "chaos",
	"steps": [
		{"op": "wait_stdin", "match": "^.+\r?\n$", "capture": "answer", "timeout": "10s"},
		{"op": "fire_hook", "event": "UserPromptSubmit", "payload": {"prompt_id": "p1", "prompt": "{{answer}}"}},
		{"op": "exit", "code": 0}
	]
}`

// chaosHarness bundles everything a chaos test needs: a real HTTP server
// backed by a real store/supervisor.Registry/checkpoint.ResumeCheckpointer,
// and the already-created session these tests drive through idle-reap and
// wake. Built once per test by newChaosHarness so the three tests below
// stay focused on their own assertions rather than repeating setup.
type chaosHarness struct {
	st        *store.Store
	reg       *supervisor.Registry
	srv       *Server
	cp        chaosCheckpointer
	fakeState string
	created   sessionResponse
}

// newChaosHarness builds a chaos test's fixed setup (mirrors
// answer_e2e_test.go's TestAnswerEndToEndMetacharactersSurviveVerbatim,
// with the checkpointer swapped from nil to a real
// chaosCheckpointer{checkpoint.NewResumeCheckpointer(...)}), creates one
// session named name, and pre-writes a real-claude-shaped transcript
// fixture for it so Checkpoint's turn-boundary read and Resumable's
// transcript-exists check both succeed deterministically.
//
// The checkpointer is built with clock.Real() and a short 200ms grace,
// never the fake clock fc the store/supervisor otherwise use: Checkpoint's
// SIGTERM-to-SIGKILL escalation blocks on <-clock.After(grace), which a
// fake clock never fires on its own, so a checkpointer sharing fc would
// deadlock every CheckpointIdle call below.
func newChaosHarness(t *testing.T, name string) *chaosHarness {
	t.Helper()

	fakeClaudeBin := buildFakeClaudeForAnswerE2E(t)

	// Hermeticity: config.LoadSession consults $HOME/.corral/config.toml.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CORRAL_SESSION_CLAUDE_BIN", fakeClaudeBin)

	fakeState := t.TempDir()
	fakeHome := t.TempDir()
	scenarioDir := t.TempDir()
	claudeHome := filepath.Join(fakeHome, ".claude")

	scenarioPath := filepath.Join(scenarioDir, "chaos.json")
	if err := os.WriteFile(scenarioPath, []byte(chaosScenario), 0o644); err != nil {
		t.Fatalf("writing scenario: %v", err)
	}

	envSnapshot := map[string]string{
		"PATH":                 os.Getenv("PATH"),
		"CORRAL_FAKE_SCENARIO": scenarioPath,
		"CORRAL_FAKE_STATE":    fakeState,
		"CORRAL_FAKE_HOME":     fakeHome,
	}
	envPassthrough := []string{"CORRAL_FAKE_SCENARIO", "CORRAL_FAKE_STATE", "CORRAL_FAKE_HOME"}

	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	engine := state.New(st)

	cp := chaosCheckpointer{checkpoint.NewResumeCheckpointer(
		st, clock.Real(), 200*time.Millisecond, claudeHome, envSnapshot, envPassthrough, "", "")}

	reg := supervisor.New(st, engine, cp, fc, supervisor.Config{
		StateDir:       t.TempDir(),
		EnvSnapshot:    envSnapshot,
		EnvPassthrough: envPassthrough,
		CorralVersion:  "test",
		APIVersion:     1,
		RelayCommand:   "true", // harmless stand-in; scenario's fire_hook records regardless.
	}, nil)

	deps := SessionsDeps{Store: st, Engine: engine, Registry: reg}
	srv := newSessionsTestServer(t, deps)

	cwd := t.TempDir()
	createBody, err := json.Marshal(createSessionRequest{Cwd: cwd, Name: name})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	rec := doVersioned(t, srv.Handler(), "POST", "/v1/sessions", createBody)
	if rec.Code != 201 {
		t.Fatalf("create session status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var created sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding create response: %v, body=%s", err, rec.Body.Bytes())
	}
	if created.ID == "" {
		t.Fatalf("created session has empty ID; body=%s", rec.Body.String())
	}
	// Best-effort: if an assertion below fails before CheckpointIdle (or the
	// scenario's own exit step) reaps the child, don't leave it running.
	t.Cleanup(func() { _, _ = reg.Kill(context.Background(), created.ID, nil) })

	// Wait for fakeclaude to actually be running before any test sends it a
	// signal (CheckpointIdle -> SIGTERM). fork/exec + Go runtime bootstrap
	// takes real wall-clock time before main() runs a single line, including
	// installing fakeclaude's own signal.Notify(SIGTERM) handler; a signal
	// sent too soon after Spawn hits the OS's default terminate disposition
	// and kills the child before it even writes its first invocation record.
	// Alt-screen entry is evidence the process is alive and well past
	// program start (same readiness signal lifecycle_test.go's
	// TestGraceEscalation and screen_bridge_test.go poll for).
	waitFakeclaudeReady(t, reg, created.ID)

	// Write the transcript fixture using created.Cwd (the store's
	// persisted value), not the local cwd variable: Checkpoint/Resumable
	// both compute the transcript path from the session row's Cwd, and
	// nothing here guarantees handleCreate stores it byte-identical to
	// what was passed in (e.g. path cleaning).
	writeChaosTranscript(t, claudeHome, created.Cwd, created.ID)

	return &chaosHarness{
		st:        st,
		reg:       reg,
		srv:       srv,
		cp:        cp,
		fakeState: fakeState,
		created:   created,
	}
}

// waitFakeclaudeReady polls the live session's screen state until it has
// entered the alt screen, up to a 5s deadline (5ms poll interval) — proof
// fakeclaude's main() has run well past program start and installed its own
// SIGTERM handler, so a signal sent afterward is not racing process bootstrap.
func waitFakeclaudeReady(t *testing.T, reg *supervisor.Registry, id string) {
	t.Helper()
	ls, ok := reg.Get(id)
	if !ok {
		t.Fatalf("Get(%q) after create = false, want true", id)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !ls.Screen.DebugGrid().AltScreen {
		time.Sleep(5 * time.Millisecond)
	}
	if !ls.Screen.DebugGrid().AltScreen {
		t.Fatalf("fakeclaude (session %s) never reached alt-screen within 5s; can't trust a signal sent this early", id)
	}
}

// writeChaosTranscript pre-writes a real-claude-shaped JSONL transcript
// ending in a completed assistant turn (type=assistant,
// message.stop_reason=end_turn) at the exact path
// checkpoint.ResumeCheckpointer computes for (claudeHome, cwd, sessionID).
// fakeclaude's scenario mode never appends to this file (its JSONL writer
// only runs from the interactive TUI stdin loop, never from scenario
// dispatch — see test/fakeclaude/main.go), so this fixture is the transcript's
// entire, final content: nothing races or appends to it later, which is why
// TestChaos_IdleReapRoundTrip below can assert turn_boundary_verified's
// *value* rather than only its presence.
func writeChaosTranscript(t *testing.T, claudeHome, cwd, sessionID string) {
	t.Helper()
	path := sessions.TranscriptPath(claudeHome, cwd, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	const transcript = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}
`
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("writing transcript fixture at %s: %v", path, err)
	}
}

// waitNotLive polls until the registry no longer tracks id as live.
// CheckpointIdle's Checkpoint call writes terminal state to the store
// synchronously, but the live session is only removed from the registry's
// in-memory map by a background goroutine (cmd.Wait -> deregister; see
// answer_e2e_test.go's TestAnswerEndToEndMetacharactersSurviveVerbatim doc
// comment on the same asynchrony). Every operation below that depends on
// the session actually being gone from the registry — Wake taking its
// resume path rather than its already-live no-op, handleAnswer's
// WriteInput seeing ErrNotLive — must wait for this first, or it flakes.
func waitNotLive(t *testing.T, reg *supervisor.Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get(id); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s still live in registry 10s after CheckpointIdle", id)
}

// TestChaos_IdleReapRoundTrip is m3.md §6.1: idle-reap a live session via
// the real checkpoint.ResumeCheckpointer and confirm it lands
// stopped-but-resumable with a truthful turn-boundary record.
func TestChaos_IdleReapRoundTrip(t *testing.T) {
	h := newChaosHarness(t, "chaos-idle-reap")
	ctx := context.Background()

	// The 30m idleFor is recorded verbatim as idle_ms; it is not a timing
	// dependency (no reaper loop, no fake-clock advance needed) — selection
	// policy for *which* sessions get reaped belongs to
	// internal/reaper/reaper_test.go, not here.
	updated, err := h.reg.CheckpointIdle(ctx, h.created.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("CheckpointIdle: %v", err)
	}
	waitNotLive(t, h.reg, h.created.ID)

	if updated.Status != session.StatusExited {
		t.Fatalf("Status = %q, want %q", updated.Status, session.StatusExited)
	}
	if updated.DesiredState != session.DesiredStopped {
		t.Fatalf("DesiredState = %q, want %q", updated.DesiredState, session.DesiredStopped)
	}

	rec, err := h.st.GetSession(ctx, h.created.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Status != session.StatusExited {
		t.Fatalf("fresh GetSession Status = %q, want %q", rec.Status, session.StatusExited)
	}
	if rec.DesiredState != session.DesiredStopped {
		t.Fatalf("fresh GetSession DesiredState = %q, want %q", rec.DesiredState, session.DesiredStopped)
	}

	events, err := h.st.ListEvents(ctx, h.created.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind != session.EventSessionIdleReaped {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(e.DataJSON), &data); err != nil {
			t.Fatalf("unmarshaling session.idle_reaped data %q: %v", e.DataJSON, err)
		}
		// m3.md §6.1: the key must be present — this assertion line stays
		// correct even if the value semantics change later.
		if _, ok := data["turn_boundary_verified"]; !ok {
			t.Fatalf("session.idle_reaped data = %v, missing turn_boundary_verified key", data)
		}
		// Strictly stronger than §6.1 (which says never assert the value,
		// since flush lag makes it nondeterministic under a real CLI): here
		// the pre-written fixture is the transcript's entire, final
		// content (see writeChaosTranscript's doc comment), so there is no
		// flush lag to race and the value is deterministically true.
		if v, _ := data["turn_boundary_verified"].(bool); v != true {
			t.Fatalf("session.idle_reaped data = %v, want turn_boundary_verified=true", data)
		}
		found = true
	}
	if !found {
		t.Fatalf("no %q event found for session %s", session.EventSessionIdleReaped, h.created.ID)
	}

	resumable, why := h.cp.Resumable(*rec)
	if !resumable {
		t.Fatalf("Resumable = false (%s), want true", why)
	}
	if why != "" {
		t.Fatalf("Resumable why = %q, want empty on success", why)
	}
}

// TestChaos_WakeRoundTrip is m3.md §6.2: from an idle-reaped session, Wake
// must relaunch it with --resume <claude_session_id> and record
// session.resumed(reason=wake).
func TestChaos_WakeRoundTrip(t *testing.T) {
	h := newChaosHarness(t, "chaos-wake")
	ctx := context.Background()

	if _, err := h.reg.CheckpointIdle(ctx, h.created.ID, 30*time.Minute); err != nil {
		t.Fatalf("CheckpointIdle: %v", err)
	}
	waitNotLive(t, h.reg, h.created.ID)

	woken, err := h.reg.Wake(ctx, h.created.ID)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	t.Cleanup(func() { _, _ = h.reg.Kill(context.Background(), h.created.ID, nil) })

	// The resumed process also needs to reach the alt screen before we
	// trust its invocation record is fully written (recordInvocation runs
	// before writeStartup in fakeclaude's main(), so this is generous, but
	// consistent with newChaosHarness's readiness wait).
	waitFakeclaudeReady(t, h.reg, h.created.ID)

	if woken.Status != session.StatusRunning {
		t.Fatalf("Status = %q, want %q", woken.Status, session.StatusRunning)
	}
	if woken.DesiredState != session.DesiredRunning {
		t.Fatalf("DesiredState = %q, want %q", woken.DesiredState, session.DesiredRunning)
	}
	if woken.ResumeCount != 1 {
		t.Fatalf("ResumeCount = %d, want 1", woken.ResumeCount)
	}

	events, err := h.st.ListEvents(ctx, h.created.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind != session.EventSessionResumed {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(e.DataJSON), &data); err != nil {
			t.Fatalf("unmarshaling session.resumed data %q: %v", e.DataJSON, err)
		}
		if data["reason"] != "wake" {
			continue
		}
		found = true
	}
	if !found {
		t.Fatalf("no %q event with reason=wake found for session %s", session.EventSessionResumed, h.created.ID)
	}

	// invocation-1.json was the original fresh spawn; the resumed spawn
	// (this Wake) is fakeclaude's second invocation for this session.
	invPath := filepath.Join(h.fakeState, h.created.ID, "invocation-2.json")
	deadline := time.Now().Add(10 * time.Second)
	var invBytes []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(invPath)
		if err == nil {
			invBytes = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if invBytes == nil {
		entries, _ := os.ReadDir(filepath.Dir(invPath))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("invocation-2.json never appeared at %s within 10s; dir contents=%v", invPath, names)
	}

	var inv struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(invBytes, &inv); err != nil {
		t.Fatalf("parsing invocation-2.json: %v, raw=%s", err, invBytes)
	}

	var hasResumeFlag, hasSessionID bool
	for _, a := range inv.Argv {
		if a == "--resume" {
			hasResumeFlag = true
		}
		if a == h.created.ID {
			hasSessionID = true
		}
	}
	if !hasResumeFlag {
		t.Fatalf("invocation-2.json argv = %v, want it to contain --resume", inv.Argv)
	}
	if !hasSessionID {
		t.Fatalf("invocation-2.json argv = %v, want it to contain claude_session_id %q", inv.Argv, h.created.ID)
	}
}

// TestChaos_AnswerReapedThenWake is m3.md §6.7: an answer posted to a
// reaped session must fail with 409 session_not_live (auto-wake is
// deferred, §4.2), and a subsequent explicit wake must let a following
// answer through.
func TestChaos_AnswerReapedThenWake(t *testing.T) {
	h := newChaosHarness(t, "chaos-answer-reaped")
	ctx := context.Background()

	if _, err := h.reg.CheckpointIdle(ctx, h.created.ID, 30*time.Minute); err != nil {
		t.Fatalf("CheckpointIdle: %v", err)
	}
	waitNotLive(t, h.reg, h.created.ID)

	answerBody, err := json.Marshal(answerRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("marshal answer body: %v", err)
	}

	rec1 := doVersioned(t, h.srv.Handler(), "POST", "/v1/sessions/"+h.created.ID+"/answer", answerBody)
	if rec1.Code != 409 {
		t.Fatalf("answer-while-reaped status = %d, want 409, body=%s", rec1.Code, rec1.Body.String())
	}
	var errEnv errorEnvelope
	if err := json.Unmarshal(rec1.Body.Bytes(), &errEnv); err != nil {
		t.Fatalf("decoding error envelope: %v, body=%s", err, rec1.Body.Bytes())
	}
	if errEnv.Error.Code != CodeSessionNotLive {
		t.Fatalf("error.code = %q, want %q", errEnv.Error.Code, CodeSessionNotLive)
	}

	if _, err := h.reg.Wake(ctx, h.created.ID); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	t.Cleanup(func() { _, _ = h.reg.Kill(context.Background(), h.created.ID, nil) })

	rec2 := doVersioned(t, h.srv.Handler(), "POST", "/v1/sessions/"+h.created.ID+"/answer", answerBody)
	if rec2.Code != 200 {
		t.Fatalf("answer-after-wake status = %d, want 200, body=%s", rec2.Code, rec2.Body.String())
	}
}
