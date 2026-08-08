// Package supervisor owns session process lifecycle: spawning under a PTY
// (spawn.go, already implemented), the live-session registry (this file),
// and startup recovery (recover.go). Registry is the concrete type behind
// daemon.LiveSessionLister.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/danielbecerra/corral/internal/claude/settings"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/proto"
	"github.com/danielbecerra/corral/internal/screen"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// Checkpointer is the subset of checkpoint.Checkpointer this package needs.
// It is declared locally (rather than importing internal/checkpoint) because
// checkpoint.ResumeCheckpointer.Checkpoint takes a *supervisor.LiveSession —
// checkpoint already imports supervisor, so supervisor importing checkpoint
// back would be a cycle. The daemon package, which imports both, supplies an
// adapter satisfying this interface around its *checkpoint.ResumeCheckpointer
// (see daemon/daemon.go's checkpointerAdapter).
type Checkpointer interface {
	// Checkpoint stops s's process group and persists its terminal state
	// and claude_session_id. The token checkpoint.Checkpointer.Checkpoint
	// returns is dropped by the adapter — nothing in this package needs it.
	Checkpoint(ctx context.Context, s *LiveSession, reason string) error
	Restore(ctx context.Context, rec session.Session) (session.Spec, error)
	Resumable(rec session.Session) (bool, string)
}

// Config bundles Registry's spawn-time configuration: everything BuildEnv,
// settings.Pin, and the output log need that isn't per-request.
type Config struct {
	StateDir          string
	SockPath          string
	SettingSources    string
	EnvPassthrough    []string
	Term              string
	OutputLogMaxBytes int64
	EnvSnapshot       map[string]string
	CorralVersion     string
	APIVersion        int
	// RecoveryGrace is the SIGTERM-to-SIGKILL escalation window Recover
	// uses when it finds a live orphan. <= 0 means recover.go's own
	// 5-second default.
	RecoveryGrace time.Duration

	// PingInterval/PingTimeout are attach.go's daemon-side keepalive
	// settings (design doc §5.2, config.Attach.PingInterval/PingTimeout).
	// <= 0 falls back to attach.go's own 15s/45s defaults — every existing
	// test that builds a zero-value Config still gets a working keepalive
	// rather than a busy-loop or a disabled ticker.
	PingInterval time.Duration
	PingTimeout  time.Duration

	// RelayCommand is the shell-safe command prefix (absolute corral binary
	// path + " hook-relay") the pinned settings.json's hooks{} block
	// invokes for every registered event (Amendment A.2). Resolved once at
	// daemon startup (daemon.go's resolveRelayCommand) — never re-resolved
	// per spawn.
	RelayCommand string
	// ClaudeHome is ~/.claude (or a fake HOME in tests) — Spawn reads
	// <ClaudeHome>/settings.json best-effort to detect and log any
	// pre-existing ("foreign") hooks a user configured outside corral.
	// Read-only: corral must never write to ClaudeHome.
	ClaudeHome string
}

// Registry is the concrete, in-process live-session tracker: it implements
// daemon.LiveSessionLister via ListLive, and owns Spawn/Kill/Get/List — the
// whole of step 9's supervisor surface. Keyed by session ID; a second map
// resolves session name -> ID so Get/Kill can take either (design doc §9.2:
// "{idOrName}").
type Registry struct {
	store        *store.Store
	engine       state.Engine
	checkpointer Checkpointer
	clock        clock.Clock
	cfg          Config
	log          *slog.Logger

	mu    sync.Mutex
	live  map[string]*LiveSession // keyed by session ID
	names map[string]string       // name -> ID

	// activityMu/lastActivityTouch throttle touchActivity's DB writes
	// (below) — kept separate from mu, which guards live/names, so a slow
	// activity UPDATE never blocks unrelated registry operations.
	activityMu        sync.Mutex
	lastActivityTouch map[string]int64 // session id -> unix-ms of last DB touch
}

