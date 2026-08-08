package state

// engine_real.go is M2 step 6b's deliverable: the hook-driven Engine
// (design doc §2, Amendment A.3). See machine.go for the pure classifier
// and pending.go for the per-session tool tracker this file drives.
//
// CONCURRENCY MODEL: one goroutine per live session (sessionLoop.run) owns
// all of that session's mutable state (its PendingTracker, its open
// permission requests, its last-activity level). Nothing in that state is
// guarded by a mutex — it is single-goroutine-owned by construction. The
// only mutex in this file (realEngine.mu) guards just the loops map: the
// registry of which sessions currently have a goroutine running.
// OnHookEvent is a non-blocking enqueue onto a buffered channel; it never
// blocks the hook-relay HTTP handler.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/hookrelay"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

// defaultPermissionSettle and defaultPermissionTTL are Amendment A.3.2's
// defaults, applied by NewEngine when EngineConfig leaves either at zero.
// 6c wires these from [state] config.
const (
	defaultPermissionSettle = 15 * time.Second
	defaultPermissionTTL    = 6 * time.Hour
	// Staleness defaults (Amendment §4.5). Applied by NewEngine when
	// EngineConfig leaves either at zero. Both drive render-time
	// derivation only — there is no timer or sweeper goroutine.
	defaultStaleAfter     = 15 * time.Minute
	defaultFirstHookGrace = 60 * time.Second
)

// ingestBufferSize is the per-session hook-event queue's capacity.
// OnHookEvent returns ErrQueueFull rather than blocking once it's full.
const ingestBufferSize = 256

// EngineConfig configures the real Engine's permission-request timers
// (Amendment A.3.2).
type EngineConfig struct {
	// PermissionSettle is how long an unresolved PermissionRequest waits
	// before it's considered "blocked" (drives AgentBlocked + notifier).
	PermissionSettle time.Duration
	// PermissionTTL is how long a request is kept open before it's
	// abandoned outright (removed, recorded as outcome "abandoned").
	// Measured from the request's open time (openedAt + PermissionTTL),
	// NOT from when it crossed PermissionSettle and became blocked — the
	// two timers are independent, both anchored to openedAt.
	PermissionTTL time.Duration
	// StaleAfter and FirstHookGrace drive §4.5 render-time staleness — no
	// timer, evaluated only inside State(). FirstHookGrace: a live session
	// that has produced zero hooks for longer than this since it started
	// renders AgentUnknown (the "hooks not firing?" failure). StaleAfter is
	// consumed by the ls render path (working -> "working?"), carried here
	// so the one [state] config block configures both.
	StaleAfter     time.Duration
	FirstHookGrace time.Duration
}

// Notifier is step 8's seam for delivering a blocked notification (e.g. to
// a terminal bell, a desktop notification, or a future channel). May be
// nil, in which case blocking is still recorded/persisted but nothing is
// notified.
type Notifier interface {
	NotifyBlocked(sessionID string, reason BlockedReason) error
}

// realEngine is M2's hook-driven Engine implementation.
type realEngine struct {
	store    *store.Store
	clk      clock.Clock
	cfg      EngineConfig
	notifier Notifier
	log      *slog.Logger

	// afterStep is a TEST-ONLY synchronization hook: if non-nil, every
	// sessionLoop calls it at the end of each iteration of run(). nil in
	// production.
	afterStep func()

	mu    sync.Mutex
	loops map[string]*sessionLoop
}

var _ Engine = (*realEngine)(nil)

