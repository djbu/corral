package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/danielbecerra/corral/internal/procinfo"
	"github.com/danielbecerra/corral/internal/session"
)

func baseSpec() session.Spec {
	return session.Spec{
		ID:             "11111111-1111-1111-1111-111111111111",
		Name:           "corral-1",
		Mode:           session.ModeInteractive,
		Cwd:            "/tmp",
		ClaudeBin:      "/usr/local/bin/claude",
		SettingSources: "user,project,local",
		SettingsPath:   "/home/ci/.corral/sessions/id/settings.json",
		Env:            map[string]string{"PATH": "/usr/bin:/bin"},
		Rows:           40,
		Cols:           120,
	}
}

// --- BuildArgv ---------------------------------------------------------

func TestBuildArgvFreshNoModel(t *testing.T) {
	spec := baseSpec()
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--session-id", spec.ID,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(fresh, no model) =\n%v\nwant\n%v", got, want)
	}
}

func TestBuildArgvFreshWithModel(t *testing.T) {
	spec := baseSpec()
	spec.Model = "opus"
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--session-id", spec.ID,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
		"--model", "opus",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(fresh, model) =\n%v\nwant\n%v", got, want)
	}
}

func TestBuildArgvInteractiveWithPermissionMode(t *testing.T) {
	spec := baseSpec()
	spec.PermissionMode = "manual"
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--session-id", spec.ID,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
		"--permission-mode", "manual",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(interactive, permission-mode) =\n%v\nwant\n%v", got, want)
	}
}

func TestBuildArgvResume(t *testing.T) {
	spec := baseSpec()
	spec.ResumeFrom = "22222222-2222-2222-2222-222222222222"
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--resume", spec.ResumeFrom,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(resume) =\n%v\nwant\n%v", got, want)
	}
	for _, a := range got {
		if a == "--session-id" {
			t.Fatal("resume argv must not contain --session-id")
		}
	}
}

func TestBuildArgvResumeWithModel(t *testing.T) {
	spec := baseSpec()
	spec.ResumeFrom = "22222222-2222-2222-2222-222222222222"
	spec.Model = "haiku"
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--resume", spec.ResumeFrom,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
		"--model", "haiku",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(resume, model) =\n%v\nwant\n%v", got, want)
	}
}

func headlessSpec() session.Spec {
	spec := baseSpec()
	spec.Mode = session.ModeHeadless
	spec.Prompt = "fix the bug in main.go"
	return spec
}

// TestBuildArgvHeadlessExecCarriesRealPrompt is design doc §5.1's exec
// copy: redact=false must carry the literal prompt text, in the exact
// order the doc specifies (-p, --output-format stream-json, --verbose),
// and never emit --input-format.
func TestBuildArgvHeadlessExecCarriesRealPrompt(t *testing.T) {
	spec := headlessSpec()
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--session-id", spec.ID,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
		"-p", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(headless, exec) =\n%v\nwant\n%v", got, want)
	}
	for _, a := range got {
		if a == "--input-format" {
			t.Fatal("headless argv must never contain --input-format (§5.1: no live child to inject a frame into)")
		}
	}
}

// TestBuildArgvHeadlessRedactedCarriesPlaceholder is §5.1's persisted copy:
// redact=true must replace the prompt value with a byte-count placeholder,
// never the real text, so the sessions.argv column never carries the full
// prompt.
func TestBuildArgvHeadlessRedactedCarriesPlaceholder(t *testing.T) {
	spec := headlessSpec()
	got := BuildArgv(spec, true)

	found := false
	for i, a := range got {
		if a == "-p" {
			found = true
			if i+1 >= len(got) {
				t.Fatal("-p has no following value")
			}
			want := fmt.Sprintf("<prompt:%d bytes>", len(spec.Prompt))
			if got[i+1] != want {
				t.Fatalf("-p value = %q, want placeholder %q", got[i+1], want)
			}
			if got[i+1] == spec.Prompt {
				t.Fatal("redacted argv must not carry the real prompt text")
			}
		}
	}
	if !found {
		t.Fatal("redacted headless argv missing -p")
	}
}

