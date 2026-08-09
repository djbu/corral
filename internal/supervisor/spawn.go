// Package supervisor owns the process lifecycle of a spawned claude: argv
// assembly, environment construction, and starting it under a PTY (design
// doc §7). Everything in this file is a pure function of its inputs except
// Spawn itself, which does the one unavoidable side effect (forking a
// child process) — argv/env construction never reads os.Environ() or the
// filesystem, precisely so they can be unit-tested without a daemon (§11
// step 6: "No daemon yet").
package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/creack/pty"

	"github.com/danielbecerra/corral/internal/procinfo"
	"github.com/danielbecerra/corral/internal/session"
)

// envWhitelist is the fixed set of environment variable NAMES corral
// copies from the frozen daemon-start snapshot, if present (design doc
// §7.2). This is a strict allowlist by construction: BuildEnv never
// reads any name not on this list (or added via passthrough), so no
// separate denylist is needed — in particular, an ambient CLAUDE_* or
// CORRAL_SESSION_SECRET (reserved for M2, never set in M1) in the
// snapshot is never copied, because it is never on this list.
var envWhitelist = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TMPDIR",
	"LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSH_AUTH_SOCK",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
}

// BuildArgv returns the exact argv corral execs claude with (design doc
// §7.3/§5.1), as a full command line including the binary itself as argv[0]:
//
//	<claude_bin>
//	  --session-id <ID>            (fresh spawn)   |   --resume <ResumeFrom>   (restore)
//	  --settings <SettingsPath>
//	  --setting-sources <SettingSources>
//	  --name <Name>
//	  [--model <Model>]            (omitted when empty)
//	  -p "<Prompt>"                (Mode==ModeHeadless only)
//	  --output-format stream-json  (Mode==ModeHeadless only)
//	  --verbose                    (Mode==ModeHeadless only — stream-json requires it for full records)
//	  [--permission-mode <PermissionMode>]  (all modes, omitted when empty)
//
// No --input-format, ever (§5.1): `-p` is a one-shot invocation with no live
// child to inject a follow-up frame into; re-send (§9) is a fresh --resume
// -p invocation, not frame injection.
//
// redact controls only the -p value, and only for a headless spec: the
// caller that execs the child (spawn.go's Spawn/SpawnHeadless) must pass
// false so the real prompt reaches argv[0]'s process; the caller that
// persists argv to the sessions.argv column (Registry.Spawn) must pass true
// so the stored copy carries a byte-count placeholder instead — not because
// the prompt is secret (it is corral-owned cleartext, task.prompt), but to
// avoid landing an arbitrarily large prompt in that column (§5.1, "decided").
// Interactive specs never carry a prompt, so redact is a no-op there.
func BuildArgv(spec session.Spec, redact bool) []string {
	argv := []string{spec.ClaudeBin}
	if spec.ResumeFrom != "" {
		argv = append(argv, "--resume", spec.ResumeFrom)
	} else {
		argv = append(argv, "--session-id", spec.ID)
	}
	argv = append(argv, "--settings", spec.SettingsPath)
	argv = append(argv, "--setting-sources", spec.SettingSources)
	argv = append(argv, "--name", spec.Name)
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	if spec.Mode == session.ModeHeadless {
		prompt := spec.Prompt
		if redact {
			prompt = fmt.Sprintf("<prompt:%d bytes>", len(spec.Prompt))
		}
		argv = append(argv, "-p", prompt)
		argv = append(argv, "--output-format", "stream-json")
		argv = append(argv, "--verbose")
	}
	if spec.PermissionMode != "" {
		argv = append(argv, "--permission-mode", spec.PermissionMode)
	}

	return argv
}

