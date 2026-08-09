package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/claude/streamjson"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// newHeadlessTestRegistry is newLifecycleTestRegistry's headless twin: same
// real-sqlite-store + real-fakeclaude wiring, but spec.Mode ==
// ModeHeadless with a Prompt set, and extraEnv always carries
// CORRAL_FAKE_HEADLESS_SCENARIO so fakeclaude's headless.go engine (not
// its M1/M2 interactive one) drives the child.
func newHeadlessTestRegistry(t *testing.T, scenario string) (*Registry, *store.Store, session.Spec, string) {
	t.Helper()
	claudeBin := buildFakeClaude(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cwd := t.TempDir()
	extraEnv := map[string]string{"CORRAL_FAKE_HEADLESS_SCENARIO": scenario}
	snapshot := map[string]string{"PATH": os.Getenv("PATH")}
	passthrough := make([]string, 0, len(extraEnv))
	for k, v := range extraEnv {
		snapshot[k] = v
		passthrough = append(passthrough, k)
	}

	spec := session.Spec{
		ID:             newTestUUID(),
		Name:           "headless-" + newTestUUID(),
		Mode:           session.ModeHeadless,
		Cwd:            cwd,
		ClaudeBin:      claudeBin,
		SettingSources: "user,project,local",
		Prompt:         "do the thing",
		// PermissionMode set here (not just in the BuildArgv-only tests in
		// spawn_test.go) so at least one end-to-end spawn exercises the
		// full --permission-mode delivery chain: corral's argv -> real
		// exec.Cmd -> fakeclaude's parseArgs -> its system:init echo. This
		// is the flag design doc §5.3 calls out as the bypassPermissions
		// security boundary, so BuildArgv-level coverage alone is not
		// enough.
		PermissionMode: "acceptEdits",
	}
	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             spec.ID,
		Name:           spec.Name,
		Mode:           spec.Mode,
		Cwd:            spec.Cwd,
		ClaudeBin:      spec.ClaudeBin,
		SettingSources: spec.SettingSources,
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusStarting,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stateDir := t.TempDir()
	r := New(st, state.New(st), killingCheckpointer{}, clock.Real(), Config{
		StateDir:          stateDir,
		EnvSnapshot:       snapshot,
		EnvPassthrough:    passthrough,
		CorralVersion:     "test",
		APIVersion:        1,
		OutputLogMaxBytes: 1 << 20,
	}, nil)
	return r, st, spec, stateDir
}

func waitForStatus(t *testing.T, st *store.Store, id string, want session.Status) *session.Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got *session.Session
	for time.Now().Before(deadline) {
		var err error
		got, err = st.GetSession(context.Background(), id)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.Status == want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Status = %q after 5s, want %q", got.Status, want)
	return nil
}

// TestHeadlessSpawnSuccessProducesResultEventAndTerminalState drives a
// real headless spawn (Registry.Spawn -> spawnHeadless -> fakeclaude's
// headless.go "success" scenario) end to end: fakeclaude emits
// system:init, one assistant turn, and a terminal result line with
// is_error=false, then exits 0 on its own (design doc §5.1's one-shot
// lifecycle). Asserts the durable session.result event (the primary,
// survives-deregistration record per §8.1) and, secondarily, the
// in-memory LiveSession.Result the reader stashed.
func TestHeadlessSpawnSuccessProducesResultEventAndTerminalState(t *testing.T) {
	r, st, spec, stateDir := newHeadlessTestRegistry(t, "success")

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Capture the LiveSession pointer immediately: deregister only removes
	// it from the registry's map once the child exits, the struct itself
	// stays valid and is safe to read (under r.mu for the Result field).
	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatal("Get(id) after Spawn = false, want true")
	}
	if !ls.Headless {
		t.Fatal("LiveSession.Headless = false, want true for a Mode==ModeHeadless spawn")
	}
	if ls.PTYMaster != nil {
		t.Fatal("LiveSession.PTYMaster != nil, want nil for a headless session")
	}
	if ls.Screen != nil {
		t.Fatal("LiveSession.Screen != nil, want nil for a headless session")
	}

	got := waitForStatus(t, st, spec.ID, session.StatusExited)
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0 (fakeclaude exits 0 after its result line)", got.ExitCode)
	}
	if got.DesiredState != session.DesiredStopped {
		t.Fatalf("DesiredState = %q, want %q (a one-shot -p exit is a self-exit, never resumed)", got.DesiredState, session.DesiredStopped)
	}

	events, err := st.ListEvents(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionResult) {
		t.Fatalf("events = %v, want a %q event", eventKinds(events), session.EventSessionResult)
	}
	resultData := eventDataFor(t, events, session.EventSessionResult)
	// Presence checks, not just value checks: resultData["is_error"].(bool)
	// silently returns false, false-ok both when the field is really false
	// and when appendEvent dropped the payload (e.g. a marshal failure
	// left DataJSON as "{}"). A bare value comparison would pass either
	// way, masking a payload-emission regression. §5.2/§8.1 name all four
	// fields on this event, so all four get a presence check here.
	for _, field := range []string{"is_error", "total_cost_usd", "stop_reason", "num_turns"} {
		if _, ok := resultData[field]; !ok {
			t.Fatalf("session.result event missing %q field (data = %v)", field, resultData)
		}
	}
	if isErr, _ := resultData["is_error"].(bool); isErr {
		t.Fatalf("session.result event is_error = %v, want false", resultData["is_error"])
	}
	if resultData["stop_reason"] != "end_turn" {
		t.Fatalf("session.result event stop_reason = %v, want end_turn", resultData["stop_reason"])
	}
	// num_turns round-trips through JSON -> map[string]any as float64, not
	// int; comparing against the literal 1 would never match.
	if numTurns, _ := resultData["num_turns"].(float64); numTurns != float64(1) {
		t.Fatalf("session.result event num_turns = %v, want 1", resultData["num_turns"])
	}

	r.mu.Lock()
	result := ls.Result
	r.mu.Unlock()
	if result == nil {
		t.Fatal("LiveSession.Result = nil, want the stashed terminal Result")
	}
	if result.IsError {
		t.Fatal("LiveSession.Result.IsError = true, want false")
	}
	if !HeadlessOutcome(result) {
		t.Fatal("HeadlessOutcome(result) = false, want true for a successful result")
	}

	// End-to-end check of the --permission-mode delivery chain (design
	// doc §5.3): corral's argv -> exec.Cmd -> fakeclaude's parseArgs ->
	// its system:init echo, teed into stream.jsonl by spawnHeadless. This
	// is the flag that makes bypassPermissions reachable, so BuildArgv
	// unit coverage alone (spawn_test.go) is not enough — this asserts
	// the value actually reached the child process and came back out.
	streamPath := filepath.Join(stateDir, "sessions", spec.ID, "stream.jsonl")
	streamBytes, err := os.ReadFile(streamPath)
	if err != nil {
		t.Fatalf("reading %s: %v", streamPath, err)
	}
	if !strings.Contains(string(streamBytes), `"permissionMode":"acceptEdits"`) {
		t.Fatalf("stream.jsonl does not contain permissionMode=acceptEdits echoed by fakeclaude; got:\n%s", streamBytes)
	}
}

