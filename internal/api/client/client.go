// Package client is corral's SDK for talking to its own daemon over the
// unix socket (design doc §9): every CLI subcommand that needs the daemon
// (ls, new, attach, kill, and the daemon-launcher's own readiness probe)
// goes through this package rather than building requests by hand, so the
// version handshake, the error envelope, and the daemon-version mismatch
// warning are implemented exactly once.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/danielbecerra/corral/internal/version"
)

// Client talks to one daemon, either over its local unix socket (New) or a
// remote TCP+TLS listener with bearer auth (NewRemote, design doc m5.md
// §7). sockPath is kept (rather than folded into baseURL/target) because
// attach.go dials it directly for the attach-protocol HTTP upgrade, which
// has no client-side hijack API to route through do()'s http.Client.
type Client struct {
	httpClient *http.Client
	sockPath   string
	// baseURL is the request base: "http://corral" for the unix socket
	// (the host portion is inert — DialContext ignores it — but do() still
	// needs a well-formed URL to build requests against), "https://host:port"
	// for a remote daemon.
	baseURL string
	// token is the bearer token sent as "Authorization: Bearer <token>".
	// Empty for a unix-socket Client (design doc §4: the socket's 0600
	// permission is that transport's auth boundary, no token needed).
	token string
	// target is the human-readable dial target named in error messages:
	// sockPath for a local Client, host:port for a remote one.
	target string
	stderr io.Writer

	warnOnce  sync.Once
	sawDaemon string // last Corral-Daemon-Version seen, for tests
}

// New returns a Client that dials sockPath for every request. stderr
// receives the one-time daemon-version-mismatch warning (design doc §9.1);
// pass os.Stderr in production code.
func New(sockPath string, stderr io.Writer) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 10 * time.Second,
		},
		sockPath: sockPath,
		baseURL:  "http://corral",
		target:   sockPath,
		stderr:   stderr,
	}
}

// NewRemote returns a Client that dials host ("host:port") over TLS,
// presenting token as a bearer credential on every request (design doc
// m5.md §7). If caCertPath is non-empty, it is read and pinned as the SOLE
// trusted CA for TLS verification — no other root, including the system
// trust store, is consulted (rule 4: no InsecureSkipVerify, ever; an
// operator who wants a publicly-trusted-CA daemon just leaves this empty).
// If caCertPath is empty, RootCAs is left nil, which makes crypto/tls fall
// back to the system trust store.
func NewRemote(host, token, caCertPath string, stderr io.Writer) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caCertPath != "" {
		pem, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("client: reading client.cacert %s: %w", caCertPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client: client.cacert %s: no valid PEM certificate found", caCertPath)
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
				// Never route corral traffic through an ambient HTTP(S)_PROXY —
				// a bearer token must go straight to the pinned daemon, not
				// through whatever proxy happens to be configured in the
				// caller's environment.
				Proxy: nil,
			},
			Timeout: 10 * time.Second,
		},
		baseURL: "https://" + host,
		token:   token,
		target:  host,
		stderr:  stderr,
	}, nil
}

// APIError is the typed decoding of the error envelope (design doc §9):
// {"error":{"code":...,"message":...,"details":{}}}. Callers that need to
// branch on Code should use errors.As.
type APIError struct {
	StatusCode int
	Code       Code
	Message    string
	Details    map[string]any
}

// Code mirrors api.Code without importing the api package (which would
// pull the whole HTTP server into every CLI binary); the string values are
// part of the wire contract and must stay in sync with api/errors.go.
type Code string

// CodeSessionNotLive mirrors api.CodeSessionNotLive (internal/api/errors.go):
// the session exists but has no live PTY to write to (design doc §7).
const CodeSessionNotLive Code = "session_not_live"

// CodeUnauthorized mirrors api.CodeUnauthorized (internal/api/errors.go:28):
// a request to the TCP+TLS listener with a missing/invalid/revoked bearer
// token (design doc m5.md §6/§7). The unix socket never returns this code —
// it is token-free — so seeing it always means a remote-client auth problem.
const CodeUnauthorized Code = "unauthorized"

func (e *APIError) Error() string {
	if e.Code == CodeUnauthorized {
		// A 401 can only come from the TCP listener (the unix socket is
		// token-free), so this message is always the right diagnosis —
		// unlike the generic fallback below, it doesn't need e.Message.
		return "corral: unauthorized — check your client.token (or CORRAL_CLIENT_TOKEN)"
	}
	return fmt.Sprintf("corral: %s: %s", e.Code, e.Message)
}