// NewEngine returns the M2 hook-driven Engine. cfg.PermissionSettle and
// cfg.PermissionTTL default to 15s/6h (Amendment A.3.2) when left zero.
// notifier may be nil (no notifications delivered, but blocking is still
// recorded and persisted). log may be nil (defaults to slog.Default()).
func NewEngine(st *store.Store, clk clock.Clock, cfg EngineConfig, notifier Notifier, log *slog.Logger) *realEngine {
	if cfg.PermissionSettle == 0 {
		cfg.PermissionSettle = defaultPermissionSettle
	}
	if cfg.PermissionTTL == 0 {
		cfg.PermissionTTL = defaultPermissionTTL
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = defaultStaleAfter
	}
	if cfg.FirstHookGrace == 0 {
		cfg.FirstHookGrace = defaultFirstHookGrace
	}
	if log == nil {
		log = slog.Default()
	}
	return &realEngine{
		store:    st,
		clk:      clk,
		cfg:      cfg,
		notifier: notifier,
		log:      log,
		loops:    make(map[string]*sessionLoop),
	}
}

// OnHookEvent is a non-blocking enqueue: it gets-or-creates sessionID's
// loop, then attempts a non-blocking send on its ingest channel. It never
// blocks the caller (the hooks HTTP handler) — a full queue returns
// ErrQueueFull instead.
func (e *realEngine) OnHookEvent(ctx context.Context, sessionID string, ev HookEvent) error {
	loop := e.getOrCreateLoop(sessionID)
	select {
	case loop.ingest <- ev:
		return nil
	default:
		return ErrQueueFull
	}
}

// OnLifecycle handles supervisor's spawn/exit/kill transitions. Spawn (and
// the lazy-start path via OnHookEvent, for resume) ensures a loop exists;
// exit/kill stops and removes it. OnLifecycle never touches persisted
// agent_state for terminal transitions — State() derives terminal state
// from session.Status itself, which supervisor already writes.
func (e *realEngine) OnLifecycle(ctx context.Context, sessionID string, kind session.EventKind, data any) error {
	switch kind {
	case session.EventSessionSpawned:
		e.getOrCreateLoop(sessionID)
	case session.EventSessionExited, session.EventSessionKilled:
		e.stopLoop(sessionID)
	}
	return nil
}

// State returns what `corral ls` renders for sessionID. A terminal Status
// (exited/failed) always wins over any persisted agent_state, so a dead
// session is never rendered as working/blocked/idle. Otherwise it returns
// the persisted, hook-driven AgentState. Unknown sessionID reports
// AgentFailed, matching NoopEngine.
func (e *realEngine) State(sessionID string) session.AgentState {
	sess, err := e.store.GetSession(context.Background(), sessionID)
	if err != nil {
		return session.AgentFailed
	}
	return e.deriveState(sess)
}

// deriveState is the pure render-time state derivation shared by State and
// Stale. A terminal Status (exited/failed) always wins over any persisted
// agent_state, so a dead session is never rendered as working/blocked/idle.
func (e *realEngine) deriveState(sess *session.Session) session.AgentState {
	if sess.Status == session.StatusExited || sess.Status == session.StatusFailed {
		return statusToAgentState(sess.Status)
	}
	// §4.5 never-hooked: a live (starting/running) session that has produced
	// no hook at all for longer than FirstHookGrace since it started renders
	// unknown — the "hooks not firing?" failure (broken settings pin, relay
	// not wired). This is the failure that matters most: without it the user
	// stares at "starting" forever. Timer-free — derived here at read time
	// from persisted timestamps against the injected clock. A session that
	// has ANY hook history (HookCount > 0) can never be unknown, so blocked
	// and idle are unaffected.
	if sess.HookCount == 0 && sess.StartedAtMs != nil {
		if e.clk.Now().Sub(time.UnixMilli(*sess.StartedAtMs)) > e.cfg.FirstHookGrace {
			return session.AgentUnknown
		}
	}
	return sess.AgentState
}

