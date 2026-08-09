package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
	"github.com/danielbecerra/corral/internal/version"
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

// TestForeground_TCPListener_HalfSetOverride proves M5 step 30's
// both-or-neither fail-closed rule: setting daemon.listen with only
// daemon.tls_cert (and not daemon.tls_key) must abort startup with an
// error, never silently fall back to the self-signed bootstrap cert while
// the operator believes their own cert is live.
func TestForeground_TCPListener_HalfSetOverride(t *testing.T) {
	cfg := testConfig(t)
	cfg.Listen = "127.0.0.1:0"
	cfg.TLSCert = "/nonexistent/cert.pem"
	cfg.TLSKey = ""

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer readyR.Close()

	err = run(context.Background(), cfg, readyW)
	if err == nil {
		t.Fatalf("run() with a half-set tls_cert/tls_key override: want error, got nil")
	}

	line := waitForReadyLine(t, readyR, 5*time.Second)
	if line == "OK" {
		t.Fatalf("ready line = %q, want an ERR: line", line)
	}
}

// TestForeground_TCPListener_RoundTrip is M5's TCP+TLS smoke test: with
// daemon.listen set (self-signed path, no operator override), the daemon
// must serve HTTPS on that address using tlsbootstrap's generated cert, and
// bearer-auth must be enforced there exactly as it is documented to be
// (design doc §4/§6) — an unauthenticated request is rejected, and a
// request carrying a freshly minted admin token succeeds.
//
// Every listener bound in this test is 127.0.0.1-only (never a wildcard,
// never :0 in the config itself — the free port is discovered up front and
// then reused as a fixed 127.0.0.1:<port> address), matching the
// no-outward-facing-action guarantee the rest of the daemon test suite
// relies on.
func TestForeground_TCPListener_RoundTrip(t *testing.T) {
	// Standard Go free-port idiom: bind 127.0.0.1:0, read back the assigned
	// port, close it, then hand that fixed 127.0.0.1:<port> address to the
	// daemon. Never 0.0.0.0/:: /a bare :0 — always loopback-only.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen (free port probe): %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	cfg := testConfig(t)
	cfg.Listen = addr

	c, stop := startForTest(t, cfg)
	defer stop()

	// The self-signed cert lands at StateDir/tls/cert.pem (tlsbootstrap's
	// fixed basename); pin it as the client's trusted CA rather than
	// relying on the system trust store, mirroring the design doc §7
	// client-pinning mechanism. 127.0.0.1 is in the cert's SANs by default
	// (tlsbootstrap's fixed defaults), so hostname verification passes.
	certPEM, err := os.ReadFile(filepath.Join(cfg.StateDir, "tls", "cert.pem"))
	if err != nil {
		t.Fatalf("reading generated cert: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatalf("AppendCertsFromPEM: failed to parse the generated cert")
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}

	get := func(token string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/version", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET https://%s/v1/version: %v", addr, err)
		}
		return resp
	}

	// No Authorization header at all: bearer-auth must reject it, even
	// though the version-handshake header is present and correct — over
	// TCP, auth runs outermost (AuthenticatedHandler), so an
	// unauthenticated caller learns nothing before proving a token.
	unauth := get("")
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status (no Authorization header) = %d, want %d", unauth.StatusCode, http.StatusUnauthorized)
	}

	// Mint an admin token over the unix socket (the SDK client, unaffected
	// by this test's TCP listener), then present it over TLS: the same
	// token must be honored on the network-facing listener.
	created, err := c.CreateToken(context.Background(), client.CreateTokenRequest{Label: "tcp-roundtrip-test"})
	if err != nil {
		t.Fatalf("CreateToken (over unix socket): %v", err)
	}
	authed := get(created.Token)
	defer authed.Body.Close()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("status (valid bearer token) = %d, want %d", authed.StatusCode, http.StatusOK)
	}
}

