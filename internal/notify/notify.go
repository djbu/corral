// Package notify delivers "session blocked" notifications to external channels
// (ntfy, webhook) without ever letting a slow or failed delivery touch the
// state machine (RUNBOOK §5.3, design doc §8).
//
// The load-bearing property is structural, not disciplinary: the engine holds
// this package's Dispatcher through the state.Notifier seam and calls
// NotifyBlocked on the per-session goroutine. NotifyBlocked does exactly one
// thing that can vary in cost — a non-blocking send to a bounded channel — and
// then returns nil. Every network call, retry, backoff, and store write
// happens later on the Dispatcher's own worker goroutine, driven by an
// injected clock.Clock so tests advance time instead of sleeping. There is no
// code path by which a hung ntfy server delays a session transition, because
// there is no code path from NotifyBlocked to http.Client.Do.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/redact"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// queueCap is the bounded queue depth (§8.4). Full → drop, never block.
const queueCap = 128

// Notification is one deliverable event (§8.1). Summary and Detail are already
// redacted by the time a Notification reaches a Backend.
type Notification struct {
	SessionID   string    `json:"session_id"`
	SessionName string    `json:"session_name"`
	Cwd         string    `json:"cwd"`
	Event       string    `json:"event"`       // "blocked" | "exited"
	ReasonKind  string    `json:"reason_kind"` // BlockedReason.Kind
	Summary     string    `json:"summary"`     // one line, already redacted
	Detail      string    `json:"detail"`      // multi-line body, already redacted
	Hint        string    `json:"hint"`        // e.g. `corral answer api-refactor "1"`
	At          time.Time `json:"at"`

	// redactions is the sorted union of redact rule names that fired while
	// building Summary/Detail. Unexported → excluded from the webhook JSON
	// body (it is audit metadata, not payload); recorded in the notify.sent
	// event so an egressing redaction leaves a trace (§8.5).
	redactions []string
}

// Title renders the ntfy notification title, e.g. "corral/api-refactor
// blocked". SessionName is the friendly session identifier the operator sees.
func (n Notification) Title() string {
	return "corral/" + n.SessionName + " " + n.Event
}

// Backend is one delivery channel (ntfy, webhook). It is the §8.1 "Notifier"
// interface, renamed here to avoid colliding with state.Notifier (the engine
// seam the Dispatcher implements). Send must be safe for concurrent use and
// must honor ctx's deadline.
type Backend interface {
	Name() string
	Send(ctx context.Context, n Notification) error
}

// Options are the dispatcher-level knobs resolved from [notify] config.
type Options struct {
	On       []string      // subset of {"blocked","exited"}
	Debounce time.Duration // suppress identical (session,kind,summary) within this window
	Timeout  time.Duration // per-attempt HTTP deadline (also the http.Client timeout)
	Retries  int           // retries AFTER the initial attempt; total sends = 1 + Retries
}

// job is the minimal item NotifyBlocked enqueues. It carries only what the
// session goroutine already had in hand — no store lookups, no redaction, no
// Notification building happens on that goroutine.
type job struct {
	sessionID string
	reason    state.BlockedReason
	event     string
	at        time.Time
}

// Dispatcher implements state.Notifier. See the package doc for the concurrency
// contract.
type Dispatcher struct {
	backends []Backend
	clk      clock.Clock
	store    *store.Store
	log      *slog.Logger

	on          map[string]bool
	debounce    time.Duration
	timeout     time.Duration
	maxAttempts int // 1 + Retries

	// ctx is cancelled by Close so an in-flight backend Send (which the done
	// channel alone cannot interrupt — done is only observed by run and
	// backoff) aborts promptly, keeping daemon shutdown from blocking for the
	// full http.Client timeout on a black-holed host.
	ctx    context.Context
	cancel context.CancelFunc

	queue chan job
	done  chan struct{}
	wg    sync.WaitGroup

	mu       sync.Mutex
	lastSent map[string]time.Time // debounce key -> last dispatch time

	// afterArm and afterJob are TEST-ONLY synchronization hooks, both nil in
	// production. afterArm fires immediately AFTER a backoff timer's waiter has
	// been registered with the clock (so a FakeClock test can block on it
	// before Advance, guaranteeing the waiter exists). afterJob fires after a
	// job is fully processed (all backends attempted), so a test can wait for
	// terminal events without polling.
	afterArm func()
	afterJob func()
}

var _ state.Notifier = (*Dispatcher)(nil)

