package supervisor

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/danielbecerra/corral/internal/procinfo"
	"github.com/danielbecerra/corral/internal/session"
)

// procChecker is the subset of internal/procinfo Recover needs, declared
// locally so tests in this package can inject a fake liveness table
// without spawning real processes (design doc §3.5's recovery table:
// "fake store rows x alive/dead x resumable/not").
type procChecker interface {
	Alive(pid int, startNs int64) bool
}

type realProcChecker struct{}

func (realProcChecker) Alive(pid int, startNs int64) bool { return procinfo.Alive(pid, startNs) }

// Recover runs design doc §3.5's startup recovery exactly: for every
// session row with desired_state="running" (never matched by name — always
// by the (pid, proc_start_ns) pair recorded at spawn time), a daemon
// restart has by definition lost the PTY fd, so a live child is always
// reaped and never re-adopted. Resumable rows are relaunched with
// --resume; everything else is marked unresumable and desired_state is
// flipped to "stopped" so a later restart doesn't keep retrying it.
func (r *Registry) Recover(ctx context.Context) error {
	return r.recover(ctx, realProcChecker{}, r.grace())
}

// grace is the SIGTERM-to-SIGKILL escalation window recovery uses for an
// orphaned process group, mirroring checkpoint.ResumeCheckpointer's own
// default grace (design doc §3.4/§3.6). It comes from Config since Recover
// has no per-call override (unlike Kill's ?grace= query param).
func (r *Registry) grace() time.Duration {
	if r.cfg.RecoveryGrace <= 0 {
		return 5 * time.Second
	}
	return r.cfg.RecoveryGrace
}

func (r *Registry) recover(ctx context.Context, pc procChecker, grace time.Duration) error {
	sessions, err := r.store.ListSessions(ctx)
	if err != nil {
		return err
	}

	for _, rec := range sessions {
		if rec.DesiredState != session.DesiredRunning {
			continue
		}
		r.recoverOne(ctx, pc, grace, rec)
	}
	return nil
}

func (r *Registry) recoverOne(ctx context.Context, pc procChecker, grace time.Duration, rec *session.Session) {
	if pc.Alive(rec.PID, rec.ProcStartNs) {
		r.log.Warn("supervisor: recovery found a live orphan; reaping (never re-adopting)",
			"session_id", rec.ID, "pid", rec.PID, "pgid", rec.PGID)
		r.reapOrphan(ctx, rec, grace)
		r.appendEvent(ctx, rec.ID, session.EventSessionOrphanReaped, map[string]any{"pid": rec.PID, "pgid": rec.PGID})
	}

	ok, why := r.checkpointer.Resumable(*rec)
	if !ok {
		if _, err := r.store.UpdateSession(ctx, rec.ID, func(sess *session.Session) {
			sess.Status = session.StatusFailed
			sess.DesiredState = session.DesiredStopped
		}); err != nil {
			r.log.Error("supervisor: recovery: marking unresumable", "session_id", rec.ID, "err", err)
		}
		r.appendEvent(ctx, rec.ID, session.EventSessionUnresumable, map[string]any{"why": why})
		return
	}

	spec, err := r.checkpointer.Restore(ctx, *rec)
	if err != nil {
		r.log.Error("supervisor: recovery: Restore", "session_id", rec.ID, "err", err)
		if _, uerr := r.store.UpdateSession(ctx, rec.ID, func(sess *session.Session) {
			sess.Status = session.StatusFailed
			sess.DesiredState = session.DesiredStopped
		}); uerr != nil {
			r.log.Error("supervisor: recovery: marking failed after Restore error", "session_id", rec.ID, "err", uerr)
		}
		r.appendEvent(ctx, rec.ID, session.EventSessionUnresumable, map[string]any{"why": err.Error()})
		return
	}

	count, err := r.resumeSpawn(ctx, spec, rec.ID)
	if err != nil {
		r.log.Error("supervisor: recovery: resume spawn", "session_id", rec.ID, "err", err)
		return
	}
	r.appendEvent(ctx, rec.ID, session.EventSessionResumed, map[string]any{"resume_count": count})
}

// resumeSpawn runs the shared tail of resuming an already-Restore'd session:
// spawn the process, bump resume_count. It does NOT emit session.resumed —
// each caller (recoverOne, Wake) owns that event so their payloads can differ.
func (r *Registry) resumeSpawn(ctx context.Context, spec session.Spec, id string) (int, error) {
	if _, err := r.Spawn(ctx, spec); err != nil {
		return 0, fmt.Errorf("supervisor: resume spawn %s: %w", id, err)
	}
	updated, err := r.store.UpdateSession(ctx, id, func(sess *session.Session) {
		sess.ResumeCount++
	})
	if err != nil {
		return 0, fmt.Errorf("supervisor: recording resume_count %s: %w", id, err)
	}
	return updated.ResumeCount, nil
}

// reapOrphan sends SIGTERM to rec's process group, waits up to grace, then
// SIGKILL — the same escalation checkpoint.Checkpointer uses, but run
// directly against procinfo since no *LiveSession exists for a process this
// daemon instance never spawned (it belongs to a previous, now-dead, daemon
// process). ESRCH/EPERM from either signal (the group is already gone) is
// not an error.
func (r *Registry) reapOrphan(ctx context.Context, rec *session.Session, grace time.Duration) {
	if err := killGroupTolerant(rec.PGID, syscall.SIGTERM); err != nil {
		r.log.Warn("supervisor: recovery: SIGTERM orphan", "session_id", rec.ID, "pgid", rec.PGID, "err", err)
	}

	select {
	case <-ctx.Done():
	case <-r.clock.After(grace):
	}

	if err := killGroupTolerant(rec.PGID, syscall.SIGKILL); err != nil {
		r.log.Warn("supervisor: recovery: SIGKILL orphan", "session_id", rec.ID, "pgid", rec.PGID, "err", err)
	}
}

// killGroupTolerant mirrors checkpoint.killGroupTolerant (unexported there,
// so duplicated rather than shared): ESRCH or EPERM from procinfo.KillGroup
// means "nothing left to signal", not a failure.
func killGroupTolerant(pgid int, sig syscall.Signal) error {
	err := procinfo.KillGroup(pgid, sig)
	if err != nil && (errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)) {
		return nil
	}
	return err
}