// BuildEnv constructs the complete child environment for spec (design doc
// §7.2): "nothing is inherited unless explicitly named". snapshot is the
// daemon-start environment frozen once at daemon startup (never
// os.Environ() read at spawn time — see the package doc comment: reading
// live process environment here would make spawn behavior depend on
// ambient state instead of being a pure function of config + snapshot,
// which is what finding #3, CLAUDE_CODE_CHILD_SESSION leaking into a
// child unexpectedly, says must never happen again). passthrough is the
// resolved session.env_passthrough config list: additional names the
// user opted in, resolved from the same snapshot. term/sockPath are
// corral's own values to set unconditionally.
//
// The returned map is always exactly: the whitelisted+passthrough names
// present in snapshot, plus TERM, COLORTERM, PWD, CORRAL_SESSION_ID,
// CORRAL_SOCK, CORRAL_SESSION_SECRET, and (only when spec.DepWorktrees is
// non-empty) CORRAL_DEP_WORKTREES — nothing else, regardless of what
// snapshot itself contains. sessionSecret is the per-spawn hook-auth secret
// (Amendment: CORRAL_SESSION_SECRET) — generated fresh per spawn by the
// caller (Registry.Spawn), never read from snapshot: envWhitelist
// deliberately excludes it so it can never be inherited from the daemon's
// own ambient environment. CORRAL_DEP_WORKTREES (design doc §6.2) is
// likewise sourced only from spec.DepWorktrees, never from snapshot — it is
// a corral-owned value the orchestrator computes per attempt, not something
// any ambient process environment could plausibly supply, so honoring it
// from snapshot would reopen exactly the kind of unintended-inheritance
// leak this function's invariant exists to prevent (finding #3).
func BuildEnv(spec session.Spec, snapshot map[string]string, passthrough []string, term, sockPath, sessionSecret string) map[string]string {
	env := make(map[string]string, len(envWhitelist)+len(passthrough)+6)

	copyIfPresent := func(name string) {
		if v, ok := snapshot[name]; ok {
			env[name] = v
		}
	}
	for _, name := range envWhitelist {
		copyIfPresent(name)
	}
	for _, name := range passthrough {
		copyIfPresent(name)
	}

	if term == "" {
		term = "xterm-256color"
	}
	env["TERM"] = term
	env["COLORTERM"] = "truecolor"
	env["PWD"] = spec.Cwd
	env["CORRAL_SESSION_ID"] = spec.ID
	env["CORRAL_SOCK"] = sockPath
	env["CORRAL_SESSION_SECRET"] = sessionSecret
	if spec.DepWorktrees != "" {
		env["CORRAL_DEP_WORKTREES"] = spec.DepWorktrees
	}

	return env
}

// envMapToSlice renders a Spec.Env map into the []string "KEY=VALUE" form
// exec.Cmd.Env requires. Key order is sorted purely for deterministic,
// diffable argv/env logs — os/exec does not care about env slice order.
func envMapToSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// ValidateSpec checks the structural invariants a Spec must satisfy
// before Spawn is called. Design doc §7.1/§10.2 name "relative cwd,
// missing dir, bad name" as the cases to test but do not define "bad
// name" anywhere in §7.1 or §8.4; this implements a conservative,
// explicit rule — documented here rather than left implicit, per the
// deviations list — and is deliberately permissive beyond it (it does not
// attempt to guess every way a name could be unwise, only the ways that
// would make BuildArgv/exec unsafe or nonsensical):
//
//   - Cwd must be an absolute path to an existing directory.
//   - Name must be non-empty, contain no ASCII control character
//     (0x00-0x1F, 0x7F) and no '/' or '\' (Name is never used to build a
//     filesystem path in M1 — SettingsPath is keyed by ID, not Name — but
//     a name containing a path separator would be confusing in `corral
//     ls` output and in the --name argv value for no benefit), and have
//     no leading or trailing whitespace.
//   - Env must contain a non-empty PATH. This is not named explicitly in
//     §7.1/§10.2, but BuildEnv/Spawn never fall back to os.Environ() by
//     design (finding #3) — a caller that forgets to populate spec.Env
//     would otherwise get exec.Cmd.Env == []string{}, i.e. a child claude
//     with no PATH/HOME at all, which fails confusingly deep inside the
//     child rather than obviously here.
func ValidateSpec(spec session.Spec) error {
	if !strings.HasPrefix(spec.Cwd, "/") {
		return fmt.Errorf("supervisor: spec.Cwd %q must be an absolute path", spec.Cwd)
	}
	info, err := os.Stat(spec.Cwd)
	if err != nil {
		return fmt.Errorf("supervisor: spec.Cwd %q: %w", spec.Cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("supervisor: spec.Cwd %q is not a directory", spec.Cwd)
	}

	if err := validateName(spec.Name); err != nil {
		return err
	}

	if p, ok := spec.Env["PATH"]; !ok || p == "" {
		return fmt.Errorf("supervisor: spec.Env must contain a non-empty PATH")
	}

	return nil
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("supervisor: spec.Name must not be empty")
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("supervisor: spec.Name %q has leading/trailing whitespace", name)
	}
	for _, r := range name {
		if r == '/' || r == '\\' {
			return fmt.Errorf("supervisor: spec.Name %q must not contain a path separator", name)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("supervisor: spec.Name %q contains a control character", name)
		}
	}
	return nil
}

