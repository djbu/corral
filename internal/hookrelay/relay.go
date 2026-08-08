package hookrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// MaxStdinBytes is the hard cap on how much of a hook's stdin the relay
// will read (design doc §2.4 step 1): "Over the cap -> truncate, set
// truncated: true, continue (do not fail; the agent is blocked on us)."
const MaxStdinBytes = 1 << 20 // 1 MiB

// DefaultHookTimeout is used when CORRAL_HOOK_TIMEOUT is unset or empty
// (design doc §2.4 step 4).
const DefaultHookTimeout = 2 * time.Second

// ReadStdin reads up to MaxStdinBytes+1 bytes from r and returns the first
// MaxStdinBytes of them, plus whether the input was longer than that (and
// therefore truncated). It never returns an error for "input too long" —
// per §2.4, an oversized hook payload is truncated and delivery continues,
// never fails.
func ReadStdin(r io.Reader) (data []byte, truncated bool, err error) {
	limited := io.LimitReader(r, MaxStdinBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, fmt.Errorf("hookrelay: reading stdin: %w", err)
	}
	if int64(len(b)) > MaxStdinBytes {
		return b[:MaxStdinBytes], true, nil
	}
	return b, false, nil
}

// Deliver posts body (the raw hook payload bytes, forwarded verbatim and
// undecoded — design doc §2.6: "The relay does not decode the payload")
// to the daemon at sockPath, with the headers §2.6 specifies. It applies a
// hard deadline of timeout to the whole call (dial + request + response).
//
// On success it returns the decoded Decision from the daemon's response
// (nil if the daemon sent none or an explicit null, per Response.
// HasDecision). On any failure — dial refused, deadline exceeded, non-2xx
// status, or a response body that doesn't decode as Response — it returns
// a non-nil error and a nil decision.
//
// Deliver never decides whether the caller should exit non-zero: design doc
// §2.4's "exit 0 always, unconditionally" is a CLI-layer policy
// (cmd_hook_relay.go), not this function's — Deliver's error return exists
// purely so the caller can log one stderr line.
func Deliver(ctx context.Context, sockPath, sessionID, sessionSecret, hookEvent, deliveryID string, body []byte, timeout time.Duration) (decision json.RawMessage, err error) {
	if timeout <= 0 {
		timeout = DefaultHookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/hooks/events", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hookrelay: building request: %w", err)
	}
	req.Header.Set("Corral-Api-Version", "1")
	req.Header.Set("Corral-Session-Id", sessionID)
	req.Header.Set("Corral-Session-Secret", sessionSecret)
	req.Header.Set("Corral-Hook-Event", hookEvent)
	req.Header.Set("Corral-Delivery-Id", deliveryID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hookrelay: delivering to %s: %w", sockPath, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hookrelay: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hookrelay: daemon responded %d: %s", resp.StatusCode, string(respBody))
	}

	if len(respBody) == 0 {
		return nil, nil
	}
	var envelope Response
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("hookrelay: decoding response: %w", err)
	}
	if !envelope.HasDecision() {
		return nil, nil
	}
	return envelope.Decision, nil
}

// Run is the whole client-side sequence design doc §2.4 specifies, given an
// already-parsed --event value and an injectable stdin/env so it is fully
// testable without a real subprocess or real environment variables.
// cmd_hook_relay.go is a thin wrapper: Run(ctx, event, os.Stdin, stdout,
// stderr, os.Getenv), and the subcommand always returns exit code 0
// regardless of what Run does internally — that unconditional-exit-0
// policy lives in the CLI layer, not here, but Run's own behavior never
// gives the caller a reason to want anything else:
//
//  1. Read CORRAL_SOCK / CORRAL_SESSION_ID / CORRAL_SESSION_SECRET. Any
//     missing -> return immediately, no output at all: a hook fired from a
//     process corral did not spawn is not corral's business.
//  2. Read stdin, capped at MaxStdinBytes.
//  3. Generate a delivery id (uuidv4) and POST to the daemon over the
//     socket, with a hard deadline of CORRAL_HOOK_TIMEOUT (default
//     DefaultHookTimeout).
//  4. On any failure (stdin read error, dial refused, deadline, non-2xx,
//     garbage response body): write one line to stderr and return.
//  5. On success, print the decision to stdout only if the daemon actually
//     sent a non-null one (in M2 it never does).
func Run(ctx context.Context, event string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) {
	sock := getenv("CORRAL_SOCK")
	sessionID := getenv("CORRAL_SESSION_ID")
	secret := getenv("CORRAL_SESSION_SECRET")
	if sock == "" || sessionID == "" || secret == "" {
		return
	}

	body, _, err := ReadStdin(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "corral hook-relay: %v\n", err)
		return
	}

	timeout := DefaultHookTimeout
	if s := getenv("CORRAL_HOOK_TIMEOUT"); s != "" {
		if d, perr := time.ParseDuration(s); perr == nil {
			timeout = d
		}
	}

	deliveryID := uuid.NewString()
	decision, err := Deliver(ctx, sock, sessionID, secret, event, deliveryID, body, timeout)
	if err != nil {
		fmt.Fprintf(stderr, "corral hook-relay: %v\n", err)
		return
	}
	if len(decision) > 0 {
		stdout.Write(decision)
	}
}