// activityTouchIntervalMs throttles touchActivity so a burst of per-keystroke
// PTY writes bumps last_activity_ms in the DB at most this often per session.
const activityTouchIntervalMs = 5000

// New returns a Registry with no live sessions.
func New(st *store.Store, engine state.Engine, checkpointer Checkpointer, clk clock.Clock, cfg Config, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		store:             st,
		engine:            engine,
		checkpointer:      checkpointer,
		clock:             clk,
		cfg:               cfg,
		log:               log,
		live:              make(map[string]*LiveSession),
		names:             make(map[string]string),
		lastActivityTouch: map[string]int64{},
	}
}

// touchActivity bumps the session's last_activity_ms, throttled to at most
// once per activityTouchIntervalMs so per-keystroke PTY writes don't hammer
// the DB. Best-effort: errors are logged, never propagated to the input
// path. Callers must pass the session's HUMAN/EXTERNAL activity only — see
// WriteInput, Attach, and state.persistHeartbeat's call sites.
func (r *Registry) touchActivity(id string) {
	nowMs := r.clock.Now().UnixMilli()
	r.activityMu.Lock()
	last := r.lastActivityTouch[id]
	if nowMs-last < activityTouchIntervalMs {
		r.activityMu.Unlock()
		return
	}
	r.lastActivityTouch[id] = nowMs
	r.activityMu.Unlock()
	if err := r.store.TouchActivity(context.Background(), id, nowMs); err != nil {
		r.log.Warn("supervisor: touching activity", "session_id", id, "err", err)
	}
}

// ListLive implements daemon.LiveSessionLister.
func (r *Registry) ListLive() []*LiveSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*LiveSession, 0, len(r.live))
	for _, ls := range r.live {
		out = append(out, ls)
	}
	return out
}

// LiveIDs returns the session IDs of every currently-live session. Used by
// the idle reaper to enumerate reap candidates without copying LiveSession
// handles out of the registry.
func (r *Registry) LiveIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.live))
	for id := range r.live {
		ids = append(ids, id)
	}
	return ids
}

// List is an exported alias for ListLive, for callers outside the
// daemon.LiveSessionLister seam that would otherwise have no reason to know
// that interface's method name.
func (r *Registry) List() []*LiveSession {
	return r.ListLive()
}

// Get resolves idOrName against the live registry: first as a session ID,
// then as a name. It only ever reports sessions this process itself spawned
// — it is not a substitute for store.GetSession/GetSessionByName, which is
// what the API's read endpoints use for the full (including non-live)
// picture.
func (r *Registry) Get(idOrName string) (*LiveSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ls, ok := r.live[idOrName]; ok {
		return ls, true
	}
	if id, ok := r.names[idOrName]; ok {
		ls, ok := r.live[id]
		return ls, ok
	}
	return nil, false
}

func (r *Registry) register(ls *LiveSession, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[ls.SessionID] = ls
	r.names[name] = ls.SessionID
}

func (r *Registry) deregister(id, name string) {
	r.mu.Lock()
	delete(r.live, id)
	delete(r.names, name)
	r.mu.Unlock()
	// Drop the activity-throttle entry so lastActivityTouch doesn't grow
	// unboundedly over the daemon's lifetime as sessions come and go. Uses
	// its own lock (never held together with r.mu).
	r.activityMu.Lock()
	delete(r.lastActivityTouch, id)
	r.activityMu.Unlock()
}

// SecretFor returns the in-memory hook-auth secret for a live session.
// ok is false if no live session with that id exists.
func (r *Registry) SecretFor(sessionID string) (secret string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ls, ok := r.live[sessionID]
	if !ok {
		return "", false
	}
	return ls.Secret, true
}

