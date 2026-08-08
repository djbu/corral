package daemon

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
)

// shortTempDir returns a freshly created directory with a short absolute
// path, cleaned up at test end. t.TempDir() nests under a path that
// includes the full test name (e.g.
// ".../TestForeground_VersionAndShutdown990946347/001"), which routinely
// blows past AF_UNIX's ~104-byte sun_path limit once "/corral.sock" is
// appended — bind(2) then fails with EINVAL ("invalid argument"), not
// something more obviously length-related. Rooting under os.TempDir()
// directly with a short random suffix keeps the socket path well under
// that limit regardless of the test's own name length.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "corral-t-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testConfig builds a config.Daemon pointed entirely at temp directories,
// so run() never touches the real user's state_dir/socket. Log level
// "error" keeps test output quiet; ShutdownGrace is short since nothing is
// ever actually alive to wait out in step 8 (noLiveSessions{}). The
// socket lives under shortTempDir rather than t.TempDir() to stay inside
// AF_UNIX's sun_path length limit (see shortTempDir's doc); StateDir can
// safely use t.TempDir() since nothing there is length-constrained.
func testConfig(t *testing.T) config.Daemon {
	t.Helper()
	return config.Daemon{
		Socket:        filepath.Join(shortTempDir(t), "corral.sock"),
		StateDir:      t.TempDir(),
		LogLevel:      "error",
		LogFormat:     "text",
		ShutdownGrace: 200 * time.Millisecond,
	}
}

// waitForReadyLine reads exactly one line from r, or fails the test after
// timeout — the same handshake cmd_daemon.go's real launcher performs, but
// local to this package so daemon package tests need no dependency on
// cmd/corral.
func waitForReadyLine(t *testing.T, r io.Reader, timeout time.Duration) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			ch <- scanner.Text()
			return
		}
		ch <- ""
	}()
	select {
	case line := <-ch:
		return line
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for ready line", timeout)
		return ""
	}
}

// startForTest runs the daemon body against cfg in a background goroutine
// and blocks until it reports ready (or fails startup). It returns the
// client SDK to talk to it and a stop func that shuts it down and blocks
// until run() actually returns, via channel synchronization rather than
// any bare time.Sleep.
func startForTest(t *testing.T, cfg config.Daemon) (*client.Client, func()) {
	t.Helper()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	runErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		runErr <- run(ctx, cfg, readyW)
	}()

	line := waitForReadyLine(t, readyR, 5*time.Second)
	readyR.Close()
	if line != "OK" {
		cancel()
		t.Fatalf("daemon did not report ready: %q", line)
	}

	c := client.New(cfg.Socket, io.Discard)

	stop := func() {
		defer cancel()
		if err := c.Shutdown(context.Background(), 0); err != nil {
			t.Logf("Shutdown request: %v", err)
		}
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("run() returned error after shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run() did not return within 5s of shutdown")
		}
	}
	return c, stop
}

// TestForeground_VersionAndShutdown drives the same code path
// `corral daemon --foreground` and the setsid-relaunched daemon-run body
// both go through (run, not Main, so no env-var config plumbing is
// needed): start it, hit GET /v1/version over the real unix socket via the
// SDK, POST /v1/daemon/shutdown, and confirm a clean exit.
func TestForeground_VersionAndShutdown(t *testing.T) {
	cfg := testConfig(t)
	c, stop := startForTest(t, cfg)

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.PID != os.Getpid() {
		t.Fatalf("version pid = %d, want this test process's pid %d (daemon runs in-process here)", v.PID, os.Getpid())
	}

	stop()

	if _, err := os.Stat(cfg.Socket); !os.IsNotExist(err) {
		t.Fatalf("socket %s still exists after shutdown (err=%v)", cfg.Socket, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "daemon.pid")); !os.IsNotExist(err) {
		t.Fatalf("daemon.pid still exists after shutdown (err=%v)", err)
	}
}

// TestForeground_SecondDaemonRefused exercises the flock end to end
// through startup: a second run() against the same state_dir while the
// first is still up must fail fast rather than corrupting the first
// daemon's socket or store.
func TestForeground_SecondDaemonRefused(t *testing.T) {
	cfg := testConfig(t)
	_, stop := startForTest(t, cfg)
	defer stop()

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer readyR.Close()

	err = run(context.Background(), cfg, readyW)
	if err == nil {
		t.Fatalf("second run() against the same state_dir: want error, got nil")
	}

	line := waitForReadyLine(t, readyR, 5*time.Second)
	if line == "OK" || line == "" {
		t.Fatalf("second daemon's ready line = %q, want an ERR: line", line)
	}
}
