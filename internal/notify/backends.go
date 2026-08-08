package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NewHTTPClient builds the single http.Client every backend shares. Timeout is
// notify.timeout (§8.2). The Transport pins Proxy: nil deliberately — the
// daemon's environment is frozen and audited, so honoring HTTP_PROXY /
// HTTPS_PROXY would open an unaudited egress path for corral's only
// third-party egress. This is a security property, not a default.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
}

// sendError carries whether a failed Send should be retried. Transport errors
// and 5xx are retryable; 4xx (a config error — bad topic, bad token, bad URL)
// is not. The dispatcher classifies via retryable() below.
type sendError struct {
	retry bool
	msg   string
}

func (e *sendError) Error() string   { return e.msg }
func (e *sendError) Retryable() bool { return e.retry }

// retryable reports whether err is a retryable send failure. A nil error and
// an error that does not implement Retryable() both report false (nothing to
// retry).
func retryable(err error) bool {
	var r interface{ Retryable() bool }
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return false
}

// classifyStatus turns an HTTP status into nil (2xx), a retryable error (5xx),
// or a permanent error (everything else — 3xx/4xx). backend names the source
// for the error message and the notify.failed event.
func classifyStatus(backend string, status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status >= 500:
		return &sendError{retry: true, msg: fmt.Sprintf("%s: status %d", backend, status)}
	default:
		return &sendError{retry: false, msg: fmt.Sprintf("%s: status %d", backend, status)}
	}
}

// drainAndClose reads a bounded prefix of the response body (so the connection
// can be reused) and closes it. Bodies are never inspected — corral never logs
// a third party's response content.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// NtfyBackend posts to an ntfy server (§8.2): POST {server}/{topic}, body =
// Detail, headers Title/Priority/Tags, and Authorization: Bearer when a token
// is configured.
type NtfyBackend struct {
	server   string
	topic    string
	token    string
	priority string
	client   *http.Client
}

// NewNtfyBackend builds an ntfy backend. server has no trailing slash
// requirement — it is joined to topic with a single "/".
func NewNtfyBackend(server, topic, token, priority string, client *http.Client) *NtfyBackend {
	return &NtfyBackend{
		server:   strings.TrimRight(server, "/"),
		topic:    topic,
		token:    token,
		priority: priority,
		client:   client,
	}
}

func (b *NtfyBackend) Name() string { return "ntfy" }

func (b *NtfyBackend) Send(ctx context.Context, n Notification) error {
	url := b.server + "/" + b.topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(n.Detail))
	if err != nil {
		return &sendError{retry: false, msg: "ntfy: build request: " + err.Error()}
	}
	req.Header.Set("Title", n.Title())
	req.Header.Set("Tags", "warning")
	if b.priority != "" {
		req.Header.Set("Priority", b.priority)
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		// Transport failure (DNS, connection, timeout) — retryable.
		return &sendError{retry: true, msg: "ntfy: post: " + err.Error()}
	}
	defer drainAndClose(resp)
	return classifyStatus("ntfy", resp.StatusCode)
}

// WebhookBackend posts the marshaled Notification as JSON to a static URL
// (§8.2), plus any configured static headers. Headers are secret-class and are
// never logged.
type WebhookBackend struct {
	url     string
	headers map[string]string
	client  *http.Client
}

// NewWebhookBackend builds a webhook backend. headers may be nil.
func NewWebhookBackend(url string, headers map[string]string, client *http.Client) *WebhookBackend {
	return &WebhookBackend{url: url, headers: headers, client: client}
}

func (b *WebhookBackend) Name() string { return "webhook" }

func (b *WebhookBackend) Send(ctx context.Context, n Notification) error {
	// n.Summary and n.Detail are already redacted by the dispatcher before the
	// Notification reaches any backend, so marshaling the struct cannot egress
	// an un-redacted secret.
	body, err := json.Marshal(n)
	if err != nil {
		return &sendError{retry: false, msg: "webhook: marshal: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		return &sendError{retry: false, msg: "webhook: build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return &sendError{retry: true, msg: "webhook: post: " + err.Error()}
	}
	defer drainAndClose(resp)
	return classifyStatus("webhook", resp.StatusCode)
}
