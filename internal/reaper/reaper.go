// Package reaper implements the idle reaper (design doc m3 §3): a background
// goroutine that checkpoints sessions which have gone idle with no human
// present, so they stop consuming resources (PTY, process group, screen
// buffer) but stay resumable on demand. It never kills a session outright —
// CheckpointIdle (internal/supervisor) persists desired_state=stopped before
// signalling, so a reaped session looks exactly like a user-initiated Kill to
// recovery: stopped-but-resumable, never resurrected on daemon restart.
// Waking a reaped session back up is a later step; this package only reaps.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/session"
)

// Supervisor is the subset of *supervisor.Registry the reaper needs.
// Declared locally (rather than importing internal/supervisor) so tests can
// fake it without real PTYs, and so this package stays a leaf of supervisor,
// not a cycle.
type Supervisor interface {
	LiveIDs() []string
	Attached(id string) bool
	CheckpointIdle(ctx context.Context, id string, idleFor time.Duration) (*session.Session, error)
}

// Store is the subset of *store.Store the reaper needs.
type Store interface {
	GetSession(ctx context.Context, id string) (*session.Session, error)
}

// Reaper owns the background reap-loop goroutine. All of its work happens on
// that one goroutine, started by Start and joined by Close.
type Reaper struct {
	sup         Supervisor
	store       Store
	clk         clock.Clock
	log         *slog.Logger
	idleTimeout time.Duration
	// templateTimeouts is a daemon-start snapshot of operator-owned policy.
	// A template may only shorten the global timeout; it can never turn a
	// globally enabled reaper into a weaker or disabled one.
	templateTimeouts map[string]time.Duration

	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a Reaper. log defaults to slog.Default(). idleTimeout <= 0
// disables idle reaping entirely (Start becomes a no-op) — see config.
// State.IdleTimeout's own doc comment for the config-level default.
func New(sup Supervisor, st Store, clk clock.Clock, log *slog.Logger, idleTimeout time.Duration, templateTimeouts ...map[string]time.Duration) *Reaper {
	if log == nil {
		log = slog.Default()
	}
	var timeouts map[string]time.Duration
	if len(templateTimeouts) > 0 {
		timeouts = templateTimeouts[0]
	}
	return &Reaper{
		sup:              sup,
		store:            st,
		clk:              clk,
		log:              log,
		idleTimeout:      idleTimeout,
		templateTimeouts: timeouts,
	}
}

// Start launches the reap loop unless idle reaping is disabled (idleTimeout
// <= 0), in which case it is a no-op and Close is likewise a safe no-op.
func (r *Reaper) Start() {
	if r.idleTimeout <= 0 {
		r.log.Debug("idle reaper disabled (state.idle_timeout=0)")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.run(ctx)
	r.log.Info("idle reaper started", "idle_timeout", r.idleTimeout, "interval", r.interval())
}

// Close stops the reap loop and waits for the goroutine to exit. Safe to call
// when Start was a no-op or was never called.
func (r *Reaper) Close() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

// interval is how often reapOnce runs: idleTimeout/4, capped to [5s, 1m] —
// frequent enough that a session is reaped soon after crossing idleTimeout,
// never so frequent that a short idle_timeout busy-loops the check.
func (r *Reaper) interval() time.Duration {
	timeout := r.idleTimeout
	for _, candidate := range r.templateTimeouts {
		if candidate > 0 && candidate < timeout {
			timeout = candidate
		}
	}
	iv := timeout / 4
	if iv > time.Minute {
		iv = time.Minute
	}
	if iv < 5*time.Second {
		iv = 5 * time.Second
	}
	return iv
}

// run is the tick loop. It exits (closing done) as soon as ctx is cancelled
// by Close.
func (r *Reaper) run(ctx context.Context) {
	defer close(r.done)
	ticker := r.clk.NewTicker(r.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			r.reapOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// reapOnce is the reap policy, run synchronously so tests can drive it
// directly without the ticker/goroutine. It enumerates live sessions and
// reaps survivors of the filter below SEQUENTIALLY — a reap storm is rare,
// and sequential reaping keeps SIGTERM/SIGKILL pressure sane and avoids
// concurrent checkpoint load on the store.
func (r *Reaper) reapOnce(ctx context.Context) {
	nowMs := r.clk.Now().UnixMilli()
	for _, id := range r.sup.LiveIDs() {
		if ctx.Err() != nil {
			return
		}

		sess, err := r.store.GetSession(ctx, id)
		if err != nil {
			r.log.Warn("reaper: reading session", "session_id", id, "err", err)
			continue
		}

		// Headless task-owned sessions are exempt from the idle reaper (m4.md §8.2):
		// their lifecycle authority is the orchestrator's per-task timeout, not idle
		// checkpointing. Hook-derived activity from a headless child is deliberately
		// NOT a reap signal — the exemption is by mode, not by silence.
		if sess.Mode == session.ModeHeadless {
			continue
		}

		// Reap ONLY idle sessions. agent_state==idle is a turn boundary (the
		// agent finished and is waiting), so checkpointing here never loses an
		// in-flight turn. working/blocked/unknown are deliberately left alone:
		// working is busy, blocked is awaiting an answer (killing it loses the
		// pending prompt), unknown means we can't tell — none are safe to reap.
		if sess.AgentState != session.AgentIdle {
			continue
		}

		// Never reap an attached session, regardless of timeout — a human is
		// present at the terminal.
		if r.sup.Attached(id) {
			continue
		}

		idleFor := time.Duration(nowMs-sess.LastActivityMs) * time.Millisecond
		if idleFor <= r.idleTimeoutFor(sess) {
			continue
		}

		if _, err := r.sup.CheckpointIdle(ctx, id, idleFor); err != nil {
			r.log.Warn("reaper: checkpointing idle session", "session_id", id, "err", err)
			continue
		}
		r.log.Info("idle-reaped session", "session_id", id, "idle", idleFor)
	}
}

// idleTimeoutFor returns the effective timeout for sess. The daemon-wide
// state.idle_timeout remains the hard maximum: a template can request an
// earlier checkpoint but never a later one. Unknown or stale names safely
// fall back to the global policy.
func (r *Reaper) idleTimeoutFor(sess *session.Session) time.Duration {
	timeout := r.idleTimeout
	if candidate := r.templateTimeouts[sess.Template]; candidate > 0 && candidate < timeout {
		return candidate
	}
	return timeout
}
