package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/djbu/corral/internal/claude/sessions"
)

// scenario is the parsed CORRAL_FAKE_SCENARIO file (design doc §9.1): an
// ordered list of steps that drive a scripted hook-event sequence through the
// *real* pinned relay command. Selected by the CORRAL_FAKE_SCENARIO env var
// M1 reserved (main.go) and this file implements.
type scenario struct {
	Name  string         `json:"name"`
	Steps []scenarioStep `json:"steps"`
}

// scenarioStep is one step. Its fields are a union across the five verbs
// (§9.1: emit_tui, fire_hook, wait_stdin, emit_stdout_jsonl, exit); only the
// fields relevant to Op are populated, the rest decode to their zero value.
type scenarioStep struct {
	Op string `json:"op"`

	// emit_tui: cosmetic text drawn into the transcript region so a human (or
	// a screen-scraping test) sees the fake agent "working".
	Text string `json:"text"`

	// fire_hook: the event name to look up in the pinned settings.json, the
	// event-specific payload to merge over the common fields, and an optional
	// per-step env override. Env exists for the hook_auth_forged scenario
	// (§9.3), which stomps CORRAL_SESSION_SECRET for one invocation to prove
	// the relay still exits 0 and the daemon records hook.unauthorized; it is
	// a field on fire_hook, not a sixth verb.
	Event   string            `json:"event"`
	Payload json.RawMessage   `json:"payload"`
	Env     map[string]string `json:"env"`

	// Raw, when set on a fire_hook, is sent verbatim as the relay's stdin
	// *instead of* a merged JSON payload. It exists for the undecodable_payload
	// scenario (§9.3), which feeds deliberate garbage to prove the relay still
	// exits 0 and the daemon records hook.undecodable rather than erroring. When
	// Raw is set, Payload is ignored.
	Raw string `json:"raw"`

	// wait_stdin: block until a line read from stdin matches Match (a regexp,
	// tested against the raw line including its trailing CR/LF so patterns may
	// anchor on \r$), storing the trimmed line under Capture for later
	// {{Capture}} interpolation. Timeout bounds the wait (a safety guard, not
	// a sleep-synchronizer — §9.1 forbids a sleep verb precisely because every
	// wait here is on a real event).
	//
	// Optional makes a timeout a *normal* outcome (the step simply elapses and
	// the scenario continues) rather than a fatal error. This is how a
	// keep-alive scenario parks a live fake child on a line that never arrives
	// while the contract harness advances its fake clock and asserts — e.g.
	// no_hooks_at_all (session must sit past first_hook_grace) and
	// blocked_permission (the settle timer only fires while the session is
	// alive; an exited child's consumer goroutine is torn down first). A
	// non-optional wait_stdin timeout stays fatal (smoke_turn's prompt must
	// actually arrive).
	Match    string `json:"match"`
	Capture  string `json:"capture"`
	Timeout  string `json:"timeout"`
	Optional bool   `json:"optional"`

	// exit: terminate the fake session with this code.
	Code int `json:"code"`
}

// hookRecording is what fire_hook captures per invocation into
// $CORRAL_FAKE_STATE/<session-id>/hooks-<N>.json (§9.2 step 5). It carries
// enough for a contract test to assert the relay was inert (ExitCode == 0,
// Stdout == "") and fast (DurationMs < hook_timeout) — i.e. that corral did
// not perturb the agent — and to diagnose a broken settings.json, a missing
// binary, or a lost CORRAL_SESSION_SECRET as a test failure rather than a
// silent pass.
type hookRecording struct {
	Event      string          `json:"event"`
	Command    string          `json:"command"`
	Payload    json.RawMessage `json:"payload"`
	ExitCode   int             `json:"exit_code"`
	Stdout     string          `json:"stdout"`
	Stderr     string          `json:"stderr"`
	DurationMs int64           `json:"duration_ms"`
}

