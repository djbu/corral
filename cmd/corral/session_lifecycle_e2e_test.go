package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/api/client"
)

// buildFakeClaudeE2E builds test/fakeclaude once per test into dir/fakeclaude,
// standing in for a real claude binary the same way daemon_e2e_test.go builds
// the real corral binary — no sync.Once cache here since this is the only
// e2e test in this package that needs it.
func buildFakeClaudeE2E(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "fakeclaude-e2e")
	build := exec.Command("go", "build", "-o", bin, "github.com/danielbecerra/corral/test/fakeclaude")
	build.Dir = mustGetwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build fakeclaude: %v\n%s", err, out)
	}
	return bin
}

// TestSessionLifecycleLsNewKill drives the real corral CLI end to end
// against a real (detached, setsid-re-exec'd) daemon: `corral new` creates a
// session over the real fakeclaude binary, `corral ls` observes it running,
// and `corral kill` stops it — exercising the full
// cmd_new/cmd_ls/cmd_kill -> client SDK -> handlers_sessions.go -> supervisor
// path in one test, per step 9's BUILD order item 6.
func TestSessionLifecycleLsNewKill(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries and spawns a real daemon; skipped in -short")
	}

	binDir := t.TempDir()
	corralBin := filepath.Join(binDir, "corral-e2e")
	build := exec.Command("go", "build", "-o", corralBin, ".")
	build.Dir = mustGetwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build corral: %v\n%s", err, out)
	}
	fakeClaudeBin := buildFakeClaudeE2E(t, binDir)

	// AF_UNIX sun_path limit rules out t.TempDir() for the socket path (see
	// daemon_e2e_test.go's identical comment); state_dir and the session
	// cwd have no such constraint.
	sockDir, err := os.MkdirTemp("", "corral-lc-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "corral.sock")
	stateDir := t.TempDir()
	sessionCwd := t.TempDir()

	daemonCmd := exec.Command(corralBin, "daemon")
	daemonCmd.Env = append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+sockPath,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
		"CORRAL_SESSION_CLAUDE_BIN="+fakeClaudeBin,
	)
	var daemonStdout, daemonStderr bytes.Buffer
	daemonCmd.Stdout = &daemonStdout
	daemonCmd.Stderr = &daemonStderr
	if err := daemonCmd.Run(); err != nil {
		t.Fatalf("corral daemon: %v (stderr=%s)", err, daemonStderr.String())
	}
	m := regexp.MustCompile(`daemon started pid=(\d+)`).FindStringSubmatch(daemonStdout.String())
	if m == nil {
		t.Fatalf("daemon stdout = %q, want to match \"daemon started pid=...\"", daemonStdout.String())
	}

	c := client.New(sockPath, os.Stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := waitForVersion(ctx, t, c); err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), 0)
		waitForGone(t, sockPath, 5*time.Second)
	})

	// cmd_new/cmd_ls/cmd_kill all resolve the daemon socket via
	// config.LoadDaemon() -> CORRAL_DAEMON_SOCKET, same env var the daemon
	// subprocess above was launched with — t.Setenv scopes it to this test
	// and restores it on cleanup.
	t.Setenv("CORRAL_DAEMON_SOCKET", sockPath)
	t.Setenv("CORRAL_DAEMON_STATE_DIR", stateDir)

	const sessionName = "lifecycle-1"
	var newOut, newErr bytes.Buffer
	code := run([]string{"new", "--cwd", sessionCwd, "--name", sessionName, "--no-attach"}, &newOut, &newErr)
	if code != exitOK {
		t.Fatalf("corral new: code=%d stderr=%q", code, newErr.String())
	}
	if !strings.HasPrefix(newOut.String(), "created "+sessionName+" (") {
		t.Fatalf("corral new stdout = %q, want prefix %q", newOut.String(), "created "+sessionName+" (")
	}

	// Poll ls --json until the session shows up running. --json is used
	// (not the tabwriter-rendered default) because tabwriter pads with
	// spaces, not tabs (padchar ' '), so a substring check for
	// "name\trunning\t" against the human-readable table is vacuous — it
	// would never match, pass or fail, regardless of actual status. Spawn
	// happens synchronously inside the POST handler, but the PTY
	// reader/screen goroutines and the store's post-spawn UpdateSession are
	// not guaranteed to have landed the instant `new` returns.
	deadline := time.Now().Add(5 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if code := run([]string{"ls", "--json"}, &out, &errBuf); code != exitOK {
			t.Fatalf("corral ls --json: code=%d stderr=%q", code, errBuf.String())
		}
		var sessions []client.SessionInfo
		if err := json.Unmarshal(out.Bytes(), &sessions); err != nil {
			t.Fatalf("unmarshaling corral ls --json output %q: %v", out.String(), err)
		}
		for _, s := range sessions {
			if s.Name == sessionName && s.Status == "running" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatalf("corral ls --json never showed %q with status running within 5s", sessionName)
	}

	var killOut, killErr bytes.Buffer
	code = run([]string{"kill", sessionName}, &killOut, &killErr)
	if code != exitOK {
		t.Fatalf("corral kill: code=%d stderr=%q", code, killErr.String())
	}
	if !strings.HasPrefix(killOut.String(), "killed "+sessionName+" ") {
		t.Fatalf("corral kill stdout = %q, want prefix %q", killOut.String(), "killed "+sessionName+" ")
	}

	// After Kill returns, the session must show a terminal status (never
	// still "running") — Kill's checkpointer call is synchronous within
	// the DELETE handler per supervisor.Registry.Kill's contract.
	var out, errBuf bytes.Buffer
	if code := run([]string{"ls", "--json"}, &out, &errBuf); code != exitOK {
		t.Fatalf("corral ls --json (post-kill): code=%d stderr=%q", code, errBuf.String())
	}
	var postKill []client.SessionInfo
	if err := json.Unmarshal(out.Bytes(), &postKill); err != nil {
		t.Fatalf("unmarshaling corral ls --json (post-kill) output %q: %v", out.String(), err)
	}
	for _, s := range postKill {
		if s.Name == sessionName && s.Status == "running" {
			t.Fatalf("corral ls --json (post-kill) shows %q still running", sessionName)
		}
	}
}