// TestForeground_SSEStream_ClosesOnShutdown is step 32's regression test for
// the shutdown ordering in shutdown.go: broker.Close() must run before
// tcpSrv.Shutdown, because http.Server.Shutdown(context.Background()) has no
// deadline of its own and only returns once every in-flight handler
// returns — and a live GET /v1/events/stream handler never returns on its
// own (its request context only cancels when the underlying connection
// drops, which a well-behaved EventSource client never does). Without
// broker.Close() unblocking that handler's select loop first, this test's
// call to stop() would hang until its own internal 5s safety-net fires,
// instead of completing promptly.
//
// This also exercises both EventSource-only exemptions end to end on the
// real network-facing TCP+TLS listener (not just the internal/api unit
// tests): no Corral-Api-Version header, and a ?token=... query param
// instead of an Authorization header.
func TestForeground_SSEStream_ClosesOnShutdown(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen (free port probe): %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	// Points session spawning at a binary that can't exist, so
	// POST /v1/sessions's session.created event (handlers_sessions.go)
	// still gets durably appended — and published to the SSE broker —
	// before the subsequent Registry.Spawn call fails. That failure
	// surfaces as a 500 to the client, which this test ignores: only the
	// broker-published event matters here, not whether a real session ever
	// starts running.
	t.Setenv("CORRAL_SESSION_CLAUDE_BIN", filepath.Join(t.TempDir(), "no-such-claude-binary"))

	cfg := testConfig(t)
	cfg.Listen = addr

	c, stop := startForTest(t, cfg)
	// stop() is not safe to call twice (its second call would block on an
	// already-drained runErr channel until its own 5s Fatal fires), so any
	// t.Fatalf between here and the deliberate stop() call below — over TLS
	// cert loading, CreateToken, the stream request, or reading frames off
	// it — must still tear the daemon down, or it keeps the flock and TCP
	// port held for the rest of this test binary's other daemon tests.
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	certPEM, err := os.ReadFile(filepath.Join(cfg.StateDir, "tls", "cert.pem"))
	if err != nil {
		t.Fatalf("reading generated cert: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatalf("AppendCertsFromPEM: failed to parse the generated cert")
	}
	// Deliberately no Timeout: a real EventSource keeps its connection open
	// indefinitely and relies on the server (or the user) to end it, which
	// is exactly what this test needs to prove — that server-side shutdown
	// ends it promptly, not that the client eventually gives up on its own.
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}

	created, err := c.CreateToken(context.Background(), client.CreateTokenRequest{Label: "sse-shutdown-test"})
	if err != nil {
		t.Fatalf("CreateToken (over unix socket): %v", err)
	}

	// No Authorization header, no Corral-Api-Version header: exactly what a
	// browser EventSource can send.
	req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/events/stream?token="+created.Token, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET https://%s/v1/events/stream: %v", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (both EventSource exemptions must apply here)", resp.StatusCode, http.StatusOK)
	}

	r := bufio.NewReader(resp.Body)

	// Trigger an event: CreateSession's AppendEvent happens before the
	// (here, doomed) spawn attempt, so this call may well return an error —
	// that's expected and ignored. Called synchronously (not concurrently
	// with the c.Shutdown call below): *client.Client's checkDaemonVersion
	// mutates unsynchronized state on c, so two calls in flight on the same
	// Client at once is a data race in the test, not something the product
	// needs to support — exec.LookPath failing on a nonexistent binary is
	// fast, so there's no risk of this blocking the test.
	cwd := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = c.CreateSession(ctx, client.CreateSessionRequest{Cwd: cwd})
	cancel()

	type readResult struct {
		line string
		err  error
	}
	readLineAsync := func() <-chan readResult {
		ch := make(chan readResult, 1)
		go func() {
			line, err := r.ReadString('\n')
			ch <- readResult{line, err}
		}()
		return ch
	}
	readLine := func() (string, error) {
		select {
		case res := <-readLineAsync():
			return res.line, res.err
		case <-time.After(5 * time.Second):
			t.Fatal("timed out reading a frame from the SSE stream")
			return "", nil
		}
	}

	// Skip past any heartbeat comments to the session.created data frame —
	// heartbeatInterval is 20s in production, but this keeps the test
	// robust regardless.
	var dataLine string
	for i := 0; i < 10; i++ {
		line, err := readLine()
		if err != nil {
			t.Fatalf("reading SSE frame: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			dataLine = line
			break
		}
	}
	if dataLine == "" {
		t.Fatal("never saw a data: frame on the SSE stream")
	}
	if !strings.Contains(dataLine, `"kind":"session.created"`) {
		t.Fatalf("data frame = %q, want it to contain session.created", dataLine)
	}
	// Consume the blank line that terminates this "data: ...\n\n" frame,
	// so the post-shutdown read below can't mistake it for evidence the
	// connection is still open.
	if line, err := readLine(); err != nil || strings.TrimRight(line, "\r\n") != "" {
		t.Fatalf("expected the blank line terminating the data frame, got %q (err=%v)", line, err)
	}

	// Now shut the daemon down with the stream still open. stop() blocks
	// until run() returns (with its own 5s safety-net Fatal if it doesn't);
	// timing it independently here proves the real regression — that this
	// stays close to ShutdownGrace (200ms) rather than silently eating that
	// whole 5s budget every time because a live stream wedged Shutdown.
	shutdownStart := time.Now()
	stop()
	stopped = true
	if elapsed := time.Since(shutdownStart); elapsed > 2*time.Second {
		t.Fatalf("shutdown with a live SSE stream open took %s, want well under 2s (broker.Close() should unblock it almost immediately)", elapsed)
	}

	// The connection itself must also have been torn down server-side, not
	// just left dangling while run() happened to return. Keep draining:
	// the doomed spawn also durably appends session.spawn_failed
	// (supervisor.go), and whether that frame lands before or after
	// shutdown begins is a genuine race unrelated to what this test is
	// checking, so any number of already-buffered frames must be tolerated
	// before the read that finally observes the closed connection.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case res := <-readLineAsync():
			if res.err != nil {
				return // success: the connection closed, as required.
			}
		case <-deadline:
			t.Fatal("SSE stream's underlying connection was not closed within 2s of shutdown completing")
		}
	}
}

