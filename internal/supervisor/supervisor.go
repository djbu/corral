// Package supervisor owns session process lifecycle: spawning under a PTY
// (spawn.go, already implemented), the live-session registry (this file),
// and startup recovery (recover.go). Registry is the concrete type behind
// daemon.LiveSessionLister.
package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/djbu/corral/internal/claude/settings"
	"github.com/djbu/corral/internal/claude/streamjson"
	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/diskspace"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/proto"
	"github.com/djbu/corral/internal/screen"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
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
	// and claude_session_id. The bool reports whether the checkpoint
	// captured a completed turn boundary (checkpoint.Token.
	// TurnBoundaryVerified, forwarded by the adapter) — this package
	// surfaces it into the lifecycle event payload rather than dropping it.
	Checkpoint(ctx context.Context, s *LiveSession, reason string) (bool, error)
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
	// MinFreeBytes rejects every spawn before it can create settings, logs,
	// processes or spawn-side store updates. The caller's minimal attempt row
	// may already exist for audit. FreeBytes is injectable for deterministic
	// tests; nil uses diskspace.FreeBytes.
	MinFreeBytes int64
	FreeBytes    func(string) (int64, error)
	// MaxInteractiveSessions and MaxHeadlessTasks are independent daemon
	// admission caps. Zero values retain M8-compatible defaults for direct
	// unit-test construction; daemon startup supplies explicit operator policy.
	MaxInteractiveSessions int
	MaxHeadlessTasks       int
}

// ErrLowDisk identifies a spawn rejected by the state filesystem guard.
var ErrLowDisk = errors.New("supervisor: insufficient free disk space")

// ErrCapacity identifies a spawn rejected before it creates any process or
// spawn-side artifact because its mode has exhausted daemon capacity.
var ErrCapacity = errors.New("supervisor: capacity exhausted")

// CapacityError describes a rejected admission without exposing paths or
// credentials. It unwraps to ErrCapacity so API and orchestrator callers can
// make a stable policy decision without parsing its human-readable message.
type CapacityError struct {
	Mode  session.Mode
	Limit int
	InUse int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("%s: mode=%s limit=%d in_use=%d", ErrCapacity, e.Mode, e.Limit, e.InUse)
}

func (e *CapacityError) Unwrap() error { return ErrCapacity }

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
	// pending holds capacity reservations between admission and register.
	// It closes the race where two concurrent Spawn calls both observe one
	// remaining slot before either process enters live.
	pending map[string]bool // session ID -> headless

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
		pending:           make(map[string]bool),
		lastActivityTouch: map[string]int64{},
	}
}

func (r *Registry) interactiveLimit() int {
	if r.cfg.MaxInteractiveSessions <= 0 {
		return 16
	}
	return r.cfg.MaxInteractiveSessions
}

func (r *Registry) headlessLimit() int {
	if r.cfg.MaxHeadlessTasks <= 0 {
		return 4
	}
	return r.cfg.MaxHeadlessTasks
}