// scenarioRunner holds the ambient state a running scenario needs: where the
// pinned settings live (to resolve each event's real relay command), the
// identity fields fire_hook fills into every payload, the transcript row
// cursor emit_tui advances, the stdin line feed wait_stdin consumes, and the
// capture table {{name}} interpolation reads.
type scenarioRunner struct {
	out          *bufio.Writer
	stdin        io.Reader
	settingsPath string
	sessionID    string
	cwd          string
	fakeHome     string
	fakeState    string

	row      int
	captures map[string]string
	lineCh   <-chan string
}

// pinnedSettings is the subset of settings.json fire_hook reads: the exact
// command corral wrote for each event (settings.BuildSettingsJSON). Only the
// command string is needed; matcher/type/timeout are irrelevant to replay.
type pinnedSettings struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

var captureRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// runScenario loads the scenario at path and executes its steps in order,
// returning the process exit code. Steps run synchronously — each fire_hook's
// relay subprocess is awaited before the next step — so event ordering is
// deterministic and never racy (a barrier the contract-suite state assertions
// depend on). A scenario that omits a trailing exit step falls off the end and
// returns 0.
func runScenario(path string, r *scenarioRunner) int {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: scenario: read %s: %v\n", path, err)
		return 1
	}
	var sc scenario
	if err := json.Unmarshal(b, &sc); err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: scenario: parse %s: %v\n", path, err)
		return 1
	}

	// A single stdin reader feeds every wait_stdin step. Lines are read raw
	// (ReadString keeps the trailing CR/LF) so scenario Match patterns can
	// anchor on \r$ the way real terminal input arrives.
	lineCh := make(chan string)
	go func() {
		reader := bufio.NewReader(r.stdin)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				lineCh <- line
			}
			if err != nil {
				close(lineCh)
				return
			}
		}
	}()
	r.lineCh = lineCh
	r.row = transcriptStartRow
	r.captures = map[string]string{}

	for i, st := range sc.Steps {
		if code, done := r.runStep(i, st); done {
			return code
		}
	}
	return 0
}

// runStep dispatches one step by its op. The bool return is true when the step
// terminates the scenario (an exit step, or a fatal engine error), in which
// case the int is the exit code.
func (r *scenarioRunner) runStep(i int, st scenarioStep) (int, bool) {
	switch st.Op {
	case "emit_tui":
		if err := writeTranscriptLine(r.out, r.row, r.interpolate(st.Text)); err != nil {
			fmt.Fprintf(os.Stderr, "fakeclaude: scenario step %d (emit_tui): %v\n", i, err)
			return 1, true
		}
		r.row++
		_ = r.out.Flush()
		return 0, false

	case "fire_hook":
		// A fire_hook failure (bad settings, missing binary) is recorded and
		// then continues rather than aborting: the recording is exactly the
		// artifact a contract test inspects to turn that failure into a red
		// test, and aborting mid-scenario would instead produce a confusing
		// partial-state failure downstream.
		if err := r.fireHook(st); err != nil {
			fmt.Fprintf(os.Stderr, "fakeclaude: scenario step %d (fire_hook %s): %v\n", i, st.Event, err)
		}
		return 0, false

	case "wait_stdin":
		if err := r.waitStdin(st); err != nil {
			fmt.Fprintf(os.Stderr, "fakeclaude: scenario step %d (wait_stdin): %v\n", i, err)
			return 1, true
		}
		return 0, false

	case "emit_stdout_jsonl":
		// Declared but unimplemented — the M4 seam (§9.1). Announce and skip
		// rather than fabricate behavior a contract test might come to rely on.
		fmt.Fprintf(os.Stderr, "fakeclaude: scenario step %d: emit_stdout_jsonl is unimplemented (M4 seam)\n", i)
		return 0, false

	case "exit":
		return st.Code, true

	default:
		fmt.Fprintf(os.Stderr, "fakeclaude: scenario step %d: unknown op %q\n", i, st.Op)
		return 0, false
	}
}