// TestBuildArgvHeadlessWithPermissionMode checks --permission-mode is
// appended last, after --verbose, when Spec.PermissionMode is non-empty
// (§5.1/§5.3).
func TestBuildArgvHeadlessWithPermissionMode(t *testing.T) {
	spec := headlessSpec()
	spec.PermissionMode = "acceptEdits"
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--session-id", spec.ID,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
		"-p", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(headless, permission-mode) =\n%v\nwant\n%v", got, want)
	}
}

// TestBuildArgvHeadlessWithModel checks --model still slots in before the
// headless-only flags, same position as the interactive case.
func TestBuildArgvHeadlessWithModel(t *testing.T) {
	spec := headlessSpec()
	spec.Model = "opus"
	got := BuildArgv(spec, false)
	want := []string{
		spec.ClaudeBin,
		"--session-id", spec.ID,
		"--settings", spec.SettingsPath,
		"--setting-sources", spec.SettingSources,
		"--name", spec.Name,
		"--model", "opus",
		"-p", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgv(headless, model) =\n%v\nwant\n%v", got, want)
	}
}

// TestBuildArgvHeadlessNoPermissionModeOmitsFlag checks the empty-string
// case explicitly (Spec.PermissionMode's zero value must never render as
// `--permission-mode ""`).
func TestBuildArgvHeadlessNoPermissionModeOmitsFlag(t *testing.T) {
	spec := headlessSpec()
	got := BuildArgv(spec, false)
	for _, a := range got {
		if a == "--permission-mode" {
			t.Fatal("--permission-mode must be omitted when Spec.PermissionMode is empty")
		}
	}
}

// --- BuildEnv -----------------------------------------------------------

// TestBuildEnvExactKeySet is design doc §10.2's supervisor/spawn row:
// "env construction — asserts the exact key set, asserts no CLAUDE_* key
// is present even when the frozen snapshot contains one".
func TestBuildEnvExactKeySet(t *testing.T) {
	spec := baseSpec()
	snapshot := map[string]string{
		"HOME":                 "/home/ci",
		"USER":                 "ci",
		"LOGNAME":              "ci",
		"SHELL":                "/bin/bash",
		"PATH":                 "/usr/bin:/bin",
		"TMPDIR":               "/tmp",
		"LANG":                 "en_US.UTF-8",
		"LC_ALL":               "en_US.UTF-8",
		"LC_CTYPE":             "en_US.UTF-8",
		"TZ":                   "UTC",
		"SSH_AUTH_SOCK":        "/tmp/ssh.sock",
		"ANTHROPIC_API_KEY":    "sk-ant-abc",
		"ANTHROPIC_AUTH_TOKEN": "tok-abc",
		"ANTHROPIC_BASE_URL":   "https://api.anthropic.com",
		// Ambient/unrelated entries that must never be copied: not on the
		// whitelist, not in passthrough. CLAUDE_CODE_CHILD_SESSION is
		// finding #3's exact leaked variable.
		"CLAUDE_CODE_CHILD_SESSION": "leaked-value",
		"CLAUDE_API_KEY":            "should-not-leak",
		"EDITOR":                    "vim",
		// A snapshot entry under this name must never be what ends up in
		// the child's CORRAL_SESSION_SECRET — envWhitelist deliberately
		// excludes it (spawn.go's doc comment), so BuildEnv can only ever
		// set it from its own sessionSecret parameter below.
		"CORRAL_SESSION_SECRET": "must-not-be-inherited-from-snapshot",
	}

	env := BuildEnv(spec, snapshot, nil, "", "/home/ci/.corral/corral.sock", "the-real-secret")

	wantKeys := []string{
		"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TMPDIR",
		"LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSH_AUTH_SOCK",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"TERM", "COLORTERM", "PWD", "CORRAL_SESSION_ID", "CORRAL_SOCK",
		"CORRAL_SESSION_SECRET",
	}
	assertExactKeySet(t, env, wantKeys)

	for k := range env {
		if strings.HasPrefix(k, "CLAUDE_") {
			t.Errorf("env contains a CLAUDE_* key %q, which must never be present", k)
		}
	}
	if env["CORRAL_SESSION_SECRET"] != "the-real-secret" {
		t.Errorf("CORRAL_SESSION_SECRET = %q, want the-real-secret (the sessionSecret argument, never the snapshot value)", env["CORRAL_SESSION_SECRET"])
	}
	if env["TERM"] != "xterm-256color" {
		t.Errorf("TERM = %q, want default xterm-256color when term arg is empty", env["TERM"])
	}
	if env["COLORTERM"] != "truecolor" {
		t.Errorf("COLORTERM = %q, want truecolor", env["COLORTERM"])
	}
	if env["PWD"] != spec.Cwd {
		t.Errorf("PWD = %q, want spec.Cwd %q", env["PWD"], spec.Cwd)
	}
	if env["CORRAL_SESSION_ID"] != spec.ID {
		t.Errorf("CORRAL_SESSION_ID = %q, want spec.ID %q", env["CORRAL_SESSION_ID"], spec.ID)
	}
	if env["CORRAL_SOCK"] != "/home/ci/.corral/corral.sock" {
		t.Errorf("CORRAL_SOCK = %q, want the given sock path", env["CORRAL_SOCK"])
	}
}