// Stale reports §4.5 render-time staleness: a session whose derived state is
// working but whose last hook arrived longer ago than StaleAfter. Callers
// render this as "working?" — the agent may be wedged. Only working goes
// stale: blocked never does (a human is the bottleneck, not the agent),
// unknown/idle/terminal are not working. Timer-free, derived at read time
// against the injected clock. Unknown sessionID or a session with no hook
// history reports false.
func (e *realEngine) Stale(sessionID string) bool {
	sess, err := e.store.GetSession(context.Background(), sessionID)
	if err != nil {
		return false
	}
	if e.deriveState(sess) != session.AgentWorking {
		return false
	}
	if sess.LastHookAtMs == nil {
		return false
	}
	return e.clk.Now().Sub(time.UnixMilli(*sess.LastHookAtMs)) > e.cfg.StaleAfter
}

// getOrCreateLoop returns sessionID's sessionLoop, creating and starting it
// if this is the first time it's been seen. The engine mutex guards only
// map access, never the loop's own state.
func (e *realEngine) getOrCreateLoop(sessionID string) *sessionLoop {
	e.mu.Lock()
	defer e.mu.Unlock()

	if l, ok := e.loops[sessionID]; ok {
		return l
	}

	l := &sessionLoop{
		sessionID:    sessionID,
		ingest:       make(chan hookrelay.HookPayload, ingestBufferSize),
		quit:         make(chan struct{}),
		done:         make(chan struct{}),
		tracker:      NewPendingTracker(),
		lastActivity: session.AgentStarting,
		engine:       e,
	}
	e.loops[sessionID] = l
	go l.run()
	return l
}

// stopLoop closes and removes sessionID's loop, if one exists. Closing quit
// unblocks run()'s select; ingest is never closed (nothing sends on it
// again after removal from the map, and closing it would race a concurrent
// OnHookEvent that already fetched the loop pointer). It waits for run() to
// actually return (via done) before returning itself — a bounded wait, since
// once quit is closed the only ready case in run()'s select is quit itself —
// so that once OnLifecycle(EventSessionExited) returns, the caller (and, in
// tests, a subsequent store close) can rely on the loop no longer touching
// the store.
func (e *realEngine) stopLoop(sessionID string) {
	e.mu.Lock()
	l, ok := e.loops[sessionID]
	if ok {
		delete(e.loops, sessionID)
	}
	e.mu.Unlock()

	if !ok {
		return
	}
	close(l.quit)
	<-l.done
}

// openReq is one in-flight PermissionRequest this session's loop is
// tracking, from PermOpen until it's resolved (approved/failed_or_denied/
// turn_ended/superseded) or abandoned (TTL).
type openReq struct {
	promptID       string
	toolName       string
	toolUseID      string // captured via tracker.MatchTriple at open time (best-effort; "" if unmatched)
	toolInput      []byte // copied
	suggestions    json.RawMessage
	agentID        string
	agentType      string
	permissionMode string

	notificationMessage string
	notificationType    string
	confidence          string // "unresolved_settled" | "notified"

	openedAt       time.Time
	settleDeadline time.Time
	ttlDeadline    time.Time
	blockedAt      time.Time // set once, when blocked flips true; never rewritten

	blocked  bool  // true once settleDeadline has passed
	notified bool  // true once NotifyBlocked has been called for this req (dedupe guard; unrelated to confidence)
	hookSeq  int64 // seq of this req's permission.requested event row
}

// sessionLoop is one live session's single-goroutine-owned state machine.
// Every field below is touched only from run() (and the functions it
// calls) — no mutex guards them.
type sessionLoop struct {
	sessionID string
	ingest    chan hookrelay.HookPayload
	quit      chan struct{}
	done      chan struct{} // closed by run() on return; stopLoop waits on it

	tracker      *PendingTracker
	open         []*openReq
	lastActivity session.AgentState // AgentStarting/AgentIdle/AgentWorking — the non-blocked activity level

	engine *realEngine
}

