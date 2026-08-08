package contract

// Shared real-daemon-subprocess harness for the corral contract suite
// (design doc §10, Amendment A.7). Both the fake driver (default `go test`)
// and the real driver (CORRAL_CONTRACT=1) drive a byte-identical `corral
// daemon` subprocess; the ONLY difference between them is which binary the
// daemon spawns as "claude" (CORRAL_SESSION_CLAUDE_BIN) and the scenario/
// prompt that drives it. Because both drivers read their observations back
// out of the daemon's own events table via the client SDK, the assertion
// code physically cannot special-case the fake.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/api/client"
)

// builtBins caches the corral + fakeclaude binaries built once per `go test`
// process; every case reuses them, so the two `go build` invocations are
// amortized across the whole table (advisor guidance; mirrors
// buildFakeClaudeForAnswerE2E).
type builtBins struct {
	corral     string
	fakeclaude string
}

var (
	buildOnce sync.Once
	bins      builtBins
	buildErr  error
)

// buildBinaries builds ./cmd/corral and ./test/fakeclaude once into a
// process-lifetime temp dir. It uses the import-path form so build.Dir can be
// the contract package dir (any dir inside the module resolves the path).
func buildBinaries(t *testing.T) builtBins {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "corral-contract-bins-")
		if err != nil {
			buildErr = err
			return
		}
		wd, err := os.Getwd()
		if err != nil {
			buildErr = err
			return
		}
		build := func(out, pkg string) error {
			cmd := exec.Command("go", "build", "-o", out, pkg)
			cmd.Dir = wd
			if b, err := cmd.CombinedOutput(); err != nil {
				return &buildFailure{pkg: pkg, out: b, err: err}
			}
			return nil
		}
		corralBin := filepath.Join(dir, "corral")
		fakeBin := filepath.Join(dir, "fakeclaude")
		if buildErr = build(corralBin, "github.com/danielbecerra/corral/cmd/corral"); buildErr != nil {
			return
		}
		if buildErr = build(fakeBin, "github.com/danielbecerra/corral/test/fakeclaude"); buildErr != nil {
			return
		}
		bins = builtBins{corral: corralBin, fakeclaude: fakeBin}
	})
	if buildErr != nil {
		t.Fatalf("building contract binaries: %v", buildErr)
	}
	return bins
}

type buildFailure struct {
	pkg string
	out []byte
	err error
}

func (e *buildFailure) Error() string {
	return "go build " + e.pkg + ": " + e.err.Error() + "\n" + string(e.out)
}

// daemonHandle is a running `corral daemon` subprocess plus a client wired to
// its socket. Cleanup is registered via t.Cleanup by startDaemon.
type daemonHandle struct {
	client   *client.Client
	sockPath string
	stateDir string
}

// startDaemon launches one real `corral daemon` per case. extraEnv is appended
// to the daemon's environment (used to inject CORRAL_SESSION_CLAUDE_BIN,
// CORRAL_SESSION_ENV_PASSTHROUGH, and the fake driver's CORRAL_FAKE_* vars).
//
// The socket lives under os.MkdirTemp (not t.TempDir) because AF_UNIX sun_path
// is ~104 bytes and t.TempDir embeds the full subtest name — long case names
// like TestContract/subagent_attribution/fake would overflow it (mirrors the
// deliberate choice in session_lifecycle_e2e_test.go).
func startDaemon(t *testing.T, extraEnv ...string) *daemonHandle {
	t.Helper()
	b := buildBinaries(t)

	sockDir, err := os.MkdirTemp("", "corral-ct-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "corral.sock")
	stateDir := t.TempDir()

	env := append(os.Environ(),
		"CORRAL_DAEMON_SOCKET="+sockPath,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
	)
	env = append(env, extraEnv...)

	cmd := exec.Command(b.corral, "daemon")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// `corral daemon` double-forks (setsid re-exec) and the foreground call
	// returns once the detached daemon has written its pid line, exactly like
	// session_lifecycle_e2e_test.go.
	if err := cmd.Run(); err != nil {
		t.Fatalf("corral daemon: %v (stderr=%s)", err, stderr.String())
	}
	if !regexp.MustCompile(`daemon started pid=(\d+)`).MatchString(stdout.String()) {
		t.Fatalf("daemon stdout = %q, want \"daemon started pid=...\"", stdout.String())
	}

	c := client.New(sockPath, os.Stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitForVersion(ctx, t, c)

	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), 0)
		waitForGone(t, sockPath, 5*time.Second)
	})

	return &daemonHandle{client: c, sockPath: sockPath, stateDir: stateDir}
}

func waitForVersion(ctx context.Context, t *testing.T, c *client.Client) {
	t.Helper()
	for {
		if _, err := c.Version(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("daemon never answered Version within deadline")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func waitForGone(t *testing.T, sockPath string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}
