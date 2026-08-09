package api

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// answerE2EFakeClaudeBinOnce/Path cache one build of test/fakeclaude across
// every e2e test in this package, mirroring
// internal/supervisor/screen_bridge_test.go's buildFakeClaude (unexported
// there, so this package needs its own copy rather than reaching across
// package boundaries).
var (
	answerE2EFakeClaudeBinOnce sync.Once
	answerE2EFakeClaudeBinPath string
	answerE2EFakeClaudeBinErr  error
)

func buildFakeClaudeForAnswerE2E(t *testing.T) string {
	t.Helper()
	answerE2EFakeClaudeBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "corral-fakeclaude-answer-e2e-")
		if err != nil {
			answerE2EFakeClaudeBinErr = err
			return
		}
		bin := filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/danielbecerra/corral/test/fakeclaude")
		if out, err := cmd.CombinedOutput(); err != nil {
			answerE2EFakeClaudeBinErr = err
			t.Logf("go build fakeclaude output:\n%s", out)
			return
		}
		answerE2EFakeClaudeBinPath = bin
	})
	if answerE2EFakeClaudeBinErr != nil {
		t.Fatalf("building test/fakeclaude: %v", answerE2EFakeClaudeBinErr)
	}
	return answerE2EFakeClaudeBinPath
}