// run is the session's event loop: a select over quit, ingest, and a
// single timer re-armed each iteration for the earliest pending deadline
// across all open requests. See the package doc comment above for the
// concurrency model and the FakeClock staleness note below.
func (l *sessionLoop) run() {
	defer close(l.done)
	for {
		now := l.engine.clk.Now()

		var timerCh <-chan time.Time
		if d, ok := l.earliestPendingDeadline(); ok {
			wait := d.Sub(now)
			timerCh = l.engine.clk.After(wait)
		}

		select {
		case <-l.quit:
			return
		case ev := <-l.ingest:
			l.processEvent(ev)
		case <-timerCh:
			// Always recompute now from the clock and act purely on
			// now-vs-deadline comparisons (never on the mere fact that
			// the channel fired): because clock.Clock.After cannot be
			// cancelled, a timer armed in an earlier iteration for a
			// request that has since been resolved or superseded by an
			// earlier deadline can still deliver here. scanDeadlines is
			// safe against that: a premature/stale fire simply finds
			// nothing due yet and the next iteration re-arms correctly.
			l.scanDeadlines(l.engine.clk.Now())
		}

		if l.engine.afterStep != nil {
			l.engine.afterStep()
		}
	}
}

// earliestPendingDeadline returns the earliest deadline among all open
// requests — settleDeadline for a not-yet-blocked request, ttlDeadline for
// one already blocked — or ok=false if there are no open requests.
func (l *sessionLoop) earliestPendingDeadline() (time.Time, bool) {
	var (
		earliest time.Time
		found    bool
	)
	for _, r := range l.open {
		d := r.settleDeadline
		if r.blocked {
			d = r.ttlDeadline
		}
		if !found || d.Before(earliest) {
			earliest = d
			found = true
		}
	}
	return earliest, found
}

// processEvent classifies ev and applies its effects: heartbeat bookkeeping,
// the pending-tool tracker, the permission-request lifecycle, subagent
// bookkeeping, corroboration, and the activity-target transition — then
// persists the result.
func (l *sessionLoop) processEvent(ev hookrelay.HookPayload) {
	ctx := context.Background()
	tr := Classify(ev)

	// Heartbeat bookkeeping happens for every event, recognized or not.
	nowMs := l.engine.clk.Now().UnixMilli()

	if tr.Unknown {
		l.appendEvent(ctx, session.EventHookUnknownEvent, map[string]any{
			"hook_event_name": ev.HookEventName,
		})
		l.persistHeartbeat(ctx, ev, nowMs)
		return
	}

	if tr.Reset {
		// SessionStart ends any prior turn: resolve (don't silently drop)
		// still-open requests so every permission.requested row has a
		// terminal permission.resolved counterpart and the append-only
		// event stream stays balanced (advisor review, A.7 audit shape).
		l.resolveAll(ctx, "session_reset")
		l.tracker.Reset()
		l.lastActivity = session.AgentIdle
	}

	if tr.OpensTool {
		l.tracker.Open(ev)
	}

	if tr.ClosesTool {
		l.tracker.Close(ev)
	}

	switch tr.Perm {
	case PermOpen:
		l.openPermission(ctx, ev)
	case PermResolveApproved:
		l.resolveMatching(ctx, ev, "approved")
	case PermResolveFailed:
		l.resolveMatching(ctx, ev, "failed_or_denied")
	case PermResolveTurnEnded:
		// Stop/StopFailure end the turn. If the Stop payload carries a
		// prompt_id we scope to it; but if real Claude Code omits it on
		// Stop (unverified until step 7/10 — m2.md), an empty prompt_id
		// would match nothing and the turn-end safety net would be dead
		// code, so fall back to resolving every open request. A session
		// has one active turn at a time, so resolve-all at turn end is
		// correct either way.
		if ev.PromptID == "" {
			l.resolveAll(ctx, "turn_ended")
		} else {
			l.resolveAllForPrompt(ctx, ev.PromptID, "turn_ended")
		}
	case PermResolveSuperseded:
		l.resolveAll(ctx, "superseded")
	}

	// Corroboration: a permission_prompt Notification never itself drives
	// a transition, but it strengthens confidence on same-prompt open
	// requests.
	if ev.HookEventName == "Notification" && ev.NotificationType == "permission_prompt" {
		for _, r := range l.open {
			if r.promptID == ev.PromptID {
				r.confidence = "notified"
				r.notificationMessage = ev.Message
				r.notificationType = ev.NotificationType
			}
		}
	}

	if ev.HookEventName == "SubagentStart" {
		l.appendEvent(ctx, session.EventSubagentStarted, map[string]any{
			"agent_id": ev.AgentID, "agent_type": ev.AgentType,
		})
	}
	if ev.HookEventName == "SubagentStop" {
		l.appendEvent(ctx, session.EventSubagentStopped, map[string]any{
			"agent_id": ev.AgentID, "agent_type": ev.AgentType,
		})
	}

	switch tr.Target {
	case TargetIdle:
		l.lastActivity = session.AgentIdle
	case TargetWorking:
		l.lastActivity = session.AgentWorking
	}

	l.persistHeartbeat(ctx, ev, nowMs)
	l.recomputeAndPersist(ctx)
}

