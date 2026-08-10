package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/djbu/corral/internal/api/client"
	claudesessions "github.com/djbu/corral/internal/claude/sessions"
	"github.com/djbu/corral/internal/daemon"
)

// TestDaemonE2E_SetsidReExec builds the real corral binary and exercises
// `corral daemon`'s full two-hop setsid re-exec path end to end (design
// doc §3.1): the launcher re-execs itself as the hidden "daemon-run"
// subcommand, detached via Setsid, and blocks on the fd-3 ready pipe
// before printing "daemon started pid=...". This is the one test in the
// whole step-8 suite that never calls daemon.Main/run directly — every
// other test drives the daemon body in-process.
func TestDaemonE2E_SetsidReExec(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real binary; skipped in -short")
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "corral-e2e")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = mustGetwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// AF_UNIX's sun_path limit (~104 bytes on darwin) rules out
	// t.TempDir()'s test-name-qualified paths for the socket; state_dir
	// has no such constraint. See internal/daemon's shortTempDir for the
	// same issue hit there.
	sockDir, err := os.MkdirTemp("", "corral-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "corral.sock")
	stateDir := t.TempDir()

	cmd := exec.Command(binPath, "daemon")
	cmd.Env = append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+sockPath,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("corral daemon: %v (stderr=%s)", err, stderr.String())
	}

	line := stdout.String()
	m := regexp.MustCompile(`daemon started pid=(\d+) sock=(\S+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("stdout = %q, want to match \"daemon started pid=... sock=...\"", line)
	}
	daemonPID := m[1]
	t.Logf("launcher reported daemon pid=%s", daemonPID)

	logPath := filepath.Join(stateDir, "daemon.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("daemon.log missing: %v", err)
	}

	c := client.New(sockPath, os.Stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := waitForVersion(ctx, t, c)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.PID == 0 {
		t.Fatalf("version pid = 0, want the detached daemon's real pid")
	}

	if err := c.Shutdown(context.Background(), 0); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !waitForGone(t, sockPath, 5*time.Second) {
		t.Fatalf("socket %s still exists after shutdown", sockPath)
	}
	if !waitForGone(t, filepath.Join(stateDir, "daemon.pid"), 5*time.Second) {
		t.Fatalf("daemon.pid still exists after shutdown")
	}
}

func TestDaemonLifecycleE2E_IdempotentStartStopRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and drives a real daemon binary")
	}

	binPath := buildCorralE2E(t)
	sockDir, err := os.MkdirTemp("", "corral-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	stateDir := t.TempDir()
	socket := filepath.Join(sockDir, "corral.sock")
	env := append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+socket,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
		"CORRAL_DAEMON_SHUTDOWN_GRACE=1s",
	)
	t.Cleanup(func() {
		cmd := exec.Command(binPath, "daemon", "stop", "--timeout", "5s")
		cmd.Env = env
		_ = cmd.Run()
	})

	before := lifecycleStatusE2E(t, binPath, env)
	if before.State != daemon.LifecycleStopped {
		t.Fatalf("before start state = %q, want stopped", before.State)
	}

	startOut := runCorralE2E(t, binPath, env, "daemon", "start")
	pid1 := parseLifecyclePID(t, startOut, `daemon started pid=(\d+)`)

	alreadyOut := runCorralE2E(t, binPath, env, "daemon", "start")
	if got := parseLifecyclePID(t, alreadyOut, `daemon already running pid=(\d+)`); got != pid1 {
		t.Fatalf("idempotent start pid = %d, want %d", got, pid1)
	}

	running := lifecycleStatusE2E(t, binPath, env)
	if running.State != daemon.LifecycleRunning || running.PID != pid1 {
		t.Fatalf("running status = %+v, want pid %d", running, pid1)
	}

	restartOut := runCorralE2E(t, binPath, env, "daemon", "restart", "--timeout", "10s")
	m := regexp.MustCompile(`daemon restarted old_pid=(\d+) new_pid=(\d+)`).FindStringSubmatch(restartOut)
	if m == nil {
		t.Fatalf("restart output = %q", restartOut)
	}
	pid2 := parseLifecyclePID(t, m[2], `(\d+)`)
	if pid2 == pid1 {
		t.Fatalf("restart preserved pid %d", pid1)
	}
	if got := lifecycleStatusE2E(t, binPath, env); got.State != daemon.LifecycleRunning || got.PID != pid2 {
		t.Fatalf("after restart = %+v, want running pid %d", got, pid2)
	}

	stopOut := runCorralE2E(t, binPath, env, "daemon", "stop", "--timeout", "10s")
	if !regexp.MustCompile(`daemon stopped pid=`).MatchString(stopOut) {
		t.Fatalf("stop output = %q", stopOut)
	}
	secondStop := runCorralE2E(t, binPath, env, "daemon", "stop")
	if secondStop != "daemon already stopped state=stopped\n" {
		t.Fatalf("second stop = %q", secondStop)
	}
	if got := lifecycleStatusE2E(t, binPath, env); got.State != daemon.LifecycleStopped {
		t.Fatalf("after stop = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "daemon.state")); !os.IsNotExist(err) {
		t.Fatalf("daemon.state remains after shutdown: %v", err)
	}
}

func TestDaemonLifecycleE2E_StalePIDAndHeldLockFailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real binary")
	}
	binPath := buildCorralE2E(t)
	stateDir := t.TempDir()
	socket := filepath.Join(t.TempDir(), "corral.sock")
	env := append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+socket,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
	)

	if err := os.WriteFile(filepath.Join(stateDir, "daemon.pid"), []byte("424242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleStatusE2E(t, binPath, env); got.State != daemon.LifecycleStalePID || got.PID != 424242 {
		t.Fatalf("stale pid status = %+v", got)
	}
	if got := runCorralE2E(t, binPath, env, "daemon", "stop"); got != "daemon already stopped state=stale-pid\n" {
		t.Fatalf("stale stop = %q", got)
	}

	lock, err := daemon.AcquireLock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.ReleaseLock(lock)
	if err := os.WriteFile(filepath.Join(stateDir, "daemon.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	status := lifecycleStatusE2E(t, binPath, env)
	if status.State != daemon.LifecycleLockHeld || status.PID != os.Getpid() {
		t.Fatalf("held lock status = %+v", status)
	}
	cmd := exec.Command(binPath, "daemon", "stop", "--timeout", "200ms")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(out, []byte("refusing to signal pid")) {
		t.Fatalf("held-lock stop err=%v output=%q", err, out)
	}
}

func TestDaemonLifecycleE2E_GracefulAndCrashRestartRecoverSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries and exercises daemon recovery")
	}
	binDir := t.TempDir()
	corralBin := filepath.Join(binDir, "corral-e2e")
	build := exec.Command("go", "build", "-o", corralBin, ".")
	build.Dir = mustGetwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build corral: %v\n%s", err, out)
	}
	fakeClaudeBin := buildFakeClaudeE2E(t, binDir)

	sockDir, err := os.MkdirTemp("", "corral-m7c-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	stateDir := t.TempDir()
	fakeHome := t.TempDir()
	fakeState := t.TempDir()
	cwd := t.TempDir()
	socket := filepath.Join(sockDir, "corral.sock")
	env := append(os.Environ(),
		"HOME="+fakeHome,
		"CORRAL_DAEMON_SOCKET="+socket,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
		"CORRAL_DAEMON_SHUTDOWN_GRACE=200ms",
		"CORRAL_SESSION_CLAUDE_BIN="+fakeClaudeBin,
		"CORRAL_SESSION_ENV_PASSTHROUGH=CORRAL_FAKE_HOME,CORRAL_FAKE_STATE",
		"CORRAL_FAKE_HOME="+fakeHome,
		"CORRAL_FAKE_STATE="+fakeState,
	)
	t.Cleanup(func() {
		cmd := exec.Command(corralBin, "daemon", "stop", "--timeout", "5s", "--grace", "100ms")
		cmd.Env = env
		_ = cmd.Run()
	})

	runCorralE2E(t, corralBin, env, "daemon", "start")
	newOut := runCorralE2E(t, corralBin, env, "new", "--cwd", cwd, "--name", "recover-m7c", "--no-attach")
	m := regexp.MustCompile(`created recover-m7c \(([^)]+)\)`).FindStringSubmatch(newOut)
	if m == nil {
		t.Fatalf("new output=%q", newOut)
	}
	sessionID := m[1]
	c := client.New(socket, os.Stderr)
	waitSessionRunningE2E(t, c, sessionID, 0)

	// A real Claude transcript makes this interactive session resumable. The
	// fake only needs its own minimal JSONL shape to prove invocation 2/3 use
	// --resume and reload durable history.
	transcript := claudesessions.TranscriptPath(filepath.Join(fakeHome, ".claude"), cwd, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{\"text\":\"survives restart\",\"at\":\"2026-08-10T00:00:00Z\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runCorralE2E(t, corralBin, env, "daemon", "restart", "--timeout", "10s", "--grace", "200ms")
	waitSessionRunningE2E(t, c, sessionID, 1)

	// Simulate the service manager's Restart=on-failure/KeepAlive path. The
	// PID comes from the authenticated local API, never from daemon.pid.
	crashed := lifecycleStatusE2E(t, corralBin, env)
	if crashed.PID <= 1 {
		t.Fatalf("unsafe daemon pid %d", crashed.PID)
	}
	if err := syscall.Kill(crashed.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("crashing daemon pid %d: %v", crashed.PID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := lifecycleStatusE2E(t, corralBin, env)
		if status.State != daemon.LifecycleRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	runCorralE2E(t, corralBin, env, "daemon", "start", "--timeout", "10s")
	waitSessionRunningE2E(t, c, sessionID, 2)
	// Do not mistake Registry.Spawn's short starting->running transition for
	// durable recovery: the replacement must still be running after the fake
	// process has had ample time to initialize.
	time.Sleep(500 * time.Millisecond)
	stable, err := c.GetSession(context.Background(), sessionID)
	if err != nil || stable.Status != "running" || stable.ResumeCount != 2 || stable.PID <= 1 {
		t.Fatalf("post-crash recovery is not stable: session=%+v err=%v", stable, err)
	}

	// The graceful recovery and crash recovery share Restore/resumeSpawn; the
	// former's diagnostic record proves that path uses --resume and never a
	// fresh --session-id. resume_count=2 above proves the crash traversed it a
	// second time without depending on a child-side diagnostic write race.
	invocation2 := filepath.Join(fakeState, sessionID, "invocation-2.json")
	data, err := os.ReadFile(invocation2)
	if err != nil {
		t.Fatalf("reading resumed invocation: %v", err)
	}
	if !bytes.Contains(data, []byte(`"--resume"`)) || bytes.Contains(data, []byte(`"--session-id"`)) {
		t.Fatalf("resumed invocation did not use safe resume: %s", data)
	}

	runCorralE2E(t, corralBin, env, "daemon", "stop", "--timeout", "10s", "--grace", "200ms")
}

func waitSessionRunningE2E(t *testing.T, c *client.Client, id string, resumeCount int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last client.SessionInfo
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = c.GetSession(context.Background(), id)
		if lastErr == nil && last.Status == "running" && last.ResumeCount == resumeCount {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s did not become running with resume_count=%d: last=%+v err=%v", id, resumeCount, last, lastErr)
}

func buildCorralE2E(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "corral-e2e")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = mustGetwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binPath
}

func runCorralE2E(t *testing.T, binPath string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("corral %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func lifecycleStatusE2E(t *testing.T, binPath string, env []string) daemon.LifecycleStatus {
	t.Helper()
	out := runCorralE2E(t, binPath, env, "daemon", "status", "--json")
	var status daemon.LifecycleStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("decode status %q: %v", out, err)
	}
	return status
}

func parseLifecyclePID(t *testing.T, output, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("output %q does not match %q", output, pattern)
	}
	var pid int
	if _, err := fmt.Sscanf(m[1], "%d", &pid); err != nil {
		t.Fatalf("parse pid %q: %v", m[1], err)
	}
	return pid
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return wd
}

// waitForVersion polls GET /v1/version until the daemon accepts
// connections or ctx expires — the socket file can exist slightly before
// the HTTP server is actually Serve()-ing on it.
func waitForVersion(ctx context.Context, t *testing.T, c *client.Client) (client.VersionInfo, error) {
	t.Helper()
	var lastErr error
	for {
		v, err := c.Version(ctx)
		if err == nil {
			return v, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return client.VersionInfo{}, lastErr
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitForGone polls until path no longer exists or timeout elapses,
// returning whether it's gone.
func waitForGone(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}
