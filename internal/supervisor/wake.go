package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

// Wake brings a reaped/stopped-but-resumable session (idle-reaped by
// CheckpointIdle, or otherwise stopped with a resumable transcript) back to
// life on demand — the deferred half of design doc's "reaped session is
// stopped-but-resumable, woken only on demand" (see CheckpointIdle's doc
// comment). It never auto-wakes on an answer or hook; callers are always an
// explicit API/CLI request.
//
// desired_state=running is persisted BEFORE Restore/Spawn are attempted
// (mirroring Kill's crash-safety ordering, just inverted): if the daemon
// crashes mid-wake, a restart's Recover sees desired_state=running and
// retries the resume itself rather than leaving the session stranded at
// desired_state=stopped.
func (r *Registry) Wake(ctx context.Context, idOrName string) (*session.Session, error) {
	if ls, ok := r.Get(idOrName); ok {
		// Already live: waking a running session is a no-op success, not an
		// error — a client racing a wake against an idle-reap losing the
		// race should see the same live session it asked for.
		return r.store.GetSession(ctx, ls.SessionID)
	}

	rec, err := r.store.GetSession(ctx, idOrName)
	if errors.Is(err, store.ErrNotFound) {
		rec, err = r.store.GetSessionByName(ctx, idOrName)
	}
	if err != nil {
		return nil, fmt.Errorf("supervisor: waking %s: %w", idOrName, err)
	}

	if ok, why := r.checkpointer.Resumable(*rec); !ok {
		return nil, fmt.Errorf("%w: %s: %s", ErrNotResumable, rec.ID, why)
	}

	// Name-collision pre-check. GetSessionByName can't be used here: the
	// partial unique index sessions_name_active only enforces uniqueness
	// among non-terminal rows, so rec.Name may currently be shared by rec
	// itself (terminal) and a different, active session — and
	// GetSessionByName's unordered `WHERE name = ?` may return either one,
	// including rec itself, which would silently defeat this check. Scan
	// instead for any other, non-terminal row with the same name.
	others, err := r.store.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("supervisor: waking %s: listing sessions for name check: %w", rec.ID, err)
	}
	for _, other := range others {
		if other.ID == rec.ID || other.Name != rec.Name {
			continue
		}
		if other.DesiredState == session.DesiredStopped && (other.Status == session.StatusExited || other.Status == session.StatusFailed) {
			continue // other is terminal too; no live claim on the name
		}
		return nil, fmt.Errorf("%w: cannot wake %s: name %q held by active session %s", ErrNameTaken, rec.ID, rec.Name, other.ID)
	}

	if _, err := r.store.UpdateSession(ctx, rec.ID, func(s *session.Session) {
		s.DesiredState = session.DesiredRunning
	}); err != nil {
		return nil, fmt.Errorf("supervisor: marking %s running: %w", rec.ID, err)
	}

	spec, err := r.checkpointer.Restore(ctx, *rec)
	if err != nil {
		return nil, fmt.Errorf("supervisor: waking %s: Restore: %w", rec.ID, err)
	}

	count, err := r.resumeSpawn(ctx, spec, rec.ID)
	if err != nil {
		return nil, err
	}

	r.appendEvent(ctx, rec.ID, session.EventSessionResumed, map[string]any{"resume_count": count, "reason": "wake"})

	return r.store.GetSession(ctx, rec.ID)
}