// normalizeSize mirrors Spawn's own default-40x120-when-either-is-zero rule
// (design doc §7.1) so the Screen this package creates before calling Spawn
// is sized identically to the PTY Spawn actually starts.
// envKeyNames returns env's keys only (never values), for the store's
// env_keys_json column (session.Session.EnvKeys doc comment: "names only,
// never values") — a deliberate redaction so the DB never holds secrets
// that rode in on the passthrough allowlist.
func envKeyNames(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func normalizeSize(rows, cols uint16) (uint16, uint16) {
	if rows == 0 || cols == 0 {
		return 40, 120
	}
	return rows, cols
}

// Spawn starts spec's process under a PTY and wires it into the registry.
// The caller (the POST /v1/sessions handler for a fresh session, or
// recover.go for a resumed one) is responsible for the session's store row
// already existing before Spawn is called — Spawn only ever updates an
// existing row (via store.UpdateSession) with the pid/pgid/proc_start_ns/
// rows/cols/status a successful (or failed) spawn observes; it never
// inserts one.
//
// ctx is used only for the synchronous work below; the reader, reply-pump,
// and reaper goroutines Spawn starts outlive any request context and use
// context.Background() for their own store/event writes.
func (r *Registry) Spawn(ctx context.Context, spec session.Spec) (*session.Session, error) {
	rows, cols := normalizeSize(spec.Rows, spec.Cols)
	spec.Rows, spec.Cols = rows, cols

	secret, err := newSessionSecret()
	if err != nil {
		return nil, fmt.Errorf("supervisor: generating session secret for %s: %w", spec.ID, err)
	}

	foreignHooks := settings.ForeignHookEvents(r.cfg.ClaudeHome)
	if len(foreignHooks) > 0 {
		r.log.Info("supervisor: foreign hooks present in supervised session", "session_id", spec.ID, "events", foreignHooks)
	}

	pinned, err := settings.Pin(r.cfg.StateDir, spec, r.clock, r.cfg.CorralVersion, r.cfg.APIVersion, r.cfg.RelayCommand, foreignHooks)
	if err != nil {
		return nil, fmt.Errorf("supervisor: pinning settings for %s: %w", spec.ID, err)
	}
	spec.SettingsPath = pinned.SettingsPath
	if spec.SettingSources == "" {
		spec.SettingSources = r.cfg.SettingSources
	}
	spec.Env = BuildEnv(spec, r.cfg.EnvSnapshot, r.cfg.EnvPassthrough, r.cfg.Term, r.cfg.SockPath, secret)

	scr := screen.New(int(rows), int(cols), r.log)
	if ol, err := screen.OpenOutputLog(pinned.OutputLogPath, r.cfg.OutputLogMaxBytes, r.log); err != nil {
		r.log.Warn("supervisor: opening output log", "session_id", spec.ID, "err", err)
	} else {
		scr.SetOutputLog(ol)
	}

	master, cmd, info, err := Spawn(spec)
	if err != nil {
		scr.Close()
		if _, uerr := r.store.UpdateSession(ctx, spec.ID, func(sess *session.Session) {
			sess.Status = session.StatusFailed
		}); uerr != nil {
			r.log.Error("supervisor: recording spawn failure", "session_id", spec.ID, "err", uerr)
		}
		r.appendEvent(ctx, spec.ID, session.EventSessionSpawnFailed, map[string]any{"error": err.Error()})
		return nil, fmt.Errorf("supervisor: spawning %s: %w", spec.ID, err)
	}

	ls := &LiveSession{
		SessionID: spec.ID,
		PGID:      info.PGID,
		PTYMaster: master,
		Cmd:       cmd,
		Screen:    scr,
		Secret:    secret,
	}
	r.register(ls, spec.Name)

	startedAtMs := r.clock.Now().UnixMilli()
	updated, err := r.store.UpdateSession(ctx, spec.ID, func(sess *session.Session) {
		sess.ClaudeSessionID = spec.ID
		sess.PID = info.PID
		sess.PGID = info.PGID
		sess.ProcStartNs = info.ProcStartNs
		sess.Rows = int(info.Rows)
		sess.Cols = int(info.Cols)
		sess.Status = session.StatusRunning
		sess.StartedAtMs = &startedAtMs
		sess.Argv = BuildArgv(spec)
		sess.EnvKeys = envKeyNames(spec.Env)
	})
	if err != nil {
		// The process is running but we couldn't record it — leaving a
		// live, untracked child around is worse than killing it and
		// surfacing the error.
		r.deregister(spec.ID, spec.Name)
		_ = syscallKillGroupBestEffort(info.PGID)
		scr.Close()
		_ = master.Close()
		return nil, fmt.Errorf("supervisor: persisting spawn of %s: %w", spec.ID, err)
	}

	r.appendEvent(ctx, spec.ID, session.EventSessionSpawned, map[string]any{"pid": info.PID, "pgid": info.PGID})
	if err := r.engine.OnLifecycle(ctx, spec.ID, session.EventSessionSpawned, nil); err != nil {
		r.log.Warn("supervisor: engine.OnLifecycle spawned", "session_id", spec.ID, "err", err)
	}

	r.runGoroutines(ls, spec.ID, spec.Name)

	return updated, nil
}

// syscallKillGroupBestEffort is used only on the persist-after-spawn failure
// path above, which is expected to be vanishingly rare (a store write
// failing immediately after a successful spawn). It intentionally ignores
// its own error: there is nothing more constructive to do with it than log,
// and the caller already logs the outer error.
func syscallKillGroupBestEffort(pgid int) error {
	if pgid <= 1 {
		return nil
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// runGoroutines starts the three goroutines a live session needs for the
// rest of its life (design doc §7.3/§9): a PTY-reader feeding Screen, a
// reply-pump writing the emulator's device-query answers back to the PTY,
// and a reaper blocking on cmd.Wait() to observe the child's own exit.
func (r *Registry) runGoroutines(ls *LiveSession, id, name string) {
	readerDone := make(chan struct{})
	repliesDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := ls.PTYMaster.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				ls.Screen.Feed(cp)
			}
			if err != nil {
				// EOF (darwin) or EIO (linux) on the child closing its end
				// of the PTY is the normal way this loop ends, not an
				// error worth logging.
				return
			}
		}
	}()

	go func() {
		defer close(repliesDone)
		_, _ = io.Copy(ls.PTYMaster, ls.Screen.Replies())
	}()

	go func() {
		_ = ls.Cmd.Wait()

		// Unblock the reader (master.Read returns an error once the write
		// end is gone) and the reply-pump (Screen.Close closes the
		// emulator's input pipe, which is what pumpReplies blocks on) in
		// that order, then wait for both before touching the store — see
		// live.go/screen.go's Close doc comments for why this order avoids
		// a deadlock between the two.
		_ = ls.PTYMaster.Close()
		<-readerDone
		ls.Screen.Close()
		<-repliesDone

		// If a client is attached when the child exits, tell it before
		// tearing anything else down (design doc §5.2's 0x83 Exit frame) —
		// attach.go's own reader loop will observe the connection error
		// this write's subsequent close triggers and unwind on its own.
		r.mu.Lock()
		att := ls.Attachment
		r.mu.Unlock()
		if att != nil {
			exitCode, exitSignal := exitInfo(ls.Cmd.ProcessState)
			_ = att.writeFrame(proto.Frame{Type: proto.TypeExit, Payload: mustEncode(proto.Exit{
				ExitCode: exitCode,
				Signal:   exitSignal,
				Reason:   "child_exited",
			})})
			// Mark why this attachment is about to see its connection
			// error out, so attach.go's reader loop knows not to log a
			// session.client_crashed event for what was actually a normal
			// (or crashing, but attributable-to-the-child) session exit.
			att.forceClose("session_exited")
		}

		r.deregister(id, name)
		r.reap(ls, id)
	}()
}

