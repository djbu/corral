package supervisor

import (
	"fmt"
	"io"
	"os/exec"
	"syscall"

	"github.com/djbu/corral/internal/procinfo"
	"github.com/djbu/corral/internal/session"
)

// SpawnHeadless is Spawn's fork for Mode==ModeHeadless (design doc §5.2):
// a one-shot `claude -p --output-format stream-json` invocation captured
// via plain os/exec pipes, never a PTY. It mirrors Spawn's prologue
// (ValidateSpec, BuildArgv, exec.Command, Dir, Env) but sets
// syscall.SysProcAttr{Setsid: true} directly — the pipe-path's equivalent
// of what pty.StartWithSize already guarantees for the PTY path — so
// PGID==PID and the existing SIGTERM-to-process-group discipline (Kill,
// checkpoint escalation, recovery's orphan reap) apply identically
// regardless of which fork spawned the child.
//
// Callers MUST fully drain stdout and stderr to EOF before calling
// cmd.Wait(): per exec.Cmd's own doc comment, Wait closes the pipes once
// the child exits, and calling it while a reader is still in flight races
// that close and can silently drop the last bytes written — including the
// terminal `result` line stream-json ends on (§5.2). SpawnHeadless itself
// does not read anything; that ordering is runHeadlessGoroutines's job
// (supervisor.go).
func SpawnHeadless(spec session.Spec) (stdout io.ReadCloser, stderr io.ReadCloser, cmd *exec.Cmd, info Info, err error) {
	if err := ValidateSpec(spec); err != nil {
		return nil, nil, nil, Info{}, err
	}

	argv := BuildArgv(spec, false)
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = spec.Cwd
	c.Env = envMapToSlice(spec.Env)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	stdout, err = c.StdoutPipe()
	if err != nil {
		return nil, nil, nil, Info{}, fmt.Errorf("supervisor: headless stdout pipe for %s: %w", spec.ClaudeBin, err)
	}
	stderr, err = c.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, Info{}, fmt.Errorf("supervisor: headless stderr pipe for %s: %w", spec.ClaudeBin, err)
	}

	if err := c.Start(); err != nil {
		return nil, nil, nil, Info{}, fmt.Errorf("supervisor: spawn headless %s: %w", spec.ClaudeBin, err)
	}

	pid := c.Process.Pid
	startNs, err := procinfo.StartTime(pid)
	if err != nil {
		// Same call as Spawn's own StartTime read: the process did start
		// (c.Start succeeded); a bookkeeping-read failure right after is
		// not itself a spawn failure.
		startNs = 0
	}

	return stdout, stderr, c, Info{PID: pid, PGID: pid, ProcStartNs: startNs, Rows: spec.Rows, Cols: spec.Cols}, nil
}