// New builds a Dispatcher. backends may be empty (the Dispatcher then records
// nothing but never errors — a valid "notifications configured but no channel
// enabled" state). log defaults to slog.Default(). Retries<0 is clamped to 0.
func New(backends []Backend, opts Options, clk clock.Clock, st *store.Store, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	retries := opts.Retries
	if retries < 0 {
		retries = 0
	}
	on := map[string]bool{}
	for _, e := range opts.On {
		on[e] = true
	}
	if len(on) == 0 {
		// M2 delivers "blocked" only. "exited" is an accepted config value
		// (config validates it) but a no-op here — NotifyBlocked is the sole
		// entry point, so an on=["exited"] dispatcher notifies nothing until a
		// later milestone wires an exit path. Defaulting empty→blocked keeps a
		// zero-Options dispatcher useful.
		on["blocked"] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		backends:    backends,
		clk:         clk,
		store:       st,
		log:         log,
		on:          on,
		debounce:    opts.Debounce,
		timeout:     opts.Timeout,
		maxAttempts: 1 + retries,
		ctx:         ctx,
		cancel:      cancel,
		queue:       make(chan job, queueCap),
		done:        make(chan struct{}),
		lastSent:    map[string]time.Time{},
	}
}

// Start launches the worker goroutine. It is idempotent-unsafe: call exactly
// once, paired with Close.
func (d *Dispatcher) Start() {
	d.wg.Add(1)
	go d.run()
}

// Close signals the worker to stop and waits for it. Queued-but-undelivered
// jobs are dropped on shutdown — a daemon stopping is not the moment to block
// on a slow ntfy server.
func (d *Dispatcher) Close() {
	// Order matters: cancel first so any Send blocked in the http.Client (a
	// black-holed host that never sends a RST) aborts now instead of holding
	// the worker for the full client timeout; close(done) then stops run and
	// any backoff wait; Wait then joins the drained worker. Queued-but-unstarted
	// jobs are dropped — a stopping daemon does not deliver a backlog.
	d.cancel()
	close(d.done)
	d.wg.Wait()
}

// NotifyBlocked implements state.Notifier. It is called synchronously on the
// engine's per-session goroutine and MUST NOT block: it builds a minimal job
// and does a non-blocking channel send, then returns. The return value is
// always nil by construction — delivery outcomes are recorded as events
// (notify.sent/failed/dropped/suppressed), never surfaced here. Returning an
// error would only add log noise on the session goroutine (r.notified is
// already set before this call) and would tempt a future reader to move the
// HTTP path onto this goroutine. Do not.
func (d *Dispatcher) NotifyBlocked(sessionID string, reason state.BlockedReason) error {
	if !d.on["blocked"] {
		return nil
	}
	j := job{sessionID: sessionID, reason: reason, event: "blocked", at: d.clk.Now()}
	select {
	case d.queue <- j:
	default:
		// Queue full: drop and record, never block the state machine.
		d.log.Warn("notify: queue full, dropping notification", "session_id", sessionID)
		d.appendEvent(sessionID, session.EventNotifyDropped, map[string]any{
			"event":     "blocked",
			"queue_cap": queueCap,
		})
	}
	return nil
}

// run is the worker loop: dequeue, process, repeat until Close.
func (d *Dispatcher) run() {
	defer d.wg.Done()
	for {
		select {
		case <-d.done:
			return
		case j := <-d.queue:
			d.process(j)
			if d.afterJob != nil {
				d.afterJob()
			}
		}
	}
}

// process handles one job end to end: resolve the session, build+redact the
// Notification, apply debounce, and dispatch to every backend with retry.
func (d *Dispatcher) process(j job) {
	name, cwd := d.resolveSession(j.sessionID)
	n := d.buildNotification(j, name, cwd)

	// Debounce on (session_id, reason_kind, summary). The summary is the
	// redacted one — that is the identity that actually egresses.
	key := j.sessionID + "\x00" + n.ReasonKind + "\x00" + n.Summary
	d.mu.Lock()
	last, seen := d.lastSent[key]
	within := seen && d.debounce > 0 && j.at.Sub(last) < d.debounce
	if !within {
		// Record the dispatch time now (at gate-pass, not at delivery success):
		// a flapping block whose first delivery failed must not re-notify 5s
		// later — the debounce is about operator noise, not delivery success.
		d.lastSent[key] = j.at
	}
	d.mu.Unlock()

	if within {
		d.appendEvent(j.sessionID, session.EventNotifySuppressed, map[string]any{
			"event":       n.Event,
			"reason_kind": n.ReasonKind,
			"summary":     n.Summary,
			"within_ms":   j.at.Sub(last).Milliseconds(),
			"debounce_ms": d.debounce.Milliseconds(),
		})
		return
	}

	for _, be := range d.backends {
		d.deliver(be, n)
	}
}

// deliver attempts one backend with retry/backoff, then records the outcome.
func (d *Dispatcher) deliver(be Backend, n Notification) {
	attempts, err := d.attempt(be, n)
	if err == nil {
		d.appendEvent(n.SessionID, session.EventNotifySent, map[string]any{
			"backend":     be.Name(),
			"attempts":    attempts,
			"event":       n.Event,
			"reason_kind": n.ReasonKind,
			"redactions":  n.redactions,
		})
		return
	}
	// Redact the error string before persisting: a transport error can embed a
	// URL, and a webhook URL may carry a token in its query string.
	redErr, _ := redact.Redact([]byte(err.Error()))
	d.appendEvent(n.SessionID, session.EventNotifyFailed, map[string]any{
		"backend":     be.Name(),
		"attempts":    attempts,
		"event":       n.Event,
		"reason_kind": n.ReasonKind,
		"error":       string(redErr),
	})
}

