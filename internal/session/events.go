package session

// EventKind is the append-only event log's vocabulary (design doc §6.2).
// New kinds may be added in later milestones; existing kinds are never
// renamed or removed, since the events table is never rewritten.
type EventKind string

const (
	EventDaemonStarted        EventKind = "daemon.started"
	EventDaemonStopping       EventKind = "daemon.stopping"
	EventSessionCreated       EventKind = "session.created"
	EventSessionSpawned       EventKind = "session.spawned"
	EventSessionSpawnFailed   EventKind = "session.spawn_failed"
	EventSessionAttached      EventKind = "session.attached"
	EventSessionDetached      EventKind = "session.detached"
	EventSessionClientCrashed EventKind = "session.client_crashed"
	EventSessionClientDropped EventKind = "session.client_dropped"
	EventSessionResized       EventKind = "session.resized"
	EventSessionExited        EventKind = "session.exited"
	EventSessionKilled        EventKind = "session.killed"
	EventSessionResumed       EventKind = "session.resumed"
	EventSessionOrphanReaped  EventKind = "session.orphan_reaped"
	EventSessionUnresumable   EventKind = "session.unresumable"
	EventSessionAnswered      EventKind = "session.answered"    // corral answer wrote input to the PTY
	EventSessionIdleReaped    EventKind = "session.idle_reaped" // idle reaper checkpointed a session past state.idle_timeout

	// New in M4 step 19 (design doc §5.2/§8.1) — headless-only: the
	// terminal stream-json `result` line's outcome, carrying
	// is_error/total_cost_usd/stop_reason/num_turns. Never emitted for an
	// interactive session.
	EventSessionResult EventKind = "session.result"

	// New in M4 step 22 (design doc §7) — a task/dag crossed its budget.
	// Attached to the tripping attempt's session_id (the exact attempt that
	// pushed cost at/over the cap). Recording only: status stays `failed`
	// (§7).
	EventTaskBudgetExceeded EventKind = "task.budget_exceeded"

	EventHookReceived     EventKind = "hook.received"
	EventHookDropped      EventKind = "hook.dropped"
	EventHookUnauthorized EventKind = "hook.unauthorized"
	EventHookUndecodable  EventKind = "hook.undecodable"

	// New in M2 step 6b (design doc §2, Amendment A.3) — the hook-driven
	// Engine's own event vocabulary.
	EventPermissionRequested EventKind = "permission.requested"
	EventPermissionBlocked   EventKind = "permission.blocked"
	EventPermissionResolved  EventKind = "permission.resolved"
	EventSubagentStarted     EventKind = "subagent.started"
	EventSubagentStopped     EventKind = "subagent.stopped"
	EventHookUnknownEvent    EventKind = "hook.unknown_event"

	// New in M2 step 8 (design doc §8.4) — the notifier's delivery outcomes.
	// A notification's fate is always an event, never a return value: the
	// engine's Notifier.NotifyBlocked returns nil by construction, so these
	// are the only durable record that a block was (or was not) delivered.
	EventNotifySent       EventKind = "notify.sent"       // delivered; carries backend + attempts
	EventNotifyFailed     EventKind = "notify.failed"     // retries exhausted; carries backend + last error
	EventNotifyDropped    EventKind = "notify.dropped"    // queue full; never blocked the state machine
	EventNotifySuppressed EventKind = "notify.suppressed" // debounced duplicate within notify.debounce

	// New in M5 step 28 (design doc §4) — the bearer-auth middleware's
	// only event: any 401 (missing/malformed header, unknown token,
	// revoked token). Always daemon-scoped (session_id ""), and its
	// data JSON carries only {"remote_addr": ...} — never token bytes,
	// prefix, or hash, since a failed-auth log that echoes the attempted
	// secret would itself be a leak.
	EventTokenUnauthorized EventKind = "token.unauthorized"

	// M6 learning-loop audit events are daemon-scoped. Detailed immutable
	// provenance remains in learning_evidence.
	EventLearningMined    EventKind = "learning.mined"
	EventLearningVerified EventKind = "learning.verified"
	EventLearningProposed EventKind = "learning.proposed"
	EventLearningRejected EventKind = "learning.rejected"
)

// Event is one row of the append-only events table. SessionID is "" for a
// daemon-scoped event (NULL in the DB, e.g. daemon.started/daemon.stopping).
type Event struct {
	Seq       int64
	SessionID string
	TsMs      int64
	Kind      EventKind
	DataJSON  string // raw JSON; "{}" if the kind carries no data
}
