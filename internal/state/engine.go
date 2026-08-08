// Package state defines the seam M2 sits behind (design doc §2, decision
// 9): an Engine that turns hook events and lifecycle transitions into the
// AgentState `corral ls` renders. M1 ships only NoopEngine, which derives
// AgentState purely from session.Status — no hooks exist yet to drive
// anything richer. Nothing in M1 calls OnHookEvent; supervisor calls
// OnLifecycle for every spawn/exit/attach/detach so that M2 can swap the
// implementation without touching any caller.
package state

import (
	"context"
	"errors"

	"github.com/danielbecerra/corral/internal/hookrelay"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

// HookEvent is M2's hook-relay entry-point payload: a type alias (not a
// wrapper) for hookrelay.HookPayload, so the Engine interface's method
// signature stays textually identical to M1's placeholder while callers now
// get the real decoded payload.
type HookEvent = hookrelay.HookPayload

// ErrQueueFull is returned by OnHookEvent when the per-session ingest queue
// is full. The caller (the hooks HTTP handler) treats this as non-fatal: it
// records a hook.dropped event and still returns HTTP 200 to the relay —
// corral must never perturb the agent process over a full queue. The queue
// itself arrives in a later step.
var ErrQueueFull = errors.New("state: hook ingest queue full")

// Engine is the seam between session lifecycle and the AgentState rendered
// by `corral ls`. M1's NoopEngine is the only implementation; M2 replaces
// it with a hook-driven one behind this same interface.
type Engine interface {
	// OnHookEvent is M2's entry point; M1 never calls it.
	OnHookEvent(ctx context.Context, sessionID string, ev HookEvent) error
	// OnLifecycle is called by supervisor for every spawn/exit/attach/detach
	// transition in M1 — the same call site that writes the session's
	// events row, so M2 only needs to swap the Engine implementation.
	OnLifecycle(ctx context.Context, sessionID string, kind session.EventKind, data any) error
	// State returns what `corral ls` renders for sessionID. M1's
	// NoopEngine derives it purely from session.Status; M2 overrides it
	// with a hook-driven working/blocked/idle rendering.
	State(sessionID string) session.AgentState
}

// NoopEngine is M1's Engine: OnHookEvent and OnLifecycle are no-ops (the
// events row itself is written by the caller, not by Engine), and State is
// a pure function of the session's persisted Status.
type NoopEngine struct {
	store *store.Store
}

// New returns a NoopEngine backed by st, used only by State to look up a
// session's current Status.
func New(st *store.Store) *NoopEngine {
	return &NoopEngine{store: st}
}

// OnHookEvent is a no-op in M1; NoopEngine implements it only to satisfy
// Engine, and nothing calls it yet.
func (e *NoopEngine) OnHookEvent(ctx context.Context, sessionID string, ev HookEvent) error {
	return nil
}

// OnLifecycle is a no-op in M1: the caller (supervisor) is responsible for
// writing the events row itself; NoopEngine has nothing further to do with
// the transition.
func (e *NoopEngine) OnLifecycle(ctx context.Context, sessionID string, kind session.EventKind, data any) error {
	return nil
}

// State derives an AgentState purely from the session's persisted Status
// (design doc §6.4, §9.2): M1 has no hook data to do anything richer. If
// sessionID can't be found at all, State reports AgentFailed rather than
// erroring, since the interface has no error return.
func (e *NoopEngine) State(sessionID string) session.AgentState {
	sess, err := e.store.GetSession(context.Background(), sessionID)
	if err != nil {
		return session.AgentFailed
	}
	return statusToAgentState(sess.Status)
}

func statusToAgentState(status session.Status) session.AgentState {
	switch status {
	case session.StatusStarting:
		return session.AgentStarting
	case session.StatusRunning, session.StatusStopping:
		return session.AgentRunning
	case session.StatusExited:
		return session.AgentExited
	case session.StatusFailed:
		return session.AgentFailed
	default:
		return session.AgentFailed
	}
}
