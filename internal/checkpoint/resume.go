package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/danielbecerra/corral/internal/claude/sessions"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/procinfo"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// ResumeCheckpointer is M1's whole restart strategy (design doc §2, §3.5,
// §3.6): Checkpoint stops the process group (SIGTERM, wait Grace, SIGKILL)
// and persists claude_session_id; Restore rebuilds a Spec with --resume.
// M1 never learns a claude_session_id different from the corral session ID
// it assigned at spawn (design doc §0's correction: corral knows the
// session_id *before* the child starts), so Checkpoint always persists
// s.SessionID as the claude_session_id.
type ResumeCheckpointer struct {
	store *store.Store
	clock clock.Clock
	grace time.Duration

	// ClaudeHome is ~/.claude (or a fake HOME in tests) — the root
	// Resumable checks for a session transcript file under.
	ClaudeHome string
	// EnvSnapshot, EnvPassthrough, Term, SockPath mirror the inputs
	// supervisor.BuildEnv needs (design doc §7.2): Restore rebuilds a
	// session's environment from the daemon's frozen startup snapshot,
	// since only env var *names* are persisted (env_keys_json), never
	// values.
	EnvSnapshot    map[string]string
	EnvPassthrough []string
	Term           string
	SockPath       string
}

// NewResumeCheckpointer returns a ResumeCheckpointer. grace is the
// SIGTERM-to-SIGKILL escalation window (design doc §3.4/§3.6,
// daemon.shutdown_grace).
func NewResumeCheckpointer(st *store.Store, clk clock.Clock, grace time.Duration, claudeHome string, envSnapshot map[string]string, envPassthrough []string, term, sockPath string) *ResumeCheckpointer {
	return &ResumeCheckpointer{
		store:          st,
		clock:          clk,
		grace:          grace,
		ClaudeHome:     claudeHome,
		EnvSnapshot:    envSnapshot,
		EnvPassthrough: envPassthrough,
		Term:           term,
		SockPath:       sockPath,
	}
}

// WithGrace returns a shallow copy of c with a different SIGTERM-to-
// SIGKILL escalation window. The Checkpointer interface (design doc §2)
// takes no per-call grace parameter, so a caller that needs one for a
// single call (POST /v1/daemon/shutdown's optional "grace" override,
// DELETE /v1/sessions/{id}?grace=... in step 9/10) uses this rather than
// mutating the shared *ResumeCheckpointer.
func (c *ResumeCheckpointer) WithGrace(grace time.Duration) *ResumeCheckpointer {
	copy := *c
	copy.grace = grace
	return &copy
}

// Checkpoint stops s's process group (SIGTERM, wait c.grace via the
// injected Clock, then SIGKILL regardless of whether SIGTERM already
// succeeded — ESRCH from either signal, meaning the group is already gone,
// is not an error) and persists claude_session_id on the session row.
// TurnBoundaryVerified is always false: M1 has no way to detect a flushed
// turn boundary (that's M3's job).
func (c *ResumeCheckpointer) Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) (Token, error) {
	if _, err := c.store.UpdateSession(ctx, s.SessionID, func(sess *session.Session) {
		sess.Status = session.StatusStopping
	}); err != nil {
		return Token{}, fmt.Errorf("checkpoint: marking %s stopping: %w", s.SessionID, err)
	}

	exitSignal := "SIGTERM"
	if err := killGroupTolerant(s.PGID, syscall.SIGTERM); err != nil {
		return Token{}, fmt.Errorf("checkpoint: SIGTERM %s: %w", s.SessionID, err)
	}

	select {
	case <-ctx.Done():
	case <-c.clock.After(c.grace):
		exitSignal = "SIGKILL"
		if err := killGroupTolerant(s.PGID, syscall.SIGKILL); err != nil {
			return Token{}, fmt.Errorf("checkpoint: SIGKILL %s: %w", s.SessionID, err)
		}
	}

	now := c.clock.Now()
	endedAtMs := now.UnixMilli()
	if _, err := c.store.UpdateSession(ctx, s.SessionID, func(sess *session.Session) {
		sess.ClaudeSessionID = s.SessionID
		sess.Status = session.StatusExited
		sess.ExitSignal = exitSignal
		sess.EndedAtMs = &endedAtMs
	}); err != nil {
		return Token{}, fmt.Errorf("checkpoint: recording %s stopped: %w", s.SessionID, err)
	}

	return Token{ClaudeSessionID: s.SessionID, TurnBoundaryVerified: false, At: now}, nil
}

// killGroupTolerant calls procinfo.KillGroup, treating "the group is
// already gone" as success rather than an error. That is normally ESRCH
// (no such process), but on darwin a group emptied out between our
// SIGTERM and the SIGKILL escalation below can also surface as EPERM —
// kill(2)'s pgrp lookup can find zero live members yet still report
// EPERM rather than ESRCH for the now-empty group id, observed directly
// in this package's own tests (SIGKILL right after a SIGTERM that killed
// the sole process in the group). Either errno here means the same
// thing: nothing left to signal.
func killGroupTolerant(pgid int, sig syscall.Signal) error {
	err := procinfo.KillGroup(pgid, sig)
	if err != nil && (errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)) {
		return nil
	}
	return err
}

// Restore builds a Spec to relaunch rec with --resume <claude_session_id>.
// It reconstructs Env from the frozen daemon-start snapshot and passthrough
// list rather than from rec itself, since only env var names — never
// values — are ever persisted (design doc §6.2's env_keys_json comment).
func (c *ResumeCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	if rec.ClaudeSessionID == "" {
		return session.Spec{}, fmt.Errorf("checkpoint: restoring %s: no claude_session_id recorded", rec.ID)
	}
	spec := session.Spec{
		ID:             rec.ID,
		Name:           rec.Name,
		Mode:           rec.Mode,
		Cwd:            rec.Cwd,
		ClaudeBin:      rec.ClaudeBin,
		Model:          rec.Model,
		SettingSources: rec.SettingSources,
		SettingsPath:   rec.SettingsPath,
		Rows:           uint16(rec.Rows),
		Cols:           uint16(rec.Cols),
		ResumeFrom:     rec.ClaudeSessionID,
	}
	spec.Env = supervisor.BuildEnv(spec, c.EnvSnapshot, c.EnvPassthrough, c.Term, c.SockPath)
	return spec, nil
}

// Resumable reports whether Restore can succeed for rec: it needs a
// recorded claude_session_id, and the session's transcript file must
// actually exist under ClaudeHome (design doc: "M1 never calls
// internal/claude/sessions on the critical path" except here, as a
// verification-only check before attempting --resume).
func (c *ResumeCheckpointer) Resumable(rec session.Session) (bool, string) {
	if rec.ClaudeSessionID == "" {
		return false, "no claude_session_id recorded"
	}
	path := sessions.TranscriptPath(c.ClaudeHome, rec.Cwd, rec.ClaudeSessionID)
	if _, err := os.Stat(path); err != nil {
		return false, fmt.Sprintf("session transcript not found at %s", path)
	}
	return true, ""
}