// TestBuildEnvMissingSnapshotKeysAreOmitted checks that a whitelist name
// simply absent from the snapshot is omitted, not set to "" — BuildEnv
// must never invent a value.
func TestBuildEnvMissingSnapshotKeysAreOmitted(t *testing.T) {
	spec := baseSpec()
	env := BuildEnv(spec, map[string]string{"HOME": "/home/ci"}, nil, "", "/sock", "s")

	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY should be absent, not present with an empty/invented value")
	}
	if env["HOME"] != "/home/ci" {
		t.Errorf("HOME = %q, want /home/ci", env["HOME"])
	}
}

// TestBuildEnvPassthroughExtendsWhitelist checks session.env_passthrough
// names are copied from the same snapshot, and that a name not listed in
// passthrough (and not on the base whitelist) is still never copied.
func TestBuildEnvPassthroughExtendsWhitelist(t *testing.T) {
	spec := baseSpec()
	snapshot := map[string]string{
		"HOME":        "/home/ci",
		"MY_EXTRA":    "extra-value",
		"NOT_ALLOWED": "must-not-appear",
	}
	env := BuildEnv(spec, snapshot, []string{"MY_EXTRA"}, "", "/sock", "s")

	if env["MY_EXTRA"] != "extra-value" {
		t.Errorf("MY_EXTRA = %q, want extra-value (should be copied via passthrough)", env["MY_EXTRA"])
	}
	if _, ok := env["NOT_ALLOWED"]; ok {
		t.Error("NOT_ALLOWED must not be copied: it is neither on the base whitelist nor in passthrough")
	}
}

// TestBuildEnvCustomTerm checks a non-empty term argument overrides the
// xterm-256color default.
func TestBuildEnvCustomTerm(t *testing.T) {
	spec := baseSpec()
	env := BuildEnv(spec, nil, nil, "screen-256color", "/sock", "s")
	if env["TERM"] != "screen-256color" {
		t.Errorf("TERM = %q, want screen-256color", env["TERM"])
	}
}

// TestBuildEnvSessionSecret checks the sessionSecret argument is what ends
// up in CORRAL_SESSION_SECRET (step 5.7).
func TestBuildEnvSessionSecret(t *testing.T) {
	spec := baseSpec()
	env := BuildEnv(spec, map[string]string{"PATH": "/usr/bin"}, nil, "", "/sock", "mysecret")
	if env["CORRAL_SESSION_SECRET"] != "mysecret" {
		t.Errorf("CORRAL_SESSION_SECRET = %q, want mysecret", env["CORRAL_SESSION_SECRET"])
	}
}