// openPermission handles PermOpen (a PermissionRequest): it builds an
// openReq, best-effort resolves its tool_use_id via the pending tracker
// (captured now, since the tracker entry it matches may be closed or the
// session may progress long before settle/TTL fire), appends
// permission.requested, and adds it to open[].
func (l *sessionLoop) openPermission(ctx context.Context, ev hookrelay.HookPayload) {
	now := l.engine.clk.Now()

	toolInput := make([]byte, len(ev.ToolInput))
	copy(toolInput, ev.ToolInput)

	var toolUseID string
	if entry, ok := l.tracker.MatchTriple(ev.AgentID, ev.PromptID, ev.ToolName, toolInput); ok {
		toolUseID = entry.ToolUseID
	}

	// A nil/empty suggestions slice must marshal to nil, not "null" —
	// json.Marshal(nil slice) yields the literal "null", which the
	// BlockedReason field's omitempty does not drop (len 4, non-empty).
	var suggestions json.RawMessage
	if len(ev.PermissionSuggestions) > 0 {
		if b, err := json.Marshal(ev.PermissionSuggestions); err == nil {
			suggestions = b
		}
	}

	r := &openReq{
		promptID:       ev.PromptID,
		toolName:       ev.ToolName,
		toolUseID:      toolUseID,
		toolInput:      toolInput,
		suggestions:    suggestions,
		agentID:        ev.AgentID,
		agentType:      ev.AgentType,
		permissionMode: ev.PermissionMode,
		confidence:     "unresolved_settled",
		openedAt:       now,
		settleDeadline: now.Add(l.engine.cfg.PermissionSettle),
		ttlDeadline:    now.Add(l.engine.cfg.PermissionTTL),
	}
	l.open = append(l.open, r)

	r.hookSeq = l.appendEvent(ctx, session.EventPermissionRequested, map[string]any{
		"prompt_id": ev.PromptID, "tool_name": ev.ToolName,
	})
}

// resolveMatching resolves the open request matching (ev.PromptID,
// ev.ToolName, byte-equal ev.ToolInput) with outcome, if any is found
// (Amendment A.3.1's join key).
func (l *sessionLoop) resolveMatching(ctx context.Context, ev hookrelay.HookPayload, outcome string) {
	for i, r := range l.open {
		if matchOpen(r, ev) {
			l.resolve(ctx, i, outcome)
			return
		}
	}
}