// TestForeground_RemoteClient_RoundTrip is M5 step 31's exit criterion: the
// client SDK's NewRemote constructor (internal/api/client/client.go), used
// exactly as `corral --host` wires it up, against a real TCP+TLS listener
// with the self-signed bootstrap cert — no operator TLS override, mirroring
// TestForeground_TCPListener_RoundTrip above but exercising the SDK's own
// remote path instead of a hand-built http.Client.
//
// Every listener bound in this test is loopback-only (the same free-port-
// on-127.0.0.1 probe as TestForeground_TCPListener_RoundTrip above) — never
// wildcard, never a bare :0.
func TestForeground_RemoteClient_RoundTrip(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen (free port probe): %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	cfg := testConfig(t)
	cfg.Listen = addr

	c, stop := startForTest(t, cfg)
	defer stop()

	// Mint an admin token over the unix socket (the SDK client, unaffected
	// by this test's TCP listener) — the same mint-over-unix pattern as
	// TestForeground_TCPListener_RoundTrip above.
	created, err := c.CreateToken(context.Background(), client.CreateTokenRequest{Label: "remote-client-roundtrip-test"})
	if err != nil {
		t.Fatalf("CreateToken (over unix socket): %v", err)
	}

	// The self-signed cert lands at StateDir/tls/cert.pem (tlsbootstrap's
	// fixed basename) — the exact path client.cacert is documented to point
	// at (design doc m5.md §7 rule 4).
	certPath := filepath.Join(cfg.StateDir, "tls", "cert.pem")

	// Valid token + pinned cert: ListSessions must succeed. An empty list is
	// fine — this proves the remote round trip works end to end, not that
	// any particular session exists.
	rc, err := client.NewRemote(addr, created.Token, certPath, io.Discard)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	if _, err := rc.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions (valid token, pinned cert): %v", err)
	}

	// Bad token: the daemon's bearer-auth middleware must reject it with
	// api.CodeUnauthorized, and the SDK must decode that into an
	// *client.APIError an errors.As caller can branch on.
	badRC, err := client.NewRemote(addr, "crl_bogus", certPath, io.Discard)
	if err != nil {
		t.Fatalf("NewRemote (bad token): %v", err)
	}
	_, err = badRC.ListSessions(context.Background())
	if err == nil {
		t.Fatal("ListSessions (bad token): want error, got nil")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ListSessions (bad token) error %v does not errors.As to *client.APIError", err)
	}
	if apiErr.Code != client.CodeUnauthorized {
		t.Fatalf("ListSessions (bad token) APIError.Code = %q, want %q", apiErr.Code, client.CodeUnauthorized)
	}

	// No cacert against a self-signed cert: the system trust store won't
	// trust it, so this must fail — and the error must carry the
	// client.cacert hint dialError adds for exactly this case.
	noCACertRC, err := client.NewRemote(addr, created.Token, "", io.Discard)
	if err != nil {
		t.Fatalf("NewRemote (no cacert): %v", err)
	}
	_, err = noCACertRC.ListSessions(context.Background())
	if err == nil {
		t.Fatal("ListSessions (no cacert, self-signed daemon): want error, got nil")
	}
	if !strings.Contains(err.Error(), "client.cacert") {
		t.Fatalf("ListSessions (no cacert) error %q does not mention client.cacert", err.Error())
	}
}
