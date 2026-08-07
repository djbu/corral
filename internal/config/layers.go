package config

// layer is the raw, unresolved shape of one config source: a TOML file, the
// environment, defaults, or an API request. Every field is a pointer so
// "absent" (nil) is distinguishable from "present with the zero value" —
// this is what makes merge() able to tell whether a source actually set a
// key. Durations and byte sizes are kept as their original strings here;
// parsing happens once, after the final merge, in load.go.
type layer struct {
	Daemon  daemonLayer  `toml:"daemon"`
	Session sessionLayer `toml:"session"`
	Attach  attachLayer  `toml:"attach"`
}

type daemonLayer struct {
	Socket        *string `toml:"socket"`
	StateDir      *string `toml:"state_dir"`
	LogLevel      *string `toml:"log_level"`
	LogFormat     *string `toml:"log_format"`
	ShutdownGrace *string `toml:"shutdown_grace"`
}

type sessionLayer struct {
	ClaudeBin         *string   `toml:"claude_bin"`
	Model             *string   `toml:"model"`
	SettingSources    *string   `toml:"setting_sources"`
	EnvPassthrough    *[]string `toml:"env_passthrough"`
	Term              *string   `toml:"term"`
	ScrollbackLines   *int      `toml:"scrollback_lines"`
	OutputLogMaxBytes *string   `toml:"output_log_max_bytes"`
}

type attachLayer struct {
	PrefixKey    *string `toml:"prefix_key"`
	DetachKey    *string `toml:"detach_key"`
	PingInterval *string `toml:"ping_interval"`
	PingTimeout  *string `toml:"ping_timeout"`
}

// newLayer returns an all-nil layer, ready to be merged into.
func newLayer() *layer {
	return &layer{}
}

func strPtr(s string) *string      { return &s }
func intPtr(n int) *int            { return &n }
func strsPtr(s []string) *[]string { return &s }

// sourceFunc returns the source label to record for a given "section.key"
// when merge overwrites it. Every caller of merge that isn't the env layer
// uses a single fixed label for every key it sets (uniformSource); the env
// layer computes a different label per key (the specific CORRAL_* var name
// that was actually read), so it needs the general form.
type sourceFunc func(key string) string

// uniformSource returns a sourceFunc that returns the same label for every
// key — used for the defaults layer, a user/repo file (labeled by its
// path), and the request layer.
func uniformSource(label string) sourceFunc {
	return func(string) string { return label }
}

// mapSource returns a sourceFunc backed by a precomputed per-key label map
// — used for the env layer, where each key's label names its own specific
// CORRAL_* variable.
func mapSource(labels map[string]string) sourceFunc {
	return func(key string) string { return labels[key] }
}

// merge copies every non-nil field of src into dst, overwriting whatever
// was there, and records sources[key] = srcSource(key) for each field it
// copies. dst accumulates the result of the whole precedence chain; src is
// left unmodified. This is the one place field-by-field precedence logic
// lives — no reflection, so adding a config key means adding one line here
// per section, matching layers.go and config.go.
func merge(dst, src *layer, srcSource sourceFunc, sources map[string]string) {
	mergeDaemon(&dst.Daemon, &src.Daemon, srcSource, sources)
	mergeSession(&dst.Session, &src.Session, srcSource, sources)
	mergeAttach(&dst.Attach, &src.Attach, srcSource, sources)
}

func mergeDaemon(dst, src *daemonLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Socket != nil {
		dst.Socket = src.Socket
		sources["daemon.socket"] = srcSource("daemon.socket")
	}
	if src.StateDir != nil {
		dst.StateDir = src.StateDir
		sources["daemon.state_dir"] = srcSource("daemon.state_dir")
	}
	if src.LogLevel != nil {
		dst.LogLevel = src.LogLevel
		sources["daemon.log_level"] = srcSource("daemon.log_level")
	}
	if src.LogFormat != nil {
		dst.LogFormat = src.LogFormat
		sources["daemon.log_format"] = srcSource("daemon.log_format")
	}
	if src.ShutdownGrace != nil {
		dst.ShutdownGrace = src.ShutdownGrace
		sources["daemon.shutdown_grace"] = srcSource("daemon.shutdown_grace")
	}
}

func mergeSession(dst, src *sessionLayer, srcSource sourceFunc, sources map[string]string) {
	if src.ClaudeBin != nil {
		dst.ClaudeBin = src.ClaudeBin
		sources["session.claude_bin"] = srcSource("session.claude_bin")
	}
	if src.Model != nil {
		dst.Model = src.Model
		sources["session.model"] = srcSource("session.model")
	}
	if src.SettingSources != nil {
		dst.SettingSources = src.SettingSources
		sources["session.setting_sources"] = srcSource("session.setting_sources")
	}
	if src.EnvPassthrough != nil {
		dst.EnvPassthrough = src.EnvPassthrough
		sources["session.env_passthrough"] = srcSource("session.env_passthrough")
	}
	if src.Term != nil {
		dst.Term = src.Term
		sources["session.term"] = srcSource("session.term")
	}
	if src.ScrollbackLines != nil {
		dst.ScrollbackLines = src.ScrollbackLines
		sources["session.scrollback_lines"] = srcSource("session.scrollback_lines")
	}
	if src.OutputLogMaxBytes != nil {
		dst.OutputLogMaxBytes = src.OutputLogMaxBytes
		sources["session.output_log_max_bytes"] = srcSource("session.output_log_max_bytes")
	}
}

func mergeAttach(dst, src *attachLayer, srcSource sourceFunc, sources map[string]string) {
	if src.PrefixKey != nil {
		dst.PrefixKey = src.PrefixKey
		sources["attach.prefix_key"] = srcSource("attach.prefix_key")
	}
	if src.DetachKey != nil {
		dst.DetachKey = src.DetachKey
		sources["attach.detach_key"] = srcSource("attach.detach_key")
	}
	if src.PingInterval != nil {
		dst.PingInterval = src.PingInterval
		sources["attach.ping_interval"] = srcSource("attach.ping_interval")
	}
	if src.PingTimeout != nil {
		dst.PingTimeout = src.PingTimeout
		sources["attach.ping_timeout"] = srcSource("attach.ping_timeout")
	}
}