// fireHook implements §9.2: resolve the event's real pinned command, merge the
// step payload over the common fields fakeclaude fills itself, run the command
// via `sh -c` (as Claude Code does) feeding the payload on stdin, and record
// the outcome to hooks-<N>.json.
func (r *scenarioRunner) fireHook(st scenarioStep) error {
	command, err := r.commandForEvent(st.Event)
	if err != nil {
		return err
	}

	// Raw bypasses payload construction entirely (garbage-stdin scenarios);
	// otherwise merge the step payload over fakeclaude's common fields.
	var payload []byte
	if st.Raw != "" {
		payload = []byte(st.Raw)
	} else {
		payload, err = r.mergePayload(st.Event, st.Payload)
		if err != nil {
			return err
		}
	}

	// Run through a shell — Claude Code execs hook commands via the shell, so
	// exec'ing directly would let fakeclaude quote-handle the absolute binary
	// path differently than production and hide exactly the class of bug the
	// contract suite exists to catch (§9.2 step 4).
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = envWithOverrides(st.Env)

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			// The command could not be started at all (missing sh, etc.).
			// Record a sentinel so the test sees a failure, not a silent zero.
			exitCode = -1
			stderr.WriteString(runErr.Error())
		}
	}

	rec := hookRecording{
		Event:      st.Event,
		Command:    command,
		Payload:    safePayloadJSON(payload),
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: elapsed.Milliseconds(),
	}
	return r.recordHook(rec)
}

// commandForEvent reads the pinned settings.json and returns the exact command
// string corral wrote for event. A missing settings path, unreadable file, or
// unregistered event is a scenario authoring error surfaced as an error (the
// caller records and continues) rather than a fabricated command.
func (r *scenarioRunner) commandForEvent(event string) (string, error) {
	if r.settingsPath == "" {
		return "", fmt.Errorf("no --settings file (cannot resolve relay command)")
	}
	b, err := os.ReadFile(r.settingsPath)
	if err != nil {
		return "", fmt.Errorf("read settings %s: %w", r.settingsPath, err)
	}
	var ps pinnedSettings
	if err := json.Unmarshal(b, &ps); err != nil {
		return "", fmt.Errorf("parse settings %s: %w", r.settingsPath, err)
	}
	entries, ok := ps.Hooks[event]
	if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
		return "", fmt.Errorf("event %q not registered in settings", event)
	}
	command := entries[0].Hooks[0].Command
	if command == "" {
		return "", fmt.Errorf("event %q has an empty command", event)
	}
	return command, nil
}

// mergePayload builds the JSON fed to the relay's stdin: fakeclaude's own
// common fields (§9.2 step 3) with the scenario's event payload merged over
// them (scenario wins on conflict), then {{capture}} interpolation applied to
// every string value.
func (r *scenarioRunner) mergePayload(event string, raw json.RawMessage) ([]byte, error) {
	merged := map[string]any{
		"hook_event_name": event,
		"session_id":      r.sessionID,
		"transcript_path": sessions.TranscriptPath(r.fakeHome, r.cwd, r.sessionID),
		"cwd":             r.cwd,
		"permission_mode": "default",
	}
	if len(raw) > 0 {
		var overlay map[string]any
		if err := json.Unmarshal(raw, &overlay); err != nil {
			return nil, fmt.Errorf("payload is not a JSON object: %w", err)
		}
		for k, v := range overlay {
			merged[k] = v
		}
	}
	interpolateValue(merged, r.captures)
	return json.Marshal(merged)
}