// Info is what Spawn observes immediately after starting the child
// (design doc §7.3): "record pid, pgid (== pid, since the child is a
// session leader), and proc_start_ns from procinfo.StartTime(pid)
// immediately".
// Rows/Cols are the size actually used to start the PTY — spec.Rows/Cols
// as given, or the §7.1 default (40x120) when either was 0. Callers must
// persist these, not spec.Rows/Cols, as a session's recorded size: a
// zero-valued spec size is normalized inside Spawn, and step 9's resize
// tracking (design doc's later TestResizePropagates) needs the value that
// actually matches the PTY, not the possibly-zero input.
type Info struct {
	PID         int
	PGID        int
	ProcStartNs int64
	Rows        uint16
	Cols        uint16
}

// Spawn validates spec, then starts it under a new PTY at spec.Rows x
// spec.Cols via pty.StartWithSize, which — per its own doc comment and
// confirmed by reading creack/pty's start.go for the pinned version —
// already sets Setsid:true and Setctty:true on the child, making it a new
// session leader and its own process group leader. That is why PGID is
// always recorded as equal to PID here: there is no separate getpgid
// call, because creack/pty's StartWithSize guarantees the relationship by
// construction, not by coincidence.
//
// Returns the PTY master (for the caller's reader-goroutine ->
// screen.Feed loop and reply-writing per screen.Screen.Replies), the
// started *exec.Cmd (so the caller can call cmd.Wait() to reap it), and
// the observed Info triple. On error, no process is left running: a
// failure from pty.StartWithSize itself means nothing was started.
func Spawn(spec session.Spec) (master *os.File, cmd *exec.Cmd, info Info, err error) {
	if err := ValidateSpec(spec); err != nil {
		return nil, nil, Info{}, err
	}

	argv := BuildArgv(spec, false)
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = spec.Cwd
	c.Env = envMapToSlice(spec.Env)

	rows, cols := spec.Rows, spec.Cols
	if rows == 0 || cols == 0 {
		// Design doc §7.1: "default 40x120 when nothing is attached".
		rows, cols = 40, 120
	}

	m, err := pty.StartWithSize(c, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, nil, Info{}, fmt.Errorf("supervisor: spawn %s: %w", spec.ClaudeBin, err)
	}

	pid := c.Process.Pid
	startNs, err := procinfo.StartTime(pid)
	if err != nil {
		// The process did start (pty.StartWithSize succeeded); a
		// StartTime failure immediately after is not itself a spawn
		// failure — surface it via a zero ProcStartNs rather than
		// tearing down a live child over a bookkeeping read.
		startNs = 0
	}

	return m, c, Info{PID: pid, PGID: pid, ProcStartNs: startNs, Rows: rows, Cols: cols}, nil
}
