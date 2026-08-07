package session

// Status is the supervisor's observed state of a session's process/PTY.
// This is distinct from DesiredState (durable intent) and from AgentState
// (a later-milestone derived rendering for `corral ls`) — see design doc
// §6.4 "persisted vs derived".
type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusExited   Status = "exited"
	StatusFailed   Status = "failed"
)

// DesiredState is the durable intent a session should converge to across
// daemon restarts: "running" sessions are resumed on startup; "stopped"
// sessions (set by `corral kill`) never are. The row itself is never
// deleted in M1 — see design doc §6.2's note on events' ON DELETE CASCADE
// being unreachable in this milestone.
type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

// Session is the persisted view of a session: the store's sessions table,
// typed. Nullable DB columns (model, claude_session_id, exit_signal) map to
// "" when NULL; exit_code and the three *_at_ms timestamps map to nil
// pointers when NULL, since 0 is a meaningful exit code and a meaningful
// timestamp.
type Session struct {
	ID               string
	Name             string
	Mode             Mode
	Cwd              string
	ClaudeBin        string
	Model            string   // "" = NULL = claude default
	Argv             []string // persisted as argv_json
	EnvKeys          []string // persisted as env_keys_json — names only, never values
	SettingsPath     string
	SettingSources   string
	ClaudeSessionID  string // "" = NULL; == ID unless claude forked it
	DesiredState     DesiredState
	Status           Status
	PID              int
	PGID             int
	ProcStartNs      int64
	Rows             int
	Cols             int
	ExitCode         *int
	ExitSignal       string // "" = NULL
	ResumeCount      int
	CreatedAtMs      int64
	UpdatedAtMs      int64
	StartedAtMs      *int64
	LastAttachedAtMs *int64
	EndedAtMs        *int64
}
