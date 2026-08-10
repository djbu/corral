package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djbu/corral/internal/api/client"
)

const remoteE2EScenario = `{
	"name": "dashboard_remote",
	"steps": [
		{"op": "fire_hook", "event": "SessionStart", "payload": {"source": "startup"}},
		{"op": "fire_hook", "event": "UserPromptSubmit", "payload": {"prompt_id": "p1", "prompt": "run the tests"}},
		{"op": "fire_hook", "event": "PreToolUse", "payload": {"prompt_id": "p1", "tool_name": "Bash", "tool_use_id": "toolu_01", "tool_input": {"command": "npm test"}}},
		{"op": "fire_hook", "event": "PermissionRequest", "payload": {"prompt_id": "p1", "tool_name": "Bash", "tool_input": {"command": "npm test"}}},
		{"op": "wait_stdin", "match": "^.+\\r?\\n$", "capture": "answer", "timeout": "10s"},
		{"op": "fire_hook", "event": "PostToolUse", "payload": {"prompt_id": "p1", "tool_name": "Bash", "tool_use_id": "toolu_01", "tool_response": {"stdout": "ok"}}},
		{"op": "wait_stdin", "match": "^__never_sent__$", "optional": true, "timeout": "60s"},
		{"op": "exit", "code": 0}
	]
}`

type remoteE2EHarness struct {
	ctx    context.Context
	unix   *client.Client
	remote *client.Client
	host   string
	token  string
	http   *http.Client
	caPath string
}

// TestDashboardRemoteE2E exercises the phone-dashboard path against a real
// detached daemon: TLS + bearer authentication, SSE invalidation, a dashboard
// snapshot, and PTY-backed answer delivery all cross a real TCP connection.
func TestDashboardRemoteE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries and spawns a real daemon; skipped in -short")
	}

	h := startRemoteDaemon(t)

	t.Run("AdminPath", func(t *testing.T) {
		frames, stop := subscribeRemoteE2ESSE(t, h.http, h.host, h.token)
		defer stop()

		sess := h.createBlockedSession(t, h.remote)
		if got := waitRemoteE2EFrames(t, frames, 1, 5*time.Second); got < 1 {
			t.Fatalf("SSE frames before blocked = %d, want at least 1", got)
		}
		assertRemoteE2EDashboardState(t, h.http, h.host, h.token, sess.ID, "blocked")

		before := frames.Load()
		h.answerAndWaitUnblocked(t, h.remote, sess.ID)
		if got := waitRemoteE2EFrames(t, frames, before+1, 5*time.Second); got < before+1 {
			t.Fatalf("SSE frames after answer = %d, want at least %d", got, before+1)
		}
	})

	t.Run("ScopedPath", func(t *testing.T) {
		// Create this session through the admin client before minting a token
		// rooted at it: a confined token intentionally cannot create an
		// unrooted, unrelated session for itself.
		owned := h.createBlockedSession(t, h.remote)
		tok, err := h.unix.CreateToken(h.ctx, client.CreateTokenRequest{
			Label: "e2e-scoped", Scope: "session", SessionID: owned.ID,
		})
		if err != nil {
			t.Fatalf("CreateToken scoped: %v", err)
		}
		scoped, err := client.NewRemote(h.host, tok.Token, h.caPath, os.Stderr)
		if err != nil {
			t.Fatalf("NewRemote scoped: %v", err)
		}

		if got, err := scoped.GetSession(h.ctx, owned.ID); err != nil || got.AgentState != "blocked" {
			t.Fatalf("scoped GetSession own = (%q, %v), want blocked, nil", got.AgentState, err)
		}
		h.answerAndWaitUnblocked(t, scoped, owned.ID)

		foreign, err := h.remote.CreateSession(h.ctx, client.CreateSessionRequest{Cwd: t.TempDir()})
		if err != nil {
			t.Fatalf("CreateSession foreign: %v", err)
		}
		h.cleanupSession(t, foreign.ID)
		_, err = scoped.GetSession(h.ctx, foreign.ID)
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
			t.Fatalf("scoped GetSession foreign error = %v, want HTTP 403", err)
		}
		_, err = scoped.Answer(h.ctx, foreign.ID, "no", "", true)
		apiErr = nil
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
			t.Fatalf("scoped Answer foreign error = %v, want HTTP 403", err)
		}
		assertRemoteE2EDashboardAbsent(t, h.http, h.host, tok.Token, foreign.ID)
	})
}