// reap runs after cmd.Wait() returns and both of a session's I/O goroutines
// have finished. It uses context.Background(): the request context that
// triggered Spawn is long gone by the time a child actually exits.
func (r *Registry) reap(ls *LiveSession, id string) {
	ctx := context.Background()

	rec, err := r.store.GetSession(ctx, id)
	if err != nil {
		r.log.Error("supervisor: reap: loading session", "session_id", id, "err", err)
		return
	}

	// A checkpoint (Kill or daemon shutdown) always writes Status=stopping
	// strictly before it signals the process, and Status=exited once it has
	// finished recording terminal state — either value observed here means
	// this exit was checkpoint-initiated and every write reap would
	// otherwise make (exit_code/exit_signal/status/desired_state/event) has
	// already been made by that checkpoint. A self-exit leaves Status at
	// "running" until this function is the one to change it.
	if rec.Status == session.StatusStopping || rec.Status == session.StatusExited {
		return
	}

	exitCode, exitSignal := exitInfo(ls.Cmd.ProcessState)
	endedAtMs := r.clock.Now().UnixMilli()
	updated, err := r.store.UpdateSession(ctx, id, func(sess *session.Session) {
		sess.Status = session.StatusExited
		sess.DesiredState = session.DesiredStopped
		sess.ExitCode = exitCode
		sess.ExitSignal = exitSignal
		sess.EndedAtMs = &endedAtMs
	})
	if err != nil {
		r.log.Error("supervisor: reap: recording self-exit", "session_id", id, "err", err)
		return
	}

	data := map[string]any{"exit_signal": exitSignal}
	if exitCode != nil {
		data["exit_code"] = *exitCode
	}
	r.appendEvent(ctx, id, session.EventSessionExited, data)
	if err := r.engine.OnLifecycle(ctx, id, session.EventSessionExited, nil); err != nil {
		r.log.Warn("supervisor: engine.OnLifecycle exited", "session_id", id, "err", err)
	}
	_ = updated
}

