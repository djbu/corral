// Package config implements corral's layered TOML+env configuration system
// (design doc §8). There are two independent resolution pipelines:
//
//   - LoadDaemon: defaults -> ~/.corral/config.toml -> env. Resolved once at
//     daemon startup; a repo file can never affect it.
//   - LoadSession: defaults -> ~/.corral/config.toml -> nearest .corral.toml
//     (repo-file trust boundary applies) -> env -> request. Resolved per
//     session, from the session's cwd.
//
// Both pipelines are built from the same pointer-field layer structs in
// layers.go, merged with the hand-written merge() so every resolved key
// carries an attributable source string for `corral config`.
package config

import "time"

// Daemon is the resolved daemon-scope configuration.
type Daemon struct {
	Socket        string
	StateDir      string
	LogLevel      string
	LogFormat     string
	ShutdownGrace time.Duration
	// Listen is the opt-in TCP+TLS listener's host:port (design doc m5.md
	// §5, step 30). Empty (the default) means unix-socket-only: corral
	// never becomes network-reachable unless an operator explicitly sets
	// this. Fails closed — there is no "TCP without TLS" path.
	Listen string
	// TLSCert and TLSKey are the both-or-neither operator override that
	// points Listen's TLS at a cert from the operator's own CA / tailscale
	// / Let's Encrypt instead of the self-signed bootstrap cert
	// (tlsbootstrap.LoadOrGenerate). Both empty (the default) uses the
	// self-signed cert; the daemon rejects startup if exactly one is set.
	TLSCert string
	TLSKey  string
	// MinFreeBytes rejects new spawns before state_dir runs out of space.
	// It is daemon/operator policy and can never be set by a repo file.
	MinFreeBytes int64
	// MaxInteractiveSessions and MaxHeadlessTasks are independent admission
	// caps. They are daemon/operator policy so a repository cannot consume
	// capacity by raising either value.
	MaxInteractiveSessions int
	MaxHeadlessTasks       int
	// MaxPendingDAGTasks bounds a single accepted DAG submission before it
	// creates durable work. It is deliberately separate from live headless
	// capacity: queued tasks are durable but do not own a process slot.
	MaxPendingDAGTasks int
}

// Session is the resolved session-scope configuration.
type Session struct {
	ClaudeBin string
	// Template is a selection only. Repositories may name a user-authorized
	// template but never supply the template values themselves.
	Template string
	// ClaudeVersion is an optional repository-compatible semver constraint.
	// The repo may set this policy but never the binary it probes or executes.
	ClaudeVersion     string
	Model             string
	SettingSources    string
	EnvPassthrough    []string
	Term              string
	ScrollbackLines   int
	OutputLogMaxBytes int64
	// PermissionMode is user/env-settable only, never repo-settable (design
	// doc §8.7, Amendment A.6) — see repo_allowlist.go.
	PermissionMode string
}

// State is the resolved [state] configuration (design doc §8.7, Amendment
// A.3.1/A.3.2). Entirely user-file/env only, never repo-settable; resolved
// by its own LoadState pipeline, not LoadDaemon or LoadSession.
type State struct {
	StaleAfter           time.Duration
	FirstHookGrace       time.Duration
	PendingToolTTL       time.Duration
	MaxEventPayloadBytes int64
	PersistHookEvents    string
	HookTimeout          time.Duration
	PermissionSettle     time.Duration
	PermissionTTL        time.Duration
	// IdleTimeout is the foundation for the idle reaper (a later step): 0
	// (default) disables it. Lives in [state] rather than [session]
	// because [state] is user-file/env-only, never repo-settable (trust
	// boundary — a cloned repo must not be able to set how aggressively
	// its own sessions get reaped).
	IdleTimeout time.Duration
}

// Attach is the resolved attach-scope configuration.
type Attach struct {
	PrefixKey    string
	DetachKey    string
	PingInterval time.Duration
	PingTimeout  time.Duration
}

// Notify is the resolved [notify] configuration (design doc §8.7). Entirely
// user-file/env only, never repo-settable; resolved by its own LoadNotify
// pipeline, not LoadDaemon or LoadSession.
type Notify struct {
	Enabled  bool
	On       []string // subset of ["blocked","exited"]
	Debounce time.Duration
	Timeout  time.Duration
	Retries  int
	Ntfy     NotifyNtfy
	Webhook  NotifyWebhook
}

// NotifyNtfy is the resolved [notify.ntfy] configuration.
type NotifyNtfy struct {
	Enabled  bool
	Server   string
	Topic    string
	Token    string // secret
	Priority string
	Reply    NotifyNtfyReply
}

// NotifyNtfyReply is the resolved [notify.ntfy.reply] configuration.
type NotifyNtfyReply struct {
	Enabled bool
	Topic   string
	Token   string // secret
}

// NotifyWebhook is the resolved [notify.webhook] configuration.
type NotifyWebhook struct {
	Enabled bool
	URL     string
	Headers map[string]string // secret-class
}

// Client is the resolved [client]-scope configuration (design doc m5.md
// §7): where the CLI points when targeting a remote daemon. Entirely
// user-file/env only, never repo-settable; resolved by its own LoadClient
// pipeline, not LoadDaemon or LoadSession.
type Client struct {
	Host string
	// Token is the bearer token presented to a remote daemon's TCP+TLS
	// listener. It is a secret, deliberately never CLI-flag-settable (that
	// would put plaintext in `ps aux`/shell history) and never surfaced by
	// Effective's values map (see effective.go).
	Token string // secret
	// CACert, when set, pins the remote daemon's self-signed CA/cert as the
	// SOLE trusted root for TLS verification (see client.NewRemote); empty
	// falls back to the system trust store.
	CACert string
}

// Learn controls M6's verified learning loop. It is operator policy, so it
// is resolved only from defaults, the user config, and environment variables.
type Learn struct {
	Window                  time.Duration
	MinApprovals            int
	TTL                     time.Duration
	MinSessions             int
	MinTerminalTasks        int
	CostRegressionTolerance float64
}

// Rejection records one key found in a repo .corral.toml that was not
// applied because the key is not on the repo-file allowlist (§8.2). File is
// the absolute path of the repo file; Key is "section.key".
type Rejection struct {
	File string
	Key  string
}

// sourceDefault, sourceRequest are the two fixed, non-path source labels
// used in the Sources map. User/repo-file sources are the file's absolute
// path; env sources are "env CORRAL_<SECTION>_<KEY>".
const (
	sourceDefault = "default"
	sourceRequest = "request"
)