// TestBuildEnvDepWorktreesOmittedWhenEmpty is the negative half of §6.2's
// CORRAL_DEP_WORKTREES contract: a spec with no dependency worktrees to
// report (the common case — most tasks have no deps, or no worktree-using
// deps) must not grow an env var at all, not one set to "".
func TestBuildEnvDepWorktreesOmittedWhenEmpty(t *testing.T) {
	spec := baseSpec()
	spec.DepWorktrees = ""
	env := BuildEnv(spec, map[string]string{"PATH": "/usr/bin"}, nil, "", "/sock", "secret")
	if _, ok := env["CORRAL_DEP_WORKTREES"]; ok {
		t.Fatalf("CORRAL_DEP_WORKTREES present = %q, want key absent when Spec.DepWorktrees is empty", env["CORRAL_DEP_WORKTREES"])
	}
}

// TestBuildEnvDepWorktreesCopiedVerbatim is the positive half: a non-empty
// Spec.DepWorktrees must reach the child's environment exactly as given —
// BuildEnv (spawn.go) is the ONLY place CORRAL_DEP_WORKTREES is set, per
// the trust-boundary rule in m4.md §6.2 (never assembled from spec.Env by
// a caller, never sourced from the env snapshot).
func TestBuildEnvDepWorktreesCopiedVerbatim(t *testing.T) {
	spec := baseSpec()
	spec.DepWorktrees = `[{"name":"a","worktree":"/repo/.worktrees/a","branch":"corral/task/a-a1"}]`
	env := BuildEnv(spec, map[string]string{"PATH": "/usr/bin"}, nil, "", "/sock", "secret")
	if env["CORRAL_DEP_WORKTREES"] != spec.DepWorktrees {
		t.Fatalf("CORRAL_DEP_WORKTREES = %q, want %q", env["CORRAL_DEP_WORKTREES"], spec.DepWorktrees)
	}
}

func assertExactKeySet(t *testing.T, env map[string]string, want []string) {
	t.Helper()
	gotKeys := make([]string, 0, len(env))
	for k := range env {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(gotKeys, wantSorted) {
		t.Fatalf("env key set =\n%v\nwant\n%v", gotKeys, wantSorted)
	}
}

// --- ValidateSpec --------------------------------------------------------

func TestValidateSpecRelativeCwdRejected(t *testing.T) {
	spec := baseSpec()
	spec.Cwd = "relative/path"
	if err := ValidateSpec(spec); err == nil {
		t.Fatal("expected an error for a relative Cwd")
	}
}

func TestValidateSpecMissingDirRejected(t *testing.T) {
	spec := baseSpec()
	spec.Cwd = filepath.Join(t.TempDir(), "does-not-exist")
	if err := ValidateSpec(spec); err == nil {
		t.Fatal("expected an error for a missing Cwd directory")
	}
}

func TestValidateSpecCwdIsFileNotDirRejected(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	spec := baseSpec()
	spec.Cwd = filePath
	if err := ValidateSpec(spec); err == nil {
		t.Fatal("expected an error when Cwd is a file, not a directory")
	}
}

func TestValidateSpecBadNameRejected(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		bad  string
	}{
		{"empty", ""},
		{"leading whitespace", " corral-1"},
		{"trailing whitespace", "corral-1 "},
		{"contains slash", "corral/1"},
		{"contains backslash", "corral\\1"},
		{"contains control char", "corral-\x00-1"},
		{"contains newline", "corral-\n-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec()
			spec.Cwd = dir
			spec.Name = tc.bad
			if err := ValidateSpec(spec); err == nil {
				t.Fatalf("expected an error for bad name %q", tc.bad)
			}
		})
	}
}

