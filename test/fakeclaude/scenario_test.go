package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// stubCommand is the "relay" each scenario fire_hook execs in these tests: it
// echoes the secret it saw and then cats its stdin, so the hooks-<N>.json
// recording captures (a) that the payload reached the command's stdin and (b)
// which CORRAL_SESSION_SECRET the process actually ran with — enough to prove
// the merge, the {{capture}} interpolation, the per-step env override, and the
// raw-bytes bypass without standing up the daemon.
const stubCommand = `printf 'SECRET=%s\n' "$CORRAL_SESSION_SECRET"; cat`

// writeStubSettings writes a settings.json in the shape corral pins
// (settings.BuildSettingsJSON) but with stubCommand standing in for the real
// relay command, for every event the test scenarios fire.
func writeStubSettings(t *testing.T, dir string) string {
	t.Helper()
	events := []string{
		"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
		"PermissionRequest", "Notification", "Stop",
	}
	hooks := map[string]any{}
	for _, ev := range events {
		hooks[ev] = []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": stubCommand, "timeout": 5}},
		}}
	}
	b, err := json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		t.Fatalf("marshal stub settings: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write stub settings: %v", err)
	}
	return path
}

// runScenarioForTest runs the named scenario from testdata against the stub
// settings, feeding stdinLine on the runner's injected stdin (a plain reader —
// no os.Stdin mutation, so the detached stdin-reader goroutine can never race
// the test), and returns the parsed hook recordings in order plus the exit
// code.
func runScenarioForTest(t *testing.T, name, stdinLine string) ([]hookRecording, int) {
	t.Helper()

	fakeState := t.TempDir()
	fakeHome := t.TempDir()
	settingsPath := writeStubSettings(t, t.TempDir())
	sessionID := "sess-" + name

	scenarioPath := filepath.Join("testdata", "scenarios", name+".json")
	out := bufio.NewWriter(io.Discard)
	code := runScenario(scenarioPath, &scenarioRunner{
		out:          out,
		stdin:        strings.NewReader(stdinLine),
		settingsPath: settingsPath,
		sessionID:    sessionID,
		cwd:          t.TempDir(),
		fakeHome:     fakeHome,
		fakeState:    fakeState,
	})

	return readHookRecordings(t, fakeState, sessionID), code
}

// readHookRecordings loads every hooks-<N>.json under <fakeState>/<sessionID>/
// in N order.
func readHookRecordings(t *testing.T, fakeState, sessionID string) []hookRecording {
	t.Helper()
	dir := filepath.Join(fakeState, sessionID)
	var recs []hookRecording
	for n := 1; ; n++ {
		path := filepath.Join(dir, "hooks-"+strconv.Itoa(n)+".json")
		b, err := os.ReadFile(path)
		if err != nil {
			break
		}
		var rec hookRecording
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestScenarioSmokeTurn(t *testing.T) {
	t.Setenv("CORRAL_SESSION_SECRET", "real-secret")
	recs, code := runScenarioForTest(t, "smoke_turn", "hello world\n")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop.
	if len(recs) != 5 {
		t.Fatalf("got %d hook recordings, want 5:\n%+v", len(recs), recs)
	}

	for i, rec := range recs {
		if rec.ExitCode != 0 {
			t.Errorf("hook %d (%s): exit %d, want 0", i, rec.Event, rec.ExitCode)
		}
		if !strings.Contains(rec.Stdout, "SECRET=real-secret") {
			t.Errorf("hook %d (%s): stdout %q lacks the real secret (env did not propagate)", i, rec.Event, rec.Stdout)
		}
	}

	// The UserPromptSubmit payload must show {{prompt}} interpolated from the
	// captured stdin line, plus the common fields fakeclaude fills.
	ups := findEvent(t, recs, "UserPromptSubmit")
	var payload map[string]any
	if err := json.Unmarshal(ups.Payload, &payload); err != nil {
		t.Fatalf("UserPromptSubmit payload not JSON: %v", err)
	}
	if got := payload["prompt"]; got != "hello world" {
		t.Errorf("prompt = %v, want %q (capture interpolation)", got, "hello world")
	}
	for _, k := range []string{"hook_event_name", "session_id", "transcript_path", "cwd", "permission_mode"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("payload missing common field %q", k)
		}
	}
	if payload["hook_event_name"] != "UserPromptSubmit" {
		t.Errorf("hook_event_name = %v, want UserPromptSubmit", payload["hook_event_name"])
	}
	if payload["session_id"] != "sess-smoke_turn" {
		t.Errorf("session_id = %v, want sess-smoke_turn", payload["session_id"])
	}
	if payload["permission_mode"] != "default" {
		t.Errorf("permission_mode = %v, want default (fakeclaude default)", payload["permission_mode"])
	}
}

func TestScenarioHookAuthForged(t *testing.T) {
	t.Setenv("CORRAL_SESSION_SECRET", "real-secret")
	recs, code := runScenarioForTest(t, "hook_auth_forged", "")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recordings, want 1", len(recs))
	}
	// The per-step env override must win: the relay ran with the forged secret,
	// not the real one from the environment.
	if !strings.Contains(recs[0].Stdout, "SECRET=wrong-secret-forged") {
		t.Errorf("stdout %q does not show the forged secret; per-step env override failed", recs[0].Stdout)
	}
	if strings.Contains(recs[0].Stdout, "SECRET=real-secret") {
		t.Errorf("stdout %q leaked the real secret; override did not replace it", recs[0].Stdout)
	}
}

func TestScenarioUndecodablePayload(t *testing.T) {
	t.Setenv("CORRAL_SESSION_SECRET", "real-secret")
	recs, code := runScenarioForTest(t, "undecodable_payload", "")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recordings, want 1", len(recs))
	}
	// The garbage bytes must reach the relay's stdin verbatim (echoed back by
	// `cat`), and the recording's payload is stored as a JSON string (not a
	// raw, invalid-JSON blob that would have broken MarshalIndent).
	if !strings.Contains(recs[0].Stdout, "this is not valid json {{{") {
		t.Errorf("stdout %q does not contain the raw garbage stdin", recs[0].Stdout)
	}
	var s string
	if err := json.Unmarshal(recs[0].Payload, &s); err != nil {
		t.Fatalf("garbage payload not stored as a JSON string: %v", err)
	}
	if s != "this is not valid json {{{" {
		t.Errorf("stored payload = %q, want the raw garbage", s)
	}
}

func TestScenarioNoHooksAtAll(t *testing.T) {
	t.Setenv("CORRAL_SESSION_SECRET", "real-secret")
	recs, code := runScenarioForTest(t, "no_hooks_at_all", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(recs) != 0 {
		t.Fatalf("got %d recordings, want 0 (scenario fires no hooks)", len(recs))
	}
}

func findEvent(t *testing.T, recs []hookRecording, event string) hookRecording {
	t.Helper()
	for _, rec := range recs {
		if rec.Event == event {
			return rec
		}
	}
	t.Fatalf("no recording for event %q", event)
	return hookRecording{}
}