func startRemoteDaemon(t *testing.T) remoteE2EHarness {
	t.Helper()
	binDir := t.TempDir()
	corralBin := filepath.Join(binDir, "corral-e2e")
	build := exec.Command("go", "build", "-o", corralBin, ".")
	build.Dir = mustGetwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build corral: %v\n%s", err, out)
	}
	fakeClaudeBin := buildFakeClaudeE2E(t, binDir)

	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	host := port.Addr().String()
	if err := port.Close(); err != nil {
		t.Fatalf("close preselected port: %v", err)
	}

	sockDir, err := os.MkdirTemp("", "corral-remote-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "corral.sock")
	stateDir := t.TempDir()
	fakeHome, fakeState := t.TempDir(), t.TempDir()
	scenarioPath := filepath.Join(t.TempDir(), "dashboard_remote.json")
	if err := os.WriteFile(scenarioPath, []byte(remoteE2EScenario), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	cmd := exec.Command(corralBin, "daemon")
	cmd.Env = append(os.Environ(),
		"HOME="+fakeHome,
		"CORRAL_DAEMON_SOCKET="+sockPath,
		"CORRAL_DAEMON_STATE_DIR="+stateDir,
		"CORRAL_DAEMON_LISTEN="+host,
		"CORRAL_STATE_PERMISSION_SETTLE=200ms",
		"CORRAL_SESSION_CLAUDE_BIN="+fakeClaudeBin,
		"CORRAL_SESSION_ENV_PASSTHROUGH=CORRAL_FAKE_SCENARIO,CORRAL_FAKE_STATE,CORRAL_FAKE_HOME",
		"CORRAL_FAKE_SCENARIO="+scenarioPath,
		"CORRAL_FAKE_STATE="+fakeState,
		"CORRAL_FAKE_HOME="+fakeHome,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("corral daemon: %v (stderr=%s)", err, stderr.String())
	}
	if m := regexp.MustCompile(`daemon started pid=(\d+)`).FindStringSubmatch(stdout.String()); m == nil {
		t.Fatalf("daemon stdout = %q, want daemon started pid", stdout.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	unixC := client.New(sockPath, os.Stderr)
	if _, err := waitForVersion(ctx, t, unixC); err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Cleanup(func() {
		_ = unixC.Shutdown(context.Background(), 0)
		waitForGone(t, sockPath, 5*time.Second)
	})

	token, err := unixC.CreateToken(ctx, client.CreateTokenRequest{Label: "e2e", Scope: "admin"})
	if err != nil {
		t.Fatalf("CreateToken admin: %v", err)
	}
	caPath := filepath.Join(stateDir, "tls", "cert.pem")
	waitRemoteE2EFile(t, caPath, 5*time.Second)
	remote, err := client.NewRemote(host, token.Token, caPath, os.Stderr)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	return remoteE2EHarness{ctx: ctx, unix: unixC, remote: remote, host: host, token: token.Token, http: remoteE2EHTTPClient(t, caPath), caPath: caPath}
}

func (h remoteE2EHarness) createBlockedSession(t *testing.T, c *client.Client) client.SessionInfo {
	t.Helper()
	sess, err := c.CreateSession(h.ctx, client.CreateSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	h.cleanupSession(t, sess.ID)
	deadline := time.Now().Add(5 * time.Second)
	last := sess.AgentState
	for time.Now().Before(deadline) {
		got, err := c.GetSession(h.ctx, sess.ID)
		if err != nil {
			t.Fatalf("GetSession %s: %v", sess.ID, err)
		}
		last = got.AgentState
		if last == "blocked" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s state = %q, want blocked within 5s", sess.ID, last)
	return client.SessionInfo{}
}

// cleanupSession reaps a scenario child before t.TempDir's cleanup. The fake
// scenario parks after its resolving hook so snapshot assertions see a live
// session; without this ordering it can still record hook output while Go
// removes its fake state directory on Darwin.
func (h remoteE2EHarness) cleanupSession(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = h.remote.KillSession(context.Background(), id, 20*time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			got, err := h.remote.GetSession(context.Background(), id)
			if err == nil && got.Status != "running" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func (h remoteE2EHarness) answerAndWaitUnblocked(t *testing.T, c *client.Client, id string) {
	t.Helper()
	if _, err := c.Answer(h.ctx, id, "yes", "", true); err != nil {
		t.Fatalf("Answer %s: %v", id, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	last := "blocked"
	for time.Now().Before(deadline) {
		got, err := c.GetSession(h.ctx, id)
		if err != nil {
			t.Fatalf("GetSession %s after answer: %v", id, err)
		}
		last = got.AgentState
		if last != "blocked" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s remained %q within 5s after answer", id, last)
}

func remoteE2EHTTPClient(t *testing.T, caPath string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read TLS CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("TLS CA %s contains no certificate", caPath)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, Proxy: nil}}
}

func subscribeRemoteE2ESSE(t *testing.T, hc *http.Client, host, token string) (*atomic.Int64, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v1/events/stream?token="+token, nil)
	if err != nil {
		cancel()
		t.Fatalf("build SSE request: %v", err)
	}
	req.Header.Set("Corral-Api-Version", "1")
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
	}
	frames := new(atomic.Int64)
	go func() {
		defer resp.Body.Close()
		s := bufio.NewScanner(resp.Body)
		for s.Scan() {
			if strings.HasPrefix(s.Text(), "data:") {
				frames.Add(1)
			}
		}
	}()
	return frames, cancel
}

func waitRemoteE2EFrames(t *testing.T, frames *atomic.Int64, want int64, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := frames.Load(); got >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return frames.Load()
}

func waitRemoteE2EFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear within %s", path, timeout)
}

func assertRemoteE2EDashboardState(t *testing.T, hc *http.Client, host, token, id, want string) {
	t.Helper()
	for _, sess := range remoteE2EDashboard(t, hc, host, token) {
		if sess.ID == id {
			if sess.AgentState != want {
				t.Fatalf("dashboard session %s state = %q, want %q", id, sess.AgentState, want)
			}
			return
		}
	}
	t.Fatalf("dashboard did not list session %s", id)
}

func assertRemoteE2EDashboardAbsent(t *testing.T, hc *http.Client, host, token, id string) {
	t.Helper()
	for _, sess := range remoteE2EDashboard(t, hc, host, token) {
		if sess.ID == id {
			t.Fatalf("scoped dashboard exposed foreign session %s", id)
		}
	}
}

func remoteE2EDashboard(t *testing.T, hc *http.Client, host, token string) []client.SessionInfo {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/v1/dashboard", nil)
	if err != nil {
		t.Fatalf("build dashboard request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Corral-Api-Version", "1")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET dashboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Sessions []client.SessionInfo `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	return body.Sessions
}