type errorEnvelope struct {
	Error struct {
		Code    Code           `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// do performs an HTTP request against the daemon, setting the version
// handshake headers, and returns the raw response with the daemon-version
// mismatch check already applied. Callers are responsible for closing
// resp.Body.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("client: building request: %w", err)
	}
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	req.Header.Set("Corral-Client-Version", version.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.dialError(err)
	}

	c.checkDaemonVersion(resp)
	return resp, nil
}

// dialError wraps a transport error with c.target, and — for a remote (TLS)
// client hitting a certificate-verification failure — points the operator
// at the client.cacert pin they most likely need (design doc m5.md §7 rule
// 4). A local unix-socket Client never has a token and always has
// baseURL == "http://corral", so it never takes the remote-hint branch.
func (c *Client) dialError(err error) error {
	base := fmt.Errorf("client: dialing %s: %w", c.target, err)
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var cv *tls.CertificateVerificationError
	if c.token != "" || c.baseURL != "http://corral" { // remote
		if errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &cv) {
			return fmt.Errorf("%w; if the daemon uses corral's self-signed cert, set client.cacert (or CORRAL_CLIENT_CACERT) to a copy of its <state_dir>/tls/cert.pem", base)
		}
	}
	return base
}

// checkDaemonVersion prints a one-line stderr warning, at most once per
// Client, if the daemon's advertised version differs from this binary's own
// (design doc §9.1: "never fails").
func (c *Client) checkDaemonVersion(resp *http.Response) {
	daemonVersion := resp.Header.Get("Corral-Daemon-Version")
	c.sawDaemon = daemonVersion
	if daemonVersion == "" || daemonVersion == version.Version {
		return
	}
	c.warnOnce.Do(func() {
		if c.stderr != nil {
			fmt.Fprintf(c.stderr, "corral: warning: daemon version %s differs from client version %s\n", daemonVersion, version.Version)
		}
	})
}

// decode reads resp's body, and if status is not 2xx, decodes the error
// envelope into an *APIError. If status is a 404 on a /v1/ path (design doc
// §9.1: a daemon speaking a different API version may not have the route
// at all), the returned error names that possibility explicitly. On
// success, it JSON-decodes the body into out (which may be nil to discard
// it).
func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("client: reading response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("client: decoding response: %w", err)
		}
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("corral: daemon speaks a different API version — restart it (`corral daemon` after `kill <pid>`)")
	}

	var env errorEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("client: request failed with status %d and unparseable body: %s", resp.StatusCode, string(data))
	}
	return &APIError{StatusCode: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message, Details: env.Error.Details}
}

// VersionInfo is the decoded GET /v1/version response.
type VersionInfo struct {
	DaemonVersion string `json:"daemon_version"`
	APIVersion    int    `json:"api_version"`
	SchemaVersion int    `json:"schema_version"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"`
	UptimeMs      int64  `json:"uptime_ms"`
}

// Version calls GET /v1/version.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/version", nil)
	if err != nil {
		return VersionInfo{}, err
	}
	var v VersionInfo
	if err := decode(resp, &v); err != nil {
		return VersionInfo{}, err
	}
	return v, nil
}

// ConfigInfo is the decoded GET /v1/config response.
type ConfigInfo struct {
	Values  map[string]string `json:"values"`
	Sources map[string]string `json:"sources"`
	Ignored []struct {
		File string `json:"file"`
		Key  string `json:"key"`
	} `json:"ignored"`
}

// Config calls GET /v1/config, optionally scoped to cwd ("" resolves the
// daemon's own working directory).
func (c *Client) Config(ctx context.Context, cwd string) (ConfigInfo, error) {
	path := "/v1/config"
	if cwd != "" {
		path += "?cwd=" + url.QueryEscape(cwd)
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ConfigInfo{}, err
	}
	var v ConfigInfo
	if err := decode(resp, &v); err != nil {
		return ConfigInfo{}, err
	}
	return v, nil
}

// Shutdown calls POST /v1/daemon/shutdown with the given grace ("" omits
// the field, letting the daemon use its configured default).
func (c *Client) Shutdown(ctx context.Context, grace time.Duration) error {
	body := map[string]string{}
	if grace > 0 {
		body["grace"] = grace.String()
	}
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("client: encoding shutdown request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/daemon/shutdown", b)
	if err != nil {
		return err
	}
	return decode(resp, nil)
}