func (r *Registry) reserveCapacity(id string, mode session.Mode) error {
	headless := mode == session.ModeHeadless
	limit := r.interactiveLimit()
	if headless {
		limit = r.headlessLimit()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inUse := 0
	for _, ls := range r.live {
		if ls.Headless == headless {
			inUse++
		}
	}
	for _, pendingHeadless := range r.pending {
		if pendingHeadless == headless {
			inUse++
		}
	}
	if inUse >= limit {
		return &CapacityError{Mode: mode, Limit: limit, InUse: inUse}
	}
	r.pending[id] = headless
	return nil
}

func (r *Registry) releaseCapacity(id string) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
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
	delete(r.pending, ls.SessionID)
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
	if err := r.reserveCapacity(spec.ID, spec.Mode); err != nil {
		r.recordPreSpawnFailure(ctx, spec.ID, err)
		return nil, err
	}
	defer r.releaseCapacity(spec.ID)

	if r.cfg.MinFreeBytes > 0 {
		freeBytes := r.cfg.FreeBytes
		if freeBytes == nil {
			freeBytes = diskspace.FreeBytes
		}
		free, err := freeBytes(r.cfg.StateDir)
		if err != nil {
			spawnErr := fmt.Errorf("supervisor: checking free disk before spawn %s: %w", spec.ID, err)
			r.recordPreSpawnFailure(ctx, spec.ID, spawnErr)
			return nil, spawnErr
		}
		if free < r.cfg.MinFreeBytes {
			r.log.Error("supervisor: spawn rejected: low disk", "session_id", spec.ID, "free_bytes", free, "min_free_bytes", r.cfg.MinFreeBytes)
			spawnErr := fmt.Errorf("%w: state_dir=%s free_bytes=%d min_free_bytes=%d", ErrLowDisk, r.cfg.StateDir, free, r.cfg.MinFreeBytes)
			r.recordPreSpawnFailure(ctx, spec.ID, spawnErr)
			return nil, spawnErr
		}
	}
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

	rules, err := r.adoptedPermissionRules(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("supervisor: resolving adopted rules for %s: %w", spec.ID, err)
	}
	pinned, err := settings.PinWithRules(r.cfg.StateDir, spec, r.clock, r.cfg.CorralVersion, r.cfg.APIVersion, r.cfg.RelayCommand, foreignHooks, rules)
	if err != nil {
		return nil, fmt.Errorf("supervisor: pinning settings for %s: %w", spec.ID, err)
	}
	spec.SettingsPath = pinned.SettingsPath
	if spec.SettingSources == "" {
		spec.SettingSources = r.cfg.SettingSources
	}
	spec.Env = BuildEnv(spec, r.cfg.EnvSnapshot, r.cfg.EnvPassthrough, r.cfg.Term, r.cfg.SockPath, secret)

	// Shared prologue ends here (secret, settings.Pin, BuildEnv all ran
	// against spec above — headless keeps settings.Pin deliberately, per
	// design doc §8.2, so the hooks relay callback still works). Dispatch
	// on Mode for the rest: ModeHeadless has no PTY/Screen at all (§5.2).
	if spec.Mode == session.ModeHeadless {
		return r.spawnHeadless(ctx, spec, pinned, secret)
	}

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
		sess.Argv = BuildArgv(spec, true)
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

func (r *Registry) recordPreSpawnFailure(ctx context.Context, id string, spawnErr error) {
	if r.store == nil {
		return
	}
	if _, err := r.store.UpdateSession(ctx, id, func(sess *session.Session) {
		sess.Status = session.StatusFailed
	}); err != nil {
		r.log.Error("supervisor: recording pre-spawn failure", "session_id", id, "err", err)
	}
	r.appendEvent(ctx, id, session.EventSessionSpawnFailed, map[string]any{"error": spawnErr.Error()})
}

func (r *Registry) adoptedPermissionRules(ctx context.Context, spec session.Spec) ([]string, error) {
	repo := ""
	if task, err := r.store.GetTaskBySessionID(ctx, spec.ID); err == nil {
		var resolveErr error
		repo, resolveErr = corralgit.CanonicalPath(task.Repo)
		if resolveErr != nil {
			return nil, resolveErr
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	} else {
		var resolveErr error
		repo, resolveErr = corralgit.ResolveRepo(ctx, spec.Cwd)
		if resolveErr != nil {
			return nil, resolveErr
		}
	}
	learnings, err := r.store.ListLearnings(ctx, repo, "")
	if err != nil {
		return nil, err
	}
	var rules []string
	for _, l := range learnings {
		if l.Kind != store.LearningPermissionRule || (l.Status != store.LearningAdopted &&
			l.Status != store.LearningRegressionFlagged && l.Status != store.LearningStale) {
			continue
		}
		var content struct {
			Rule string `json:"rule"`
		}
		if err := json.Unmarshal([]byte(l.ContentJSON), &content); err != nil || content.Rule == "" {
			return nil, fmt.Errorf("learning %s has invalid adopted content", l.ID)
		}
		rules = append(rules, content.Rule)
	}
	return rules, nil
}

// spawnHeadless is Spawn's fork for spec.Mode == session.ModeHeadless
// (design doc §5.2), called only from Spawn after the prologue both paths
// share (secret generation, settings.Pin, BuildEnv) has already run against
// spec. There is no PTY, no Screen, and no output.log — stdout/stderr are
// captured off pipes and parsed as stream-json rather than fed to a
// terminal emulator; the tee targets are stream.jsonl and stderr.log inside
// this session's already-pinned directory (pinned.Dir).
func (r *Registry) spawnHeadless(ctx context.Context, spec session.Spec, pinned *settings.Pinned, secret string) (*session.Session, error) {
	stdout, stderr, cmd, info, err := SpawnHeadless(spec)
	if err != nil {
		if _, uerr := r.store.UpdateSession(ctx, spec.ID, func(sess *session.Session) {
			sess.Status = session.StatusFailed
		}); uerr != nil {
			r.log.Error("supervisor: recording headless spawn failure", "session_id", spec.ID, "err", uerr)
		}
		r.appendEvent(ctx, spec.ID, session.EventSessionSpawnFailed, map[string]any{"error": err.Error()})
		return nil, fmt.Errorf("supervisor: spawning headless %s: %w", spec.ID, err)
	}

	ls := &LiveSession{
		SessionID: spec.ID,
		PGID:      info.PGID,
		Cmd:       cmd,
		Secret:    secret,
		Headless:  true,
	}
	r.register(ls, spec.Name)

	streamLog := r.openHeadlessLogOrDiscard(filepath.Join(pinned.Dir, "stream.jsonl"), spec.ID)
	stderrLog := r.openHeadlessLogOrDiscard(filepath.Join(pinned.Dir, "stderr.log"), spec.ID)

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
		sess.Argv = BuildArgv(spec, true)
		sess.EnvKeys = envKeyNames(spec.Env)
	})
	if err != nil {
		// Same rationale as the interactive persist-failure path above: a
		// live, untracked child is worse than killing it and surfacing the
		// error.
		r.deregister(spec.ID, spec.Name)
		_ = syscallKillGroupBestEffort(info.PGID)
		_ = stdout.Close()
		_ = stderr.Close()
		_ = streamLog.Close()
		_ = stderrLog.Close()
		return nil, fmt.Errorf("supervisor: persisting headless spawn of %s: %w", spec.ID, err)
	}

	r.appendEvent(ctx, spec.ID, session.EventSessionSpawned, map[string]any{"pid": info.PID, "pgid": info.PGID})
	if err := r.engine.OnLifecycle(ctx, spec.ID, session.EventSessionSpawned, nil); err != nil {
		r.log.Warn("supervisor: engine.OnLifecycle spawned", "session_id", spec.ID, "err", err)
	}

	r.runHeadlessGoroutines(ls, stdout, stderr, streamLog, stderrLog, spec.ID, spec.Name)

	return updated, nil
}