// matchOpen reports whether resolving event ev refers to open request r.
// PermissionRequest carries no tool_use_id (payload.go:53), so r.toolUseID
// is only populated when the PendingTracker matched it from the paired
// PreToolUse; PostToolUse does carry one. When BOTH sides have a
// tool_use_id we trust it exactly — it is stable regardless of any
// cosmetic re-serialization of tool_input between PermissionRequest and
// PostToolUse. Only when either side lacks one do we fall back to
// Amendment A.3.1's (prompt_id, tool_name, byte-equal tool_input) triple.
// This makes resolution robust to key-order/whitespace drift in real
// payloads, which is exactly the false-blocked failure A.3 guards against
// (see step 7/10 real-payload validation, m2.md).
func matchOpen(r *openReq, ev hookrelay.HookPayload) bool {
	if r.toolUseID != "" && ev.ToolUseID != "" {
		return r.toolUseID == ev.ToolUseID
	}
	return r.promptID == ev.PromptID && r.toolName == ev.ToolName && bytes.Equal(r.toolInput, ev.ToolInput)
}

// resolveAllForPrompt resolves every open request whose promptID matches
// promptID (Stop/StopFailure: the turn ended).
func (l *sessionLoop) resolveAllForPrompt(ctx context.Context, promptID string, outcome string) {
	// Iterate over a snapshot since resolve mutates l.open in place.
	for i := len(l.open) - 1; i >= 0; i-- {
		if l.open[i].promptID == promptID {
			l.resolve(ctx, i, outcome)
		}
	}
}

// resolveAll resolves every open request (UserPromptSubmit: a new turn
// supersedes anything still outstanding from the previous one).
func (l *sessionLoop) resolveAll(ctx context.Context, outcome string) {
	for i := len(l.open) - 1; i >= 0; i-- {
		l.resolve(ctx, i, outcome)
	}
}

// resolve removes l.open[i] and appends permission.resolved with outcome.
func (l *sessionLoop) resolve(ctx context.Context, i int, outcome string) {
	r := l.open[i]
	l.open = append(l.open[:i], l.open[i+1:]...)

	l.appendEvent(ctx, session.EventPermissionResolved, map[string]any{
		"outcome":   outcome,
		"settle_ms": l.engine.clk.Now().Sub(r.openedAt).Milliseconds(),
		"prompt_id": r.promptID,
		"tool_name": r.toolName,
	})
}

// scanDeadlines is called whenever the loop's timer fires. now is always a
// fresh clk.Now() read by the caller — never trust the mere fact that a
// timer channel delivered a value. It blocks any request whose
// settleDeadline has passed and hasn't been notified yet, and abandons any
// request whose ttlDeadline has passed.
func (l *sessionLoop) scanDeadlines(now time.Time) {
	ctx := context.Background()

	for _, r := range l.open {
		if !r.blocked && !now.Before(r.settleDeadline) {
			r.blocked = true
			r.blockedAt = now
			if !r.notified {
				r.notified = true
				l.appendEvent(ctx, session.EventPermissionBlocked, map[string]any{
					"prompt_id": r.promptID, "tool_name": r.toolName,
				})
				if l.engine.notifier != nil {
					if err := l.engine.notifier.NotifyBlocked(l.sessionID, l.reasonFor(r)); err != nil {
						l.engine.log.Warn("state: notifier.NotifyBlocked failed", "session_id", l.sessionID, "error", err)
					}
				}
			}
		}
	}

	// Abandon anything past its TTL. Iterate backwards since resolve
	// mutates l.open in place.
	for i := len(l.open) - 1; i >= 0; i-- {
		if !now.Before(l.open[i].ttlDeadline) {
			l.resolve(ctx, i, "abandoned")
		}
	}

	l.recomputeAndPersist(ctx)
}