// signalNames maps the handful of signals a supervised claude child can
// plausibly die from to the name events/API responses record. Anything
// else falls back to its numeric form ("signal N") rather than guessing.
var signalNames = map[syscall.Signal]string{
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGABRT: "SIGABRT",
}

// exitInfo extracts an exit code (nil if the process was killed by a
// signal rather than exiting normally) and a signal name (empty if it
// exited normally) from a completed process's ProcessState.
func exitInfo(ps *os.ProcessState) (*int, string) {
	if ps == nil {
		return nil, ""
	}
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		sig := status.Signal()
		name, ok := signalNames[sig]
		if !ok {
			name = fmt.Sprintf("signal %d", int(sig))
		}
		return nil, name
	}
	code := ps.ExitCode()
	return &code, ""
}

// appendEvent marshals data to JSON (best-effort — a marshal failure logs
// and falls back to "{}" rather than losing the event entirely) and
// appends it via the store.
func (r *Registry) appendEvent(ctx context.Context, sessionID string, kind session.EventKind, data map[string]any) {
	payload := "{}"
	if len(data) > 0 {
		b, err := json.Marshal(data)
		if err != nil {
			r.log.Warn("supervisor: marshaling event data", "kind", kind, "err", err)
		} else {
			payload = string(b)
		}
	}
	if _, err := r.store.AppendEvent(ctx, sessionID, kind, payload); err != nil {
		r.log.Error("supervisor: appending event", "kind", kind, "session_id", sessionID, "err", err)
	}
}

