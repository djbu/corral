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

	EventHookReceived     EventKind = "hook.received"
	EventHookDropped      EventKind = "hook.dropped"
	EventHookUnauthorized EventKind = "hook.unauthorized"
	EventHookUndecodable  EventKind = "hook.undecodable"
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
