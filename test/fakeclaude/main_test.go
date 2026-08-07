package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// --- parseArgs -----------------------------------------------------------

func TestParseArgsKnownFlagsLongAndShort(t *testing.T) {
	got := parseArgs([]string{
		"--session-id", "abc-123",
		"--settings", "/x/settings.json",
		"--setting-sources", "user,project,local",
		"--model", "opus",
		"-n", "corral-1",
	})
	want := fakeSpec{
		SessionID:      "abc-123",
		Settings:       "/x/settings.json",
		SettingSources: "user,project,local",
		Model:          "opus",
		Name:           "corral-1",
	}
	if got != want {
		t.Fatalf("parseArgs = %+v, want %+v", got, want)
	}
}

func TestParseArgsResumeShortFlag(t *testing.T) {
	got := parseArgs([]string{"-r", "sess-1", "--name", "corral-1"})
	if got.Resume != "sess-1" || got.Name != "corral-1" {
		t.Fatalf("parseArgs = %+v", got)
	}
	if got.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty when only -r is given", got.SessionID)
	}
}

func TestParseArgsUnknownFlagsDoNotDie(t *testing.T) {
	got := parseArgs([]string{
		"--totally-unknown", "value",
		"--session-id", "abc-123",
		"positional-token",
		"--name", "corral-1",
	})
	if got.SessionID != "abc-123" || got.Name != "corral-1" {
		t.Fatalf("known flags after unknown ones were not parsed: %+v", got)
	}
}

// --- record.go -------------------------------------------------------------

func TestRecordInvocationNumbering(t *testing.T) {
	dir := t.TempDir()
	inv := Invocation{Argv: []string{"fakeclaude"}, Environ: []string{"HOME=/x"}, Cwd: "/x"}

	p1, err := recordInvocation(dir, "sess-1", inv)
	if err != nil {
		t.Fatalf("recordInvocation 1: %v", err)
	}
	if filepath.Base(p1) != "invocation-1.json" {
		t.Fatalf("first invocation path = %q, want basename invocation-1.json", p1)
	}

	p2, err := recordInvocation(dir, "sess-1", inv)
	if err != nil {
		t.Fatalf("recordInvocation 2: %v", err)
	}
	if filepath.Base(p2) != "invocation-2.json" {
		t.Fatalf("second invocation path = %q, want basename invocation-2.json", p2)
	}

	// A different session's numbering starts independently at 1.
	p3, err := recordInvocation(dir, "sess-2", inv)
	if err != nil {
		t.Fatalf("recordInvocation sess-2: %v", err)
	}
	if filepath.Base(p3) != "invocation-1.json" {
		t.Fatalf("first invocation for a different session = %q, want basename invocation-1.json", p3)
	}
}