// reasonFor builds the BlockedReason for r. BlockedAt is r.blockedAt, set
// once when r first crossed its settleDeadline — it is never advanced by
// later calls (e.g. from recomputeAndPersist on a subsequent event), so
// "blocked for N minutes" reflects the real blocking moment, not "now".
func (l *sessionLoop) reasonFor(r *openReq) BlockedReason {
	return BlockedReason{
		Kind:                  kindForTool(r.toolName),
		Summary:               summaryForTool(r.toolName, r.toolInput),
		ToolName:              r.toolName,
		ToolUseID:             r.toolUseID,
		ToolInput:             json.RawMessage(r.toolInput),
		PermissionSuggestions: r.suggestions,
		PromptID:              r.promptID,
		AgentID:               r.agentID,
		AgentType:             r.agentType,
		PermissionMode:        r.permissionMode,
		NotificationMessage:   r.notificationMessage,
		NotificationType:      r.notificationType,
		Confidence:            r.confidence,
		OpenedAt:              r.openedAt.UTC().Format(time.RFC3339),
		BlockedAt:             r.blockedAt.UTC().Format(time.RFC3339),
		HookSeq:               r.hookSeq,
		Redactions:            []string{},
	}
}

// persistHeartbeat bumps HookCount/LastHookAtMs/LastPromptID. It is called
// on every event, recognized or not, independent of recomputeAndPersist
// (which is not called for unknown events).
func (l *sessionLoop) persistHeartbeat(ctx context.Context, ev hookrelay.HookPayload, nowMs int64) {
	_, err := l.engine.store.UpdateSession(ctx, l.sessionID, func(s *session.Session) {
		s.HookCount++
		s.LastHookAtMs = &nowMs
		if ev.PromptID != "" {
			s.LastPromptID = ev.PromptID
		}
		// permission_mode is "last observed, from hook payloads" (A.8): it
		// rides on nearly every payload (all but SessionStart). Record the
		// most recent non-empty value so `corral ls --json` can surface the
		// mode the agent is actually running under.
		if ev.PermissionMode != "" {
			s.PermissionMode = ev.PermissionMode
		}
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		l.engine.log.Warn("state: persisting heartbeat failed", "session_id", l.sessionID, "error", err)
	}
}

// recomputeAndPersist derives agent_state and blocked_reason_json from the
// loop's current open[]/lastActivity and writes them. The oldest
// still-blocked request (by openedAt) is the one whose reason is persisted
// (Amendment A.3.3); if none are blocked, blocked_reason_json is cleared.
func (l *sessionLoop) recomputeAndPersist(ctx context.Context) {
	var blocked []*openReq
	for _, r := range l.open {
		if r.blocked {
			blocked = append(blocked, r)
		}
	}

	newState := l.lastActivity
	reasonJSON := ""
	if len(blocked) > 0 {
		// Stable: two requests opened in the same fake-clock tick share an
		// identical openedAt, and "oldest wins" must not be allowed to
		// flip nondeterministically between them on repeated recomputes.
		sort.SliceStable(blocked, func(i, j int) bool { return blocked[i].openedAt.Before(blocked[j].openedAt) })
		newState = session.AgentBlocked
		reasonJSON = marshalBlockedReason(l.reasonFor(blocked[0]))
	}

	nowMs := l.engine.clk.Now().UnixMilli()
	_, err := l.engine.store.UpdateSession(ctx, l.sessionID, func(s *session.Session) {
		if s.AgentState != newState {
			s.AgentStateSinceMs = &nowMs
		}
		s.AgentState = newState
		s.BlockedReasonJSON = reasonJSON
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		l.engine.log.Warn("state: persisting agent_state failed", "session_id", l.sessionID, "error", err)
	}
}

// appendEvent appends an event row, logging (never failing the loop) on
// error. Returns the row's assigned Seq, or 0 if the append failed.
func (l *sessionLoop) appendEvent(ctx context.Context, kind session.EventKind, data map[string]any) int64 {
	b, err := json.Marshal(data)
	if err != nil {
		b = []byte("{}")
	}
	evt, err := l.engine.store.AppendEvent(ctx, l.sessionID, kind, string(b))
	if err != nil {
		l.engine.log.Warn("state: appending event failed", "session_id", l.sessionID, "kind", kind, "error", err)
		return 0
	}
	return evt.Seq
}