// waitStdin blocks until a line matching st.Match arrives on stdin or the
// timeout elapses. On match, the trimmed line (or regexp submatch group 1 when
// the pattern has one) is stored under st.Capture for later interpolation.
//
// When st.Optional is set, a line never arriving is a *normal* outcome: both a
// timeout and stdin closing without a match return nil so the scenario simply
// continues. This is how a keep-alive step (no_hooks_at_all, blocked_permission)
// parks a live fake child on a line that never comes while the contract harness
// advances its fake clock and asserts — the harness tears the child down when
// done, and the generous timeout is only a backstop against an orphaned fake
// outliving its test. A non-optional wait treats both as fatal (smoke_turn's
// prompt must actually arrive).
func (r *scenarioRunner) waitStdin(st scenarioStep) error {
	re, err := regexp.Compile(st.Match)
	if err != nil {
		return fmt.Errorf("bad match regexp %q: %w", st.Match, err)
	}
	timeout := 30 * time.Second
	if st.Timeout != "" {
		d, err := time.ParseDuration(st.Timeout)
		if err != nil {
			return fmt.Errorf("bad timeout %q: %w", st.Timeout, err)
		}
		timeout = d
	}
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-r.lineCh:
			if !ok {
				if st.Optional {
					return nil // no more input will arrive; parking step ends.
				}
				return fmt.Errorf("stdin closed before a line matched %q", st.Match)
			}
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if st.Capture != "" {
				captured := strings.TrimRight(line, "\r\n")
				if len(m) > 1 {
					captured = m[1]
				}
				r.captures[st.Capture] = captured
			}
			return nil
		case <-deadline:
			if st.Optional {
				return nil // parked long enough; harness has asserted and moved on.
			}
			return fmt.Errorf("timed out after %s waiting for a line matching %q", timeout, st.Match)
		}
	}
}

// recordHook writes rec to the next hooks-<N>.json under
// <fakeState>/<sessionID>/, mirroring recordInvocation's numbering so a
// scenario firing several hooks produces hooks-1.json, hooks-2.json, ….
func (r *scenarioRunner) recordHook(rec hookRecording) error {
	if r.fakeState == "" || r.sessionID == "" {
		return nil // nothing to record against (mirrors main's invocation guard).
	}
	dir := filepath.Join(r.fakeState, r.sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	n, err := nextHookNumber(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("hooks-%d.json", n))
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hook recording: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// safePayloadJSON returns payload as-is when it is valid JSON (the common
// case), or a JSON string quoting the raw bytes when it is not (the
// undecodable_payload scenario). Without this, MarshalIndent of a
// hookRecording carrying a garbage RawMessage would itself fail, losing the
// very recording the contract test needs to see the garbage was sent.
func safePayloadJSON(payload []byte) json.RawMessage {
	if json.Valid(payload) {
		return json.RawMessage(payload)
	}
	quoted, _ := json.Marshal(string(payload))
	return json.RawMessage(quoted)
}

// nextHookNumber returns one past the largest N in existing hooks-<N>.json
// files under dir (1 if none), the hook analogue of nextInvocationNumber.
func nextHookNumber(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, fmt.Errorf("readdir %s: %w", dir, err)
	}
	maxN := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "hooks-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "hooks-"), ".json")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		if n > maxN {
			maxN = n
		}
	}
	return maxN + 1, nil
}

// interpolate substitutes {{name}} with the captured value for name (empty
// string if uncaptured), for a bare string.
func (r *scenarioRunner) interpolate(s string) string {
	return captureRe.ReplaceAllStringFunc(s, func(m string) string {
		name := captureRe.FindStringSubmatch(m)[1]
		return r.captures[name]
	})
}

// interpolateValue walks a decoded JSON value in place, applying {{name}}
// substitution to every string it contains (§9.1: "a literal string
// substitution into payload string values only").
func interpolateValue(v any, captures map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				t[k] = substitute(s, captures)
			} else {
				interpolateValue(val, captures)
			}
		}
	case []any:
		for i, val := range t {
			if s, ok := val.(string); ok {
				t[i] = substitute(s, captures)
			} else {
				interpolateValue(val, captures)
			}
		}
	}
}

func substitute(s string, captures map[string]string) string {
	return captureRe.ReplaceAllStringFunc(s, func(m string) string {
		name := captureRe.FindStringSubmatch(m)[1]
		return captures[name]
	})
}

// envWithOverrides returns the current environment with the given keys
// overridden (or added). Used by fire_hook's optional per-step env — notably
// hook_auth_forged stomping CORRAL_SESSION_SECRET. A nil/empty map returns the
// environment unchanged.
func envWithOverrides(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if _, ok := overrides[key]; ok {
			continue // replaced below
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}
