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
}

// Session is the resolved session-scope configuration.
type Session struct {
	ClaudeBin         string
	Model             string
	SettingSources    string
	EnvPassthrough    []string
	Term              string
	ScrollbackLines   int
	OutputLogMaxBytes int64
}

// Attach is the resolved attach-scope configuration.
type Attach struct {
	PrefixKey    string
	DetachKey    string
	PingInterval time.Duration
	PingTimeout  time.Duration
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