// TestHeadlessSpawnErrorTurnMapsToFailureOutcome exercises fakeclaude's
// synthesized "error-turn" scenario — a terminal result line with
// is_error=true, which (per streamjson's own doc comment) the real golden
// corpus never contains, so this is the only way to cover §8.1's
// result+error -> failure mapping without a real Anthropic account.
func TestHeadlessSpawnErrorTurnMapsToFailureOutcome(t *testing.T) {
	r, st, spec, _ := newHeadlessTestRegistry(t, "error-turn")

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Capture the LiveSession before the child exits, same as the success
	// test: ls.Result is the real pointer the reader stashed off the
	// parsed stream, not a value reconstructed from the durable event's
	// JSON. Reconstructing from resultData would only re-test
	// HeadlessOutcome against a fresh value (already covered by
	// TestHeadlessOutcome below) and never actually exercise whether
	// readHeadlessStdout correctly stashed the error-turn Result.
	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatal("Get(id) after Spawn = false, want true")
	}

	waitForStatus(t, st, spec.ID, session.StatusExited)

	events, err := st.ListEvents(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	resultData := eventDataFor(t, events, session.EventSessionResult)
	isErr, _ := resultData["is_error"].(bool)
	if !isErr {
		t.Fatalf("session.result event is_error = %v, want true", resultData["is_error"])
	}

	r.mu.Lock()
	result := ls.Result
	r.mu.Unlock()
	if result == nil {
		t.Fatal("LiveSession.Result = nil, want the stashed terminal Result")
	}
	if !result.IsError {
		t.Fatal("LiveSession.Result.IsError = false, want true for the error-turn scenario")
	}
	if HeadlessOutcome(result) {
		t.Fatal("HeadlessOutcome(ls.Result) = true, want false (§8.1: result+error -> failure)")
	}
}

// TestHeadlessSpawnWriteInputReturnsError checks WriteInput's headless
// guard end to end: called against a real (still-running, thanks to the
// "slow" scenario's sleep) headless LiveSession, it must return
// ErrHeadlessNoInput rather than dereferencing the nil PTYMaster.
func TestHeadlessSpawnWriteInputReturnsError(t *testing.T) {
	r, _, spec, _ := newHeadlessTestRegistry(t, "slow")

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := r.WriteInput(context.Background(), spec.ID, []byte("hello\n")); !errors.Is(err, ErrHeadlessNoInput) {
		t.Fatalf("WriteInput on a headless session = %v, want ErrHeadlessNoInput", err)
	}
}

// TestHeadlessOutcome is a focused unit test of §8.1's attempt-outcome
// mapping in isolation, including the "no result captured at all" branch
// (nil Result — e.g. the child was killed mid-turn) that a real spawn
// cannot cheaply exercise deterministically.
func TestHeadlessOutcome(t *testing.T) {
	cases := []struct {
		name   string
		result *streamjson.Result
		want   bool
	}{
		{"nil result (no result captured)", nil, false},
		{"result success", &streamjson.Result{IsError: false}, true},
		{"result error", &streamjson.Result{IsError: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HeadlessOutcome(tc.result); got != tc.want {
				t.Fatalf("HeadlessOutcome(%+v) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
}

// eventDataFor finds the first event of kind and decodes its DataJSON,
// failing the test if no such event exists.
func eventDataFor(t *testing.T, events []*session.Event, kind session.EventKind) map[string]any {
	t.Helper()
	for _, e := range events {
		if e.Kind != kind {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(e.DataJSON), &data); err != nil {
			t.Fatalf("unmarshaling %q event data %q: %v", kind, e.DataJSON, err)
		}
		return data
	}
	t.Fatalf("no %q event found among %v", kind, eventKinds(events))
	return nil
}