func TestRecordInvocationContentRoundTrips(t *testing.T) {
	dir := t.TempDir()
	inv := Invocation{
		Argv:     []string{"fakeclaude", "--session-id", "abc"},
		Environ:  []string{"HOME=/x", "PATH=/usr/bin"},
		Cwd:      "/work",
		Settings: json.RawMessage(`{"hooks": {}}`),
	}
	path, err := recordInvocation(dir, "abc", inv)
	if err != nil {
		t.Fatalf("recordInvocation: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got Invocation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Argv, inv.Argv) || !reflect.DeepEqual(got.Environ, inv.Environ) || got.Cwd != inv.Cwd {
		t.Fatalf("round-tripped invocation = %+v, want %+v", got, inv)
	}
}

func TestSettingsRawMessageValidAndInvalid(t *testing.T) {
	dir := t.TempDir()

	validPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(validPath, []byte(`{"hooks": {}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := settingsRawMessage(validPath); !json.Valid(got) {
		t.Fatalf("settingsRawMessage(valid) = %s, not valid JSON", got)
	} else if string(got) != `{"hooks": {}}` {
		t.Fatalf("settingsRawMessage(valid) = %s, want the file's exact bytes", got)
	}

	invalidPath := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalidPath, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := settingsRawMessage(invalidPath); !json.Valid(got) {
		t.Fatalf("settingsRawMessage(invalid) = %s, want a valid JSON fallback string", got)
	}

	if got := settingsRawMessage(filepath.Join(dir, "missing.json")); !json.Valid(got) {
		t.Fatalf("settingsRawMessage(missing) = %s, want a valid JSON fallback string", got)
	}
}

// --- transcript.go ----------------------------------------------------------

func TestTranscriptPathMatchesSessionsPackage(t *testing.T) {
	got := transcriptPath("/home/ci/.fakehome", "/home/ci/work/project", "sess-1")
	want := "/home/ci/.fakehome/.claude/projects/-home-ci-work-project/sess-1.jsonl"
	if got != want {
		t.Fatalf("transcriptPath = %q, want %q", got, want)
	}
}

func TestTranscriptRoundTrip(t *testing.T) {
	fakeHome := t.TempDir()
	cwd := "/work/project"
	sessionID := "sess-1"

	prior, err := loadTranscript(fakeHome, cwd, sessionID)
	if err != nil {
		t.Fatalf("loadTranscript(never written) = %v, want nil error", err)
	}
	if prior != nil {
		t.Fatalf("loadTranscript(never written) = %v, want nil slice", prior)
	}

	if err := appendPrompt(fakeHome, cwd, sessionID, "hello world"); err != nil {
		t.Fatalf("appendPrompt 1: %v", err)
	}
	if err := appendPrompt(fakeHome, cwd, sessionID, "second prompt"); err != nil {
		t.Fatalf("appendPrompt 2: %v", err)
	}

	got, err := loadTranscript(fakeHome, cwd, sessionID)
	if err != nil {
		t.Fatalf("loadTranscript: %v", err)
	}
	if len(got) != 2 || got[0].Text != "hello world" || got[1].Text != "second prompt" {
		t.Fatalf("loadTranscript = %+v, want [hello world, second prompt] in order", got)
	}
	for _, e := range got {
		if _, err := time.Parse(time.RFC3339Nano, e.At); err != nil {
			t.Errorf("entry %+v: At does not parse as RFC3339Nano: %v", e, err)
		}
	}
}

// --- tui.go -----------------------------------------------------------------

func TestWriteStartupBeginsWithExactSequenceAndIsDeterministic(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	if err := writeStartup(&buf1, "corral-1"); err != nil {
		t.Fatalf("writeStartup 1: %v", err)
	}
	if err := writeStartup(&buf2, "corral-1"); err != nil {
		t.Fatalf("writeStartup 2: %v", err)
	}

	if !bytes.HasPrefix(buf1.Bytes(), []byte(startupSequence)) {
		t.Fatalf("writeStartup output does not begin with the exact startup sequence:\n%q", buf1.Bytes())
	}
	if !bytes.Contains(buf1.Bytes(), []byte("\x1b]0;fakeclaude\x07")) {
		t.Fatalf("writeStartup output missing the OSC 0 window title:\n%q", buf1.Bytes())
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Fatal("writeStartup is not deterministic for the same name")
	}
}

// TestStartupSequenceMatchesM0Capture guards against startupSequence
// drifting from prose-derived guesswork: it asserts the constant is a
// byte-exact, contiguous substring of testdata/pty/claude-tui-startup.raw
// — the real capture design doc §10.1 item 3 says fakeclaude must
// mirror. The real capture also has a short preamble
// (ESC7 ESC[r ESC8 ESC[?25h, a save-cursor/scroll-region/restore-cursor/
// show-cursor sequence) before ESC[?1049h, and a different OSC 0 title
// ("Claude Code" vs. fakeclaude's own "fakeclaude") — both intentionally
// out of scope per design doc §10.1 item 3's literal spec, which starts
// at ESC[?1049h and never mentions the preamble or the real title text.
// This test only holds the mode-set-through-ESC[?2031h portion to the
// real bytes, which is the part the screen package's mode sniffer (step
// 5's keystone test) actually exercises.
func TestStartupSequenceMatchesM0Capture(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/pty/claude-tui-startup.raw")
	if err != nil {
		t.Fatalf("reading M0 capture: %v", err)
	}
	if !bytes.Contains(raw, []byte(startupSequence)) {
		t.Fatalf("startupSequence is not a contiguous substring of the real M0 capture — it has drifted from what claude actually emits:\nconstant: %q", startupSequence)
	}
}

func TestWriteTranscriptLineIsCursorAddressedNotAppendOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTranscriptLine(&buf, 7, "hello"); err != nil {
		t.Fatalf("writeTranscriptLine: %v", err)
	}
	got := buf.String()
	if !bytes.HasPrefix([]byte(got), []byte("\x1b[7;1H\x1b[2K")) {
		t.Fatalf("writeTranscriptLine did not cursor-address+clear row 7 first: %q", got)
	}
	if !bytes.Contains([]byte(got), []byte("hello")) {
		t.Fatalf("writeTranscriptLine output missing text: %q", got)
	}
}

// --- self-test: build the real binary, run it twice under a real PTY -----
//
// Design doc §11 step 7: "a fakeclaude self-test asserting it enters alt
// screen, records its invocation, and round-trips a transcript through
// --resume." This is the one black-box test in the package: everything
// above tests fakeclaude's pieces in isolation, but only actually
// exec'ing the compiled binary under a PTY (the same way
// supervisor.Spawn will in step 9) proves the pieces are wired together
// correctly end to end.

func TestFakeclaudeSelfTest(t *testing.T) {
	bin := buildFakeclaude(t)

	fakeHome := t.TempDir()
	fakeState := t.TempDir()
	cwd := t.TempDir()
	sessionID := "11111111-1111-1111-1111-111111111111"

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"hooks": {}}`), 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}

	// CORRAL_FAKE_EXIT_AFTER/CODE give a deterministic, signal-race-free
	// way for the child to terminate on its own — the test just blocks on
	// cmd.Wait(), no time.Sleep on the test side.
	baseEnv := []string{
		"CORRAL_FAKE_HOME=" + fakeHome,
		"CORRAL_FAKE_STATE=" + fakeState,
		"CORRAL_FAKE_EXIT_AFTER=150ms",
		"CORRAL_FAKE_EXIT_CODE=0",
		"PATH=" + os.Getenv("PATH"),
	}

	// --- Run 1: fresh spawn --------------------------------------------

	argv1 := []string{
		"--session-id", sessionID,
		"--settings", settingsPath,
		"--setting-sources", "user,project,local",
		"--name", "corral-1",
		"--an-unknown-future-flag", "should-not-kill-it",
	}
	out1 := runFakeclaude(t, bin, argv1, baseEnv, cwd, "hello world\n")

	if !bytes.HasPrefix(out1, []byte(startupSequence)) {
		t.Fatalf("run1: output does not begin with the exact alt-screen startup sequence:\n%q", out1)
	}

	inv1 := readInvocation(t, fakeState, sessionID, 1)
	if len(inv1.Argv) == 0 {
		t.Fatal("run1: invocation-1.json argv is empty")
	}
	if !containsArg(inv1.Argv, "--session-id") || !containsArg(inv1.Argv, sessionID) {
		t.Fatalf("run1: invocation-1.json argv missing --session-id/%s: %v", sessionID, inv1.Argv)
	}
	if containsArg(inv1.Argv, "--resume") {
		t.Fatal("run1: invocation-1.json argv must not contain --resume (this was a fresh spawn)")
	}
	if len(inv1.Environ) == 0 {
		t.Fatal("run1: invocation-1.json environ is empty")
	}
	wantCwd, _ := filepath.EvalSymlinks(cwd)
	gotCwd, _ := filepath.EvalSymlinks(inv1.Cwd)
	if gotCwd != wantCwd {
		t.Fatalf("run1: invocation-1.json cwd = %q, want %q", inv1.Cwd, cwd)
	}
	var settingsParsed map[string]json.RawMessage
	if err := json.Unmarshal(inv1.Settings, &settingsParsed); err != nil {
		t.Fatalf("run1: invocation-1.json settings did not parse: %v (%s)", err, inv1.Settings)
	}
	if _, ok := settingsParsed["hooks"]; !ok {
		t.Fatalf("run1: invocation-1.json settings missing the hooks key: %s", inv1.Settings)
	}

	// --- Run 2: --resume, same session, no new stdin -------------------

	argv2 := []string{
		"--resume", sessionID,
		"--settings", settingsPath,
		"--setting-sources", "user,project,local",
		"--name", "corral-1",
	}
	out2 := runFakeclaude(t, bin, argv2, baseEnv, cwd, "")

	if !bytes.HasPrefix(out2, []byte(startupSequence)) {
		t.Fatalf("run2 (resume): output does not begin with the exact alt-screen startup sequence:\n%q", out2)
	}
	if !bytes.Contains(out2, []byte("hello world")) {
		t.Fatalf("run2 (resume): output does not contain run1's prompt text — transcript did not round-trip through --resume:\n%q", out2)
	}

	inv2 := readInvocation(t, fakeState, sessionID, 2)
	if !containsArg(inv2.Argv, "--resume") || !containsArg(inv2.Argv, sessionID) {
		t.Fatalf("run2: invocation-2.json argv missing --resume/%s: %v", sessionID, inv2.Argv)
	}
	if containsArg(inv2.Argv, "--session-id") {
		t.Fatal("run2: invocation-2.json argv must not contain --session-id (this was a resume)")
	}
}

func containsArg(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

func readInvocation(t *testing.T, fakeState, sessionID string, n int) Invocation {
	t.Helper()
	path := filepath.Join(fakeState, sessionID, "invocation-"+strconv.Itoa(n)+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var inv Invocation
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return inv
}

func buildFakeclaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakeclaude")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/danielbecerra/corral/test/fakeclaude")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build fakeclaude: %v\n%s", err, out)
	}
	return bin
}