// openHeadlessLogOrDiscard opens a capped log at path (screen.OutputLog is
// a generic size-capped io.Writer, not PTY-specific — reused here exactly
// as the interactive path reuses it for output.log), falling back to a
// no-op writer on failure rather than failing the whole spawn: losing a
// diagnostic tee is not worth tearing down an otherwise-healthy child for.
func (r *Registry) openHeadlessLogOrDiscard(path, sessionID string) io.WriteCloser {
	ol, err := screen.OpenOutputLog(path, r.cfg.OutputLogMaxBytes, r.log)
	if err != nil {
		r.log.Warn("supervisor: opening headless log; continuing without it", "session_id", sessionID, "path", path, "err", err)
		return nopWriteCloser{}
	}
	return ol
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

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

// runHeadlessGoroutines starts the I/O and reaper goroutines a headless
// session needs (design doc §5.2): a stdout reader that parses stream-json
// and tees it to streamLog, a stderr drainer that tees to stderrLog, and a
// reaper. The ordering here is the deliberate inverse of runGoroutines'
// PTY-path reaper: a sync.WaitGroup holds cmd.Wait() until BOTH readers
// have reached EOF, because cmd.Wait() closes the StdoutPipe/StderrPipe fds
// the instant it returns — calling it earlier races whichever reader is
// still in flight and can silently drop the tail of the stream, including
// the terminal `result` line. (The PTY path does the opposite — Wait()
// first, then closing the master unblocks the reader — because closing a
// PTY master is what makes the reader's Read return; do not copy that
// order here.)
func (r *Registry) runHeadlessGoroutines(ls *LiveSession, stdout, stderr io.ReadCloser, streamLog, stderrLog io.WriteCloser, id, name string) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer streamLog.Close()
		r.readHeadlessStdout(ls, stdout, streamLog, id)
	}()

	go func() {
		defer wg.Done()
		defer stderrLog.Close()
		// Drain to EOF unconditionally, even though stderrLog silently caps
		// (screen.OutputLog.Write never returns a non-nil error) — a full,
		// unread stderr pipe would otherwise block the child (§5.2).
		_, _ = io.Copy(stderrLog, stderr)
	}()

	go func() {
		wg.Wait()
		_ = ls.Cmd.Wait()

		r.deregister(id, name)
		r.reapHeadless(ls, id)
	}()
}

// readHeadlessStdout consumes stdout line-by-line via
// bufio.Reader.ReadBytes('\n') — deliberately NOT bufio.Scanner, whose
// 64KB MaxScanTokenSize a system:init line's tools array, or a big
// tool_use input, routinely exceeds. Scanner would then return
// bufio.ErrTooLong and stop, silently truncating the rest of the child's
// stream — including the terminal `result` line (§5.2's "lost-result"
// failure, which masquerades as a mid-turn crash under §8.1 rule 3).
// ReadBytes has no such cap.
func (r *Registry) readHeadlessStdout(ls *LiveSession, stdout io.Reader, streamLog io.Writer, id string) {
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// A non-nil err here means this is the final, unterminated
			// chunk before EOF (the child was killed mid-line): still hand
			// it to handleHeadlessLine, which parses it if it happens to be
			// valid JSON on its own and drops it otherwise via ParseLine's
			// tolerant-decode contract — never treated as this loop's error
			// exit.
			r.handleHeadlessLine(ls, line, streamLog, id)
		}
		if err != nil {
			return
		}
	}
}