// TestAnswerEndToEndMetacharactersSurviveVerbatim is the byte-exact security
// proof design doc §7 demands: spawn a real session over a real fakeclaude
// binary under a real PTY, call the real POST
// /v1/sessions/{idOrName}/answer handler with a string full of shell
// metacharacters, and prove — via fakeclaude's own scenario engine capturing
// what actually arrived on its stdin and relaying it into a hook payload —
// that corral's answer path never touches a shell and delivers the bytes
// unmodified end to end (client request -> handleAnswer -> encodeAnswer ->
// Registry.WriteInput -> PTY master -> real child process stdin).
//
// The scenario's fire_hook step does not require a working hook-relay: per
// scenario.go's fireHook, the payload is merged and recorded to
// hooks-1.json regardless of whether the relay command itself succeeds, so
// RelayCommand is set to a harmless "true" stand-in rather than a real
// corral binary — this test is about the PTY write, not the hook-relay
// round trip (which step 4/5's tests already cover).
func TestAnswerEndToEndMetacharactersSurviveVerbatim(t *testing.T) {
	fakeClaudeBin := buildFakeClaudeForAnswerE2E(t)

	// Hermeticity: config.LoadSession consults $HOME/.corral/config.toml.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CORRAL_SESSION_CLAUDE_BIN", fakeClaudeBin)

	fakeState := t.TempDir()
	fakeHome := t.TempDir()
	scenarioDir := t.TempDir()

	const metachars = "`; rm -rf / #$(whoami)`&& echo pwned\""

	// The scenario waits for one line on stdin (the answer's payload,
	// however the PTY's line discipline delivers it — canonical-mode PTYs
	// translate a trailing \r to \n, so the pattern accepts either
	// terminator, exactly like testdata/scenarios/smoke_turn.json), then
	// relays whatever it captured into a hook payload so this test can read
	// it back from outside the child process.
	scenario := `{
		"name": "answer_metachars",
		"steps": [
			{"op": "wait_stdin", "match": "^.+\r?\n$", "capture": "answer", "timeout": "10s"},
			{"op": "fire_hook", "event": "UserPromptSubmit", "payload": {"prompt_id": "p1", "prompt": "{{answer}}"}},
			{"op": "exit", "code": 0}
		]
	}`
	scenarioPath := filepath.Join(scenarioDir, "answer_metachars.json")
	if err := os.WriteFile(scenarioPath, []byte(scenario), 0o644); err != nil {
		t.Fatalf("writing scenario: %v", err)
	}

	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	engine := state.New(st)

	reg := supervisor.New(st, engine, nil, fc, supervisor.Config{
		StateDir: t.TempDir(),
		EnvSnapshot: map[string]string{
			"PATH":                 os.Getenv("PATH"),
			"CORRAL_FAKE_SCENARIO": scenarioPath,
			"CORRAL_FAKE_STATE":    fakeState,
			"CORRAL_FAKE_HOME":     fakeHome,
		},
		// CORRAL_FAKE_* is test-only plumbing with no business in
		// production's fixed allowlist (envWhitelist in spawn.go) — handed
		// to BuildEnv here exactly like
		// internal/supervisor/lifecycle_test.go's newLifecycleTestRegistry
		// does, without touching that allowlist.
		EnvPassthrough: []string{"CORRAL_FAKE_SCENARIO", "CORRAL_FAKE_STATE", "CORRAL_FAKE_HOME"},
		CorralVersion:  "test",
		APIVersion:     1,
		RelayCommand:   "true", // harmless stand-in; see the test's doc comment.
	}, nil)

	deps := SessionsDeps{Store: st, Engine: engine, Registry: reg}
	srv := newSessionsTestServer(t, deps)

	cwd := t.TempDir()
	createBody, err := json.Marshal(createSessionRequest{Cwd: cwd})
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
	if _, err := st.UpdateSession(context.Background(), created.ID, func(s *session.Session) {
		s.AgentState = session.AgentBlocked
		s.BlockedReasonJSON = `{"kind":"permission","hook_seq":77}`
	}); err != nil {
		t.Fatalf("setting blocked answer correlation: %v", err)
	}
	// Best-effort: if an assertion below fails before the scenario's own
	// "exit" step runs, don't leave a live fakeclaude child behind.
	t.Cleanup(func() { _, _ = reg.Kill(context.Background(), created.ID, nil) })

	answerBody, err := json.Marshal(answerRequest{Text: metachars})
	if err != nil {
		t.Fatalf("marshal answer body: %v", err)
	}
	arec := doVersioned(t, srv.Handler(), "POST", "/v1/sessions/"+created.ID+"/answer", answerBody)
	if arec.Code != 200 {
		t.Fatalf("answer status = %d, want 200, body=%s", arec.Code, arec.Body.String())
	}
	var answerAudit map[string]any
	events, err := st.ListEvents(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == session.EventSessionAnswered {
			if err := json.Unmarshal([]byte(ev.DataJSON), &answerAudit); err != nil {
				t.Fatalf("decoding session.answered: %v", err)
			}
		}
	}
	if answerAudit["via"] != "http" || answerAudit["permission_request_seq"] != float64(77) {
		t.Fatalf("session.answered audit = %#v, want via=http permission_request_seq=77", answerAudit)
	}

	// Poll for the scenario's hooks-1.json: the child must fork, reach its
	// scenario dispatch, and relay the captured line, all of which take
	// real (if small) wall-clock time.
	hookPath := filepath.Join(fakeState, created.ID, "hooks-1.json")
	deadline := time.Now().Add(10 * time.Second)
	var recBytes []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(hookPath)
		if err == nil {
			recBytes = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if recBytes == nil {
		t.Fatalf("hooks-1.json never appeared at %s within 10s", hookPath)
	}

	var hookRec struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(recBytes, &hookRec); err != nil {
		t.Fatalf("parsing hooks-1.json: %v, raw=%s", err, recBytes)
	}
	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(hookRec.Payload, &payload); err != nil {
		t.Fatalf("parsing hook payload: %v, raw=%s", err, hookRec.Payload)
	}

	if payload.Prompt != metachars {
		t.Fatalf("captured answer = %q, want %q byte-for-byte (metacharacters must survive verbatim)", payload.Prompt, metachars)
	}

	// The scenario's own "exit" step follows fire_hook, which triggers
	// Registry.reap in the background (cmd.Wait -> store writes: Status,
	// ExitCode, DesiredState, session.exited event). Wait for that to land
	// before the test returns so those writes are never racing st.Close()
	// in t.Cleanup — see internal/supervisor's TestChildExitIsObserved for
	// the same pattern.
	exitDeadline := time.Now().Add(5 * time.Second)
	var finalStatus session.Status
	for time.Now().Before(exitDeadline) {
		got, err := st.GetSession(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("GetSession while polling for exit: %v", err)
		}
		finalStatus = got.Status
		if finalStatus == session.StatusExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalStatus != session.StatusExited {
		t.Fatalf("session status = %q, want %q within 5s after scenario exit", finalStatus, session.StatusExited)
	}
}