// TestValidateSpecMissingPathRejected guards the empty-environment trap:
// Spawn never falls back to os.Environ(), so a Spec with no PATH in Env
// would otherwise start a child with no PATH/HOME at all (see
// ValidateSpec's doc comment).
func TestValidateSpecMissingPathRejected(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"nil env", nil},
		{"empty env", map[string]string{}},
		{"PATH present but empty", map[string]string{"PATH": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec()
			spec.Cwd = dir
			spec.Env = tc.env
			if err := ValidateSpec(spec); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

func TestValidateSpecGoodSpecAccepted(t *testing.T) {
	spec := baseSpec()
	spec.Cwd = t.TempDir()
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec(good spec) = %v, want nil", err)
	}
}

// --- Spawn (PTY mechanics smoke test) -----------------------------------

// TestSpawnStartsRealProcessOverPTY is not one of §10.2's named rows
// (which cover only argv/env/validation as pure-function unit tests, with
// no daemon and no real PTY yet) — but Spawn is the one function in this
// package with an actual side effect, and nothing else in step 6 or 7
// exercises it; step 9's supervisor wiring and the integration suite are
// the first things that will. This uses a tiny shebang script instead of
// a fixed-flag binary (BuildArgv's flags are not shell-configurable) as
// spec.ClaudeBin, so the script's body — not its arguments — controls
// timing: it ignores whatever flags BuildArgv appends and just sleeps
// briefly, giving a real, non-flaky (no timers racing against the OS)
// window in which to exercise procinfo against the live child before
// reaping it.
func TestSpawnStartsRealProcessOverPTY(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\nsleep 0.3\nexit 7\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake script: %v", err)
	}

	spec := baseSpec()
	spec.Cwd = dir
	spec.ClaudeBin = scriptPath
	spec.SettingsPath = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(spec.SettingsPath, []byte(`{"hooks": {}}`), 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}
	spec.Env = map[string]string{"PATH": os.Getenv("PATH")}

	master, cmd, info, err := Spawn(spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer master.Close()

	if info.PID <= 0 {
		t.Fatalf("Info.PID = %d, want > 0", info.PID)
	}
	if info.PGID != info.PID {
		t.Fatalf("Info.PGID = %d, want == PID %d (pty.StartWithSize sets Setsid)", info.PGID, info.PID)
	}
	if info.ProcStartNs == 0 {
		t.Fatal("Info.ProcStartNs = 0, want non-zero for a process that just started")
	}
	if info.Rows != spec.Rows || info.Cols != spec.Cols {
		t.Fatalf("Info.{Rows,Cols} = %d,%d, want spec's %d,%d", info.Rows, info.Cols, spec.Rows, spec.Cols)
	}
	if !procinfo.Alive(info.PID, info.ProcStartNs) {
		t.Fatal("procinfo.Alive(child, recorded start time) = false while the child should still be running")
	}

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(interface{ ExitCode() int }); !ok || exitErr.ExitCode() != 7 {
			t.Fatalf("cmd.Wait() = %v, want exit code 7", err)
		}
	} else {
		t.Fatal("cmd.Wait() = nil error, want exit code 7")
	}

	if procinfo.Alive(info.PID, info.ProcStartNs) {
		t.Fatal("procinfo.Alive(reaped child, its former start time) = true, want false")
	}
}

// TestSpawnZeroSizeDefaultsReportedInInfo checks that when spec.Rows/Cols
// is 0, Spawn normalizes to §7.1's 40x120 default and reports the
// *effective* size back via Info — not the zero value the caller passed
// in. A caller that persists spec.Rows/Cols verbatim instead of Info's
// would otherwise record a 0x0 session size while the PTY is really
// 40x120.
func TestSpawnZeroSizeDefaultsReportedInInfo(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 0.1\n"), 0o755); err != nil {
		t.Fatalf("writing fake script: %v", err)
	}

	spec := baseSpec()
	spec.Cwd = dir
	spec.ClaudeBin = scriptPath
	spec.SettingsPath = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(spec.SettingsPath, []byte(`{"hooks": {}}`), 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}
	spec.Env = map[string]string{"PATH": os.Getenv("PATH")}
	spec.Rows, spec.Cols = 0, 0

	master, cmd, info, err := Spawn(spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer master.Close()
	defer cmd.Wait()

	if info.Rows != 40 || info.Cols != 120 {
		t.Fatalf("Info.{Rows,Cols} = %d,%d, want the §7.1 default 40,120", info.Rows, info.Cols)
	}
}