// attempt runs the send/retry loop for one backend. Total sends = maxAttempts
// (1 + Retries); backoff before retry k is backoffFor(k) = 1s/4s/16s. Retries
// only on retryable errors (transport, 5xx); a 4xx returns immediately.
func (d *Dispatcher) attempt(be Backend, n Notification) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
		err := be.Send(ctx, n)
		cancel()
		if err == nil {
			return attempt, nil
		}
		lastErr = err
		if !retryable(err) || attempt == d.maxAttempts {
			return attempt, lastErr
		}
		if !d.backoff(backoffFor(attempt)) {
			// Shutdown during backoff — report the failure so far.
			return attempt, lastErr
		}
	}
	return d.maxAttempts, lastErr
}

// backoffFor returns the wait before the (attempt+1)th send after the
// attempt-th send failed: 1s, 4s, 16s, capped at 16s beyond the table.
func backoffFor(attempt int) time.Duration {
	base := time.Second
	switch attempt {
	case 1:
		return base
	case 2:
		return 4 * base
	default:
		return 16 * base
	}
}

// backoff waits dur on the injected clock, returning false if the dispatcher is
// shutting down. It is written as a recompute-against-absolute-deadline loop so
// a clock already advanced past the deadline returns immediately, and it fires
// afterArm right after registering each timer waiter so a FakeClock test can
// synchronize before advancing. Under a real clock afterArm is nil and this is
// a single After wait.
func (d *Dispatcher) backoff(dur time.Duration) bool {
	deadline := d.clk.Now().Add(dur)
	for {
		wait := deadline.Sub(d.clk.Now())
		if wait <= 0 {
			return true
		}
		timer := d.clk.After(wait)
		if d.afterArm != nil {
			d.afterArm()
		}
		select {
		case <-timer:
		case <-d.done:
			return false
		}
	}
}

// resolveSession fetches the session's friendly name and cwd for the payload.
// A session deleted between blocking and delivery is not fatal: we degrade to
// the session ID as the name and an empty cwd and still send, rather than drop
// a real block on a benign race.
func (d *Dispatcher) resolveSession(sessionID string) (name, cwd string) {
	sess, err := d.store.GetSession(d.ctx, sessionID)
	if err != nil {
		d.log.Debug("notify: session lookup failed, sending degraded", "session_id", sessionID, "err", err)
		return sessionID, ""
	}
	return sess.Name, sess.Cwd
}

// buildNotification assembles and redacts the Notification. Both Summary and
// Detail pass redact.Redact (§8.5 — corral's most important redaction call
// site); the union of rule names found is stashed on the Notification for the
// notify.sent event so an egressing redaction is never silent.
func (d *Dispatcher) buildNotification(j job, name, cwd string) Notification {
	hint := fmt.Sprintf("corral answer %s \"1\"", name)
	rawDetail := buildDetail(j.reason.Summary, name, cwd)

	redSummary, sm := redact.Redact([]byte(j.reason.Summary))
	redDetail, dm := redact.Redact([]byte(rawDetail))

	rules := map[string]struct{}{}
	for _, m := range sm {
		rules[m.Rule] = struct{}{}
	}
	for _, m := range dm {
		rules[m.Rule] = struct{}{}
	}
	names := make([]string, 0, len(rules))
	for r := range rules {
		names = append(names, r)
	}
	sort.Strings(names)

	return Notification{
		SessionID:   j.sessionID,
		SessionName: name,
		Cwd:         cwd,
		Event:       j.event,
		ReasonKind:  j.reason.Kind,
		Summary:     string(redSummary),
		Detail:      string(redDetail),
		Hint:        hint,
		At:          j.at,
		redactions:  names,
	}
}

// buildDetail renders the multi-line body (§8.5). The summary here is the raw
// (pre-redaction) summary; buildNotification redacts the whole result.
func buildDetail(summary, name, cwd string) string {
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n")
	b.WriteString("repo: " + cwd + "\n")
	b.WriteString(fmt.Sprintf("answer: corral answer %s \"1\"\n", name))
	b.WriteString("attach: corral attach " + name + "\n")
	return b.String()
}

// appendEvent writes a notify.* event, marshaling data best-effort. A store
// write failure is logged and swallowed — the worker goroutine must not die on
// a transient DB error, and a missing event is strictly less bad than a hung
// notifier.
func (d *Dispatcher) appendEvent(sessionID string, kind session.EventKind, data map[string]any) {
	b, err := json.Marshal(data)
	if err != nil {
		b = []byte("{}")
	}
	// context.Background(), not d.ctx: the events table is the durable record
	// of a notification's fate, so a notify.failed produced while a shutdown
	// aborts an in-flight Send must still persist. The write is local and fast;
	// it cannot be the thing that hangs Close.
	if _, err := d.store.AppendEvent(context.Background(), sessionID, kind, string(b)); err != nil {
		d.log.Warn("notify: appending event failed", "kind", string(kind), "session_id", sessionID, "err", err)
	}
}
