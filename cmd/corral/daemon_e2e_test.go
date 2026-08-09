package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/djbu/corral/internal/api/client"
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
