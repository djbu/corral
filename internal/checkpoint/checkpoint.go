// Package checkpoint defines the seam M3 sits behind (design doc §2,
// decision 9): a Checkpointer that durably records enough about a live
// session to bring it back later and stops the live process, and that can
// relaunch from what it recorded. M1 ships only ResumeCheckpointer, whose
// whole restart strategy is "SIGTERM the process group, wait, SIGKILL if
// still alive, then `claude --resume <id>`" — no turn-boundary verification
// exists yet (that needs hooks or stream-json parsing, neither of which M1
// has). M3 replaces the body of Checkpoint/Restore, not their callers.
package checkpoint

import (
	"context"
	"time"

	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// Token is what Checkpoint returns and Restore consumes: enough to relaunch
// a session later. TurnBoundaryVerified is always false in M1 — it exists
// so M3 can start reporting true once it can actually detect a flushed
// turn boundary, without changing this type's shape.
type Token struct {
	ClaudeSessionID      string
	TurnBoundaryVerified bool
	At                   time.Time
}

// Checkpointer is the seam between "a session needs to stop" (daemon
// shutdown, or recovery reaping an orphan) and "a session can be relaunched
// later". M1's ResumeCheckpointer is the only implementation.
type Checkpointer interface {
	// Checkpoint durably records enough to bring s back later, then stops
	// the live process. M3 adds flush-verified turn-boundary waiting; M1
	// always returns TurnBoundaryVerified: false.
	Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) (Token, error)
	// Restore relaunches from a persisted session record. M1 builds a
	// Spec with --resume <claude_session_id>.
	Restore(ctx context.Context, rec session.Session) (session.Spec, error)
	// Resumable reports whether Restore can succeed for rec, and if not,
	// why (for the session.unresumable event's data).
	Resumable(rec session.Session) (bool, string)
}