// runFakeclaude execs bin under a real PTY (mirroring how
// supervisor.Spawn will in step 9), optionally writing stdinText to it,
// and returns everything it wrote to the PTY master before exiting.
//
// stdinText is written only once fakeclaude's own startupSequence has
// actually appeared in its output — not immediately after pty.Start —
// because fakeclaude disables local tty echo itself shortly after
// starting (see rawmode.go): writing stdin before that happens would
// race the line discipline's default echo-on state and double up the
// typed text in the captured output. This is a real readiness signal
// (data received), not a fixed delay, so it introduces no
// time.Sleep-based flakiness on either side.
//
// Termination is left to the child itself via
// CORRAL_FAKE_EXIT_AFTER/CODE in env, so the only remaining
// synchronization is cmd.Wait() plus draining the PTY master.
func runFakeclaude(t *testing.T, bin string, argv, env []string, cwd, stdinText string) []byte {
	t.Helper()

	cmd := exec.Command(bin, argv...)
	cmd.Dir = cwd
	cmd.Env = env

	master, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	readyCh := make(chan struct{})
	var readyOnce sync.Once
	done := make(chan struct{})

	go func() {
		defer close(done)
		chunk := make([]byte, 4096)
		for {
			n, readErr := master.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf.Write(chunk[:n])
				// Gate readiness on the alt-screen entry alone, not the
				// full startupSequence: if someone reorders/truncates the
				// constant, this keeps failing fast on the actual
				// bytes.HasPrefix(out, startupSequence) assertions below
				// instead of masking the regression behind a 5s
				// "timed out waiting for..." that reads like flake.
				seen := bytes.Contains(buf.Bytes(), []byte("\x1b[?1049h"))
				mu.Unlock()
				if seen {
					readyOnce.Do(func() { close(readyCh) })
				}
			}
			if readErr != nil {
				// A closed PTY errors on read by design
				// (platform-dependent EOF vs. an I/O error) — only the
				// bytes collected before that matter here.
				return
			}
		}
	}()

	if stdinText != "" {
		select {
		case <-readyCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for fakeclaude's startup sequence before sending stdin")
		}
		if _, err := master.Write([]byte(stdinText)); err != nil {
			t.Fatalf("writing to pty master: %v", err)
		}
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("fakeclaude exited with error: %v", err)
	}
	_ = master.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining fakeclaude's pty output")
	}

	mu.Lock()
	defer mu.Unlock()
	return buf.Bytes()
}