// Kill marks id (an ID or name) as desired_state=stopped and stops its
// process group via the checkpointer's SIGTERM/grace/SIGKILL path (design
// doc §9.2's DELETE /v1/sessions/{idOrName}). desired_state is persisted
// before signalling — see this method's own ordering below — so that if
// the daemon crashes mid-kill, recovery (§3.5) never mistakes an
// intentionally-stopped session for one to resume.
func (r *Registry) Kill(ctx context.Context, idOrName string, grace *time.Duration) (*session.Session, error) {
	ls, ok := r.Get(idOrName)
	if !ok {
		return nil, fmt.Errorf("supervisor: %w: %s", ErrNotLive, idOrName)
	}

	if _, err := r.store.UpdateSession(ctx, ls.SessionID, func(sess *session.Session) {
		sess.DesiredState = session.DesiredStopped
	}); err != nil {
		return nil, fmt.Errorf("supervisor: marking %s stopped: %w", ls.SessionID, err)
	}

	cp := r.checkpointer
	if grace != nil {
		if wg, ok := cp.(interface {
			WithGrace(time.Duration) Checkpointer
		}); ok {
			cp = wg.WithGrace(*grace)
		}
	}
	if err := cp.Checkpoint(ctx, ls, "user_kill"); err != nil {
		return nil, fmt.Errorf("supervisor: killing %s: %w", ls.SessionID, err)
	}

	r.appendEvent(ctx, ls.SessionID, session.EventSessionKilled, nil)
	if err := r.engine.OnLifecycle(ctx, ls.SessionID, session.EventSessionKilled, nil); err != nil {
		r.log.Warn("supervisor: engine.OnLifecycle killed", "session_id", ls.SessionID, "err", err)
	}

	return r.store.GetSession(ctx, ls.SessionID)
}

// CheckpointIdle checkpoints a live session that the idle reaper has judged
// idle past state.idle_timeout (design doc m3 §3). Like Kill it persists
// desired_state=stopped BEFORE signalling, so a crash mid-reap never leaves a
// reaped session marked 'running' for recovery to resurrect — a reaped
// session is stopped-but-resumable, woken only on demand (a later step).
// idleFor is recorded on the session.idle_reaped event for observability.
func (r *Registry) CheckpointIdle(ctx context.Context, id string, idleFor time.Duration) (*session.Session, error) {
	ls, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("supervisor: %w: %s", ErrNotLive, id)
	}

	if _, err := r.store.UpdateSession(ctx, ls.SessionID, func(sess *session.Session) {
		sess.DesiredState = session.DesiredStopped
	}); err != nil {
		return nil, fmt.Errorf("supervisor: marking %s stopped: %w", ls.SessionID, err)
	}

	if err := r.checkpointer.Checkpoint(ctx, ls, "idle_reap"); err != nil {
		return nil, fmt.Errorf("supervisor: idle-reaping %s: %w", ls.SessionID, err)
	}

	r.appendEvent(ctx, ls.SessionID, session.EventSessionIdleReaped, map[string]any{
		"idle_ms": idleFor.Milliseconds(),
	})
	if err := r.engine.OnLifecycle(ctx, ls.SessionID, session.EventSessionIdleReaped, nil); err != nil {
		r.log.Warn("supervisor: engine.OnLifecycle idle_reaped", "session_id", ls.SessionID, "err", err)
	}

	return r.store.GetSession(ctx, ls.SessionID)
}

// WriteInput writes b to idOrName's live PTY master verbatim, as if a human
// had typed it (design doc §7). Callers are responsible for encoding the
// answer payload and for appending any resulting event; WriteInput does
// neither. r.mu is not held across the Write so a slow/blocked PTY write
// never stalls other registry operations.
func (r *Registry) WriteInput(ctx context.Context, idOrName string, b []byte) error {
	ls, ok := r.Get(idOrName)
	if !ok {
		return ErrNotLive
	}
	_, err := ls.PTYMaster.Write(b)
	if err != nil {
		return err
	}
	// Human/external input (design doc's activity-tracking step): bump
	// last_activity_ms via the resolved canonical session ID, never
	// idOrName, since idOrName may be a name rather than an ID.
	r.touchActivity(ls.SessionID)
	return nil
}

// ErrNotLive is returned by Kill when idOrName does not name a session this
// registry currently tracks as live.
var ErrNotLive = fmt.Errorf("supervisor: session is not live")