// handleHeadlessLine parses one stream-json line and, per §5.2: (a) tees
// its raw bytes to streamLog — the authoritative in-flight record, and the
// full record of tool_use/denial lines in v0.4.0 (no separate per-tool
// event kinds), and (b) on a terminal `result` line, stashes the parsed
// Result on ls (guarded by r.mu, same discipline as Attachment) and emits
// the durable session.result event. A blank line is skipped (ParseLine
// errors on "" by design — a line with no type cannot be classified); a
// malformed non-blank line is logged and dropped, never fatal to the
// reader loop.
func (r *Registry) handleHeadlessLine(ls *LiveSession, line []byte, streamLog io.Writer, id string) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}

	ev, err := streamjson.ParseLine(trimmed)
	if err != nil {
		r.log.Warn("supervisor: headless stream: dropping malformed line", "session_id", id, "err", err)
		return
	}

	if _, err := streamLog.Write(ev.Raw); err != nil {
		r.log.Warn("supervisor: headless stream: writing stream log", "session_id", id, "err", err)
	}
	_, _ = streamLog.Write([]byte("\n"))

	if ev.Type != "result" || ev.Result == nil {
		return
	}

	r.mu.Lock()
	ls.Result = ev.Result
	r.mu.Unlock()

	r.appendEvent(context.Background(), id, session.EventSessionResult, map[string]any{
		"is_error":       ev.Result.IsError,
		"total_cost_usd": ev.Result.TotalCostUSD,
		"stop_reason":    ev.Result.StopReason,
		"num_turns":      ev.Result.NumTurns,
	})
}

// HeadlessOutcome applies design doc §8.1's attempt-outcome authority rule
// to a headless attempt's captured terminal Result: the parsed result
// line is authoritative over the process exit code — result+success ==
// true, result+error == false, and no result captured at all (result ==
// nil: the child was killed or crashed mid-turn) == false regardless of
// exit code. Exported so step 21's orchestrator applies this exact mapping
// rather than inventing a second one (§8.1: "do not invent a second
// mapping in run.go").
func HeadlessOutcome(result *streamjson.Result) bool {
	return result != nil && !result.IsError
}

// reapHeadless is spawnHeadless's counterpart to runGoroutines' PTY
// reaper: it runs after cmd.Wait() has returned AND both of runHeadless
// Goroutines' readers have drained to EOF, logs the §8.1 outcome for
// observability, then defers to the same session-row bookkeeping the PTY
// path uses. reap (below) reads only ls.Cmd.ProcessState — never
// ls.PTYMaster/ls.Screen — so it is exactly correct for a headless
// LiveSession too, with no changes.
func (r *Registry) reapHeadless(ls *LiveSession, id string) {
	r.mu.Lock()
	result := ls.Result
	r.mu.Unlock()

	r.log.Info("supervisor: headless attempt finished",
		"session_id", id, "result_captured", result != nil, "success", HeadlessOutcome(result))

	r.reap(ls, id)
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
	turnBoundary, err := cp.Checkpoint(ctx, ls, "user_kill")
	if err != nil {
		return nil, fmt.Errorf("supervisor: killing %s: %w", ls.SessionID, err)
	}

	r.appendEvent(ctx, ls.SessionID, session.EventSessionKilled, map[string]any{"turn_boundary_verified": turnBoundary})
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

	turnBoundary, err := r.checkpointer.Checkpoint(ctx, ls, "idle_reap")
	if err != nil {
		return nil, fmt.Errorf("supervisor: idle-reaping %s: %w", ls.SessionID, err)
	}

	r.appendEvent(ctx, ls.SessionID, session.EventSessionIdleReaped, map[string]any{
		"idle_ms":                idleFor.Milliseconds(),
		"turn_boundary_verified": turnBoundary,
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
	if ls.Headless {
		return ErrHeadlessNoInput
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

// ErrNotResumable is returned by Wake when the checkpointer reports the
// target session cannot be resumed (design doc: no claude_session_id, or no
// on-disk transcript for it).
var ErrNotResumable = errors.New("supervisor: session not resumable")

// ErrNameTaken is returned by Wake when the target's name is currently held by
// a different, non-terminal session. Waking would flip the target back into
// the sessions_name_active partial index and collide — a client-resolvable
// condition (rename or kill the other session), not a server fault.
var ErrNameTaken = errors.New("supervisor: session name held by an active session")

// ErrHeadlessNoInput is returned by WriteInput when idOrName names a
// headless session (design doc §5.2): a `-p` invocation is one-shot and
// exits at `result` — there is no live input channel to write into, and
// ls.PTYMaster is nil for a headless LiveSession, so this guard exists
// specifically to avoid a nil-pointer panic on that field.
var ErrHeadlessNoInput = errors.New("supervisor: headless session accepts no input (one-shot)")
