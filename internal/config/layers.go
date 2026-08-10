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
	State   stateLayer   `toml:"state"`
	Notify  notifyLayer  `toml:"notify"`
	Client  clientLayer  `toml:"client"`
	Learn   learnLayer   `toml:"learn"`
}

type daemonLayer struct {
	Socket                 *string `toml:"socket"`
	StateDir               *string `toml:"state_dir"`
	LogLevel               *string `toml:"log_level"`
	LogFormat              *string `toml:"log_format"`
	ShutdownGrace          *string `toml:"shutdown_grace"`
	Listen                 *string `toml:"listen"`
	TLSCert                *string `toml:"tls_cert"`
	TLSKey                 *string `toml:"tls_key"`
	MinFreeBytes           *string `toml:"min_free_bytes"`
	MaxInteractiveSessions *int    `toml:"max_interactive_sessions"`
	MaxHeadlessTasks       *int    `toml:"max_headless_tasks"`
	MaxPendingDAGTasks     *int    `toml:"max_pending_dag_tasks"`
}

type sessionLayer struct {
	ClaudeBin         *string   `toml:"claude_bin"`
	Template          *string   `toml:"template"`
	ClaudeVersion     *string   `toml:"claude_version"`
	Model             *string   `toml:"model"`
	SettingSources    *string   `toml:"setting_sources"`
	EnvPassthrough    *[]string `toml:"env_passthrough"`
	Term              *string   `toml:"term"`
	ScrollbackLines   *int      `toml:"scrollback_lines"`
	OutputLogMaxBytes *string   `toml:"output_log_max_bytes"`
	// PermissionMode is user/env-settable only, never repo-settable (design
	// doc §8.7, Amendment A.6): a repo's .corral.toml must never be able to
	// widen a session's approval posture to bypassPermissions.
	PermissionMode *string `toml:"permission_mode"`
}

type attachLayer struct {
	PrefixKey    *string `toml:"prefix_key"`
	DetachKey    *string `toml:"detach_key"`
	PingInterval *string `toml:"ping_interval"`
	PingTimeout  *string `toml:"ping_timeout"`
}

// stateLayer is M2's [state] section (design doc §8.7, Amendment A.3.2/
// A.3.1): entirely user-file/env only, never repo-settable — see
// repo_allowlist.go. It is resolved by its own LoadState() pipeline
// (defaults -> user file -> env, no repo file, no request), not by
// LoadDaemon or LoadSession, so it deliberately is not touched by merge()
// below; see mergeState.
type stateLayer struct {
	StaleAfter           *string `toml:"stale_after"`
	FirstHookGrace       *string `toml:"first_hook_grace"`
	PendingToolTTL       *string `toml:"pending_tool_ttl"`
	MaxEventPayloadBytes *string `toml:"max_event_payload_bytes"`
	PersistHookEvents    *string `toml:"persist_hook_events"`
	HookTimeout          *string `toml:"hook_timeout"`
	PermissionSettle     *string `toml:"permission_settle"`
	PermissionTTL        *string `toml:"permission_ttl"`
	IdleTimeout          *string `toml:"idle_timeout"`
}

// notifyLayer is [notify]'s section (design doc §8.7): entirely
// user-file/env only, never repo-settable — see repo_allowlist.go. It is
// resolved by its own LoadNotify() pipeline (defaults -> user file -> env,
// no repo file, no request), not by LoadDaemon or LoadSession, so it
// deliberately is not touched by merge() below; see mergeNotify. Debounce
// and Timeout are kept as strings (like the other duration fields) so
// parsing happens once, after the final merge, in load.go. Retries is
// *int, like session.scrollback_lines, since the design doc's defaults
// block writes it unquoted (retries=3) — BurntSushi/toml is strictly typed
// and will not decode an unquoted TOML integer into a *string field.
type notifyLayer struct {
	Enabled  *bool              `toml:"enabled"`
	On       *[]string          `toml:"on"`
	Debounce *string            `toml:"debounce"`
	Timeout  *string            `toml:"timeout"`
	Retries  *int               `toml:"retries"`
	Ntfy     notifyNtfyLayer    `toml:"ntfy"`
	Webhook  notifyWebhookLayer `toml:"webhook"`
}

type notifyNtfyLayer struct {
	Enabled  *bool                `toml:"enabled"`
	Server   *string              `toml:"server"`
	Topic    *string              `toml:"topic"`
	Token    *string              `toml:"token"`
	Priority *string              `toml:"priority"`
	Reply    notifyNtfyReplyLayer `toml:"reply"`
}

type notifyNtfyReplyLayer struct {
	Enabled *bool   `toml:"enabled"`
	Topic   *string `toml:"topic"`
	Token   *string `toml:"token"`
}

type notifyWebhookLayer struct {
	Enabled *bool             `toml:"enabled"`
	URL     *string           `toml:"url"`
	Headers map[string]string `toml:"headers,omitempty"`
}

// clientLayer is [client]'s section (design doc m5.md §7): entirely
// user-file/env only, never repo-settable — see repo_allowlist.go. It is
// resolved by its own LoadClient() pipeline (defaults -> user file -> env,
// no repo file, no request), not by LoadDaemon or LoadSession, so it is
// deliberately not touched by merge() below; see mergeClient.
type clientLayer struct {
	Host   *string `toml:"host"`
	Token  *string `toml:"token"`
	CACert *string `toml:"cacert"`
}

type learnLayer struct {
	Window                  *string  `toml:"window"`
	MinApprovals            *int     `toml:"min_approvals"`
	TTL                     *string  `toml:"ttl"`
	MinSessions             *int     `toml:"min_sessions"`
	MinTerminalTasks        *int     `toml:"min_terminal_tasks"`
	CostRegressionTolerance *float64 `toml:"cost_regression_tolerance"`
}

// newLayer returns an all-nil layer, ready to be merged into.
func newLayer() *layer {
	return &layer{}
}

func strPtr(s string) *string      { return &s }
func intPtr(n int) *int            { return &n }
func boolPtr(b bool) *bool         { return &b }
func floatPtr(n float64) *float64  { return &n }
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
	if src.Listen != nil {
		dst.Listen = src.Listen
		sources["daemon.listen"] = srcSource("daemon.listen")
	}
	if src.TLSCert != nil {
		dst.TLSCert = src.TLSCert
		sources["daemon.tls_cert"] = srcSource("daemon.tls_cert")
	}
	if src.TLSKey != nil {
		dst.TLSKey = src.TLSKey
		sources["daemon.tls_key"] = srcSource("daemon.tls_key")
	}
	if src.MinFreeBytes != nil {
		dst.MinFreeBytes = src.MinFreeBytes
		sources["daemon.min_free_bytes"] = srcSource("daemon.min_free_bytes")
	}
	if src.MaxInteractiveSessions != nil {
		dst.MaxInteractiveSessions = src.MaxInteractiveSessions
		sources["daemon.max_interactive_sessions"] = srcSource("daemon.max_interactive_sessions")
	}
	if src.MaxHeadlessTasks != nil {
		dst.MaxHeadlessTasks = src.MaxHeadlessTasks
		sources["daemon.max_headless_tasks"] = srcSource("daemon.max_headless_tasks")
	}
	if src.MaxPendingDAGTasks != nil {
		dst.MaxPendingDAGTasks = src.MaxPendingDAGTasks
		sources["daemon.max_pending_dag_tasks"] = srcSource("daemon.max_pending_dag_tasks")
	}
}

func mergeSession(dst, src *sessionLayer, srcSource sourceFunc, sources map[string]string) {
	if src.ClaudeBin != nil {
		dst.ClaudeBin = src.ClaudeBin
		sources["session.claude_bin"] = srcSource("session.claude_bin")
	}
	if src.Template != nil {
		dst.Template = src.Template
		sources["session.template"] = srcSource("session.template")
	}
	if src.ClaudeVersion != nil {
		dst.ClaudeVersion = src.ClaudeVersion
		sources["session.claude_version"] = srcSource("session.claude_version")
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
	if src.PermissionMode != nil {
		dst.PermissionMode = src.PermissionMode
		sources["session.permission_mode"] = srcSource("session.permission_mode")
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

// mergeState is intentionally not called from merge() above: [state] has
// its own pipeline (LoadState, load.go) with no repo-file stage and no
// request stage, so it is merged by LoadState directly rather than folded
// into the daemon/session/attach merge every LoadDaemon/LoadSession call
// would otherwise perform (which would pollute their Sources maps with
// "state.*" keys neither call site returns).
func mergeState(dst, src *stateLayer, srcSource sourceFunc, sources map[string]string) {
	if src.StaleAfter != nil {
		dst.StaleAfter = src.StaleAfter
		sources["state.stale_after"] = srcSource("state.stale_after")
	}
	if src.FirstHookGrace != nil {
		dst.FirstHookGrace = src.FirstHookGrace
		sources["state.first_hook_grace"] = srcSource("state.first_hook_grace")
	}
	if src.PendingToolTTL != nil {
		dst.PendingToolTTL = src.PendingToolTTL
		sources["state.pending_tool_ttl"] = srcSource("state.pending_tool_ttl")
	}
	if src.MaxEventPayloadBytes != nil {
		dst.MaxEventPayloadBytes = src.MaxEventPayloadBytes
		sources["state.max_event_payload_bytes"] = srcSource("state.max_event_payload_bytes")
	}
	if src.PersistHookEvents != nil {
		dst.PersistHookEvents = src.PersistHookEvents
		sources["state.persist_hook_events"] = srcSource("state.persist_hook_events")
	}
	if src.HookTimeout != nil {
		dst.HookTimeout = src.HookTimeout
		sources["state.hook_timeout"] = srcSource("state.hook_timeout")
	}
	if src.PermissionSettle != nil {
		dst.PermissionSettle = src.PermissionSettle
		sources["state.permission_settle"] = srcSource("state.permission_settle")
	}
	if src.PermissionTTL != nil {
		dst.PermissionTTL = src.PermissionTTL
		sources["state.permission_ttl"] = srcSource("state.permission_ttl")
	}
	if src.IdleTimeout != nil {
		dst.IdleTimeout = src.IdleTimeout
		sources["state.idle_timeout"] = srcSource("state.idle_timeout")
	}
}

// mergeNotify is intentionally not called from merge() above: [notify] has
// its own pipeline (LoadNotify, load.go) with no repo-file stage and no
// request stage, so it is merged by LoadNotify directly rather than folded
// into the daemon/session/attach merge every LoadDaemon/LoadSession call
// would otherwise perform (which would pollute their Sources maps with
// "notify.*" keys neither call site returns).
func mergeNotify(dst, src *notifyLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Enabled != nil {
		dst.Enabled = src.Enabled
		sources["notify.enabled"] = srcSource("notify.enabled")
	}
	if src.On != nil {
		dst.On = src.On
		sources["notify.on"] = srcSource("notify.on")
	}
	if src.Debounce != nil {
		dst.Debounce = src.Debounce
		sources["notify.debounce"] = srcSource("notify.debounce")
	}
	if src.Timeout != nil {
		dst.Timeout = src.Timeout
		sources["notify.timeout"] = srcSource("notify.timeout")
	}
	if src.Retries != nil {
		dst.Retries = src.Retries
		sources["notify.retries"] = srcSource("notify.retries")
	}
	mergeNotifyNtfy(&dst.Ntfy, &src.Ntfy, srcSource, sources)
	mergeNotifyWebhook(&dst.Webhook, &src.Webhook, srcSource, sources)
}

func mergeNotifyNtfy(dst, src *notifyNtfyLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Enabled != nil {
		dst.Enabled = src.Enabled
		sources["notify.ntfy.enabled"] = srcSource("notify.ntfy.enabled")
	}
	if src.Server != nil {
		dst.Server = src.Server
		sources["notify.ntfy.server"] = srcSource("notify.ntfy.server")
	}
	if src.Topic != nil {
		dst.Topic = src.Topic
		sources["notify.ntfy.topic"] = srcSource("notify.ntfy.topic")
	}
	if src.Token != nil {
		dst.Token = src.Token
		sources["notify.ntfy.token"] = srcSource("notify.ntfy.token")
	}
	if src.Priority != nil {
		dst.Priority = src.Priority
		sources["notify.ntfy.priority"] = srcSource("notify.ntfy.priority")
	}
	mergeNotifyNtfyReply(&dst.Reply, &src.Reply, srcSource, sources)
}

func mergeNotifyNtfyReply(dst, src *notifyNtfyReplyLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Enabled != nil {
		dst.Enabled = src.Enabled
		sources["notify.ntfy.reply.enabled"] = srcSource("notify.ntfy.reply.enabled")
	}
	if src.Topic != nil {
		dst.Topic = src.Topic
		sources["notify.ntfy.reply.topic"] = srcSource("notify.ntfy.reply.topic")
	}
	if src.Token != nil {
		dst.Token = src.Token
		sources["notify.ntfy.reply.token"] = srcSource("notify.ntfy.reply.token")
	}
}

func mergeNotifyWebhook(dst, src *notifyWebhookLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Enabled != nil {
		dst.Enabled = src.Enabled
		sources["notify.webhook.enabled"] = srcSource("notify.webhook.enabled")
	}
	if src.URL != nil {
		dst.URL = src.URL
		sources["notify.webhook.url"] = srcSource("notify.webhook.url")
	}
	if src.Headers != nil {
		dst.Headers = src.Headers
		sources["notify.webhook.headers"] = srcSource("notify.webhook.headers")
	}
}

// mergeClient is intentionally not called by merge() above: [client] has
// its own pipeline (LoadClient, load.go) with no repo-file stage and no
// request stage, so it is merged by LoadClient directly rather than being
// folded into the daemon/session/attach merge every LoadDaemon/LoadSession
// call would otherwise perform (which would pollute those calls' Sources
// maps with "client.*" keys neither call site returns).
func mergeClient(dst, src *clientLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Host != nil {
		dst.Host = src.Host
		sources["client.host"] = srcSource("client.host")
	}
	if src.Token != nil {
		dst.Token = src.Token
		sources["client.token"] = srcSource("client.token")
	}
	if src.CACert != nil {
		dst.CACert = src.CACert
		sources["client.cacert"] = srcSource("client.cacert")
	}
}

func mergeLearn(dst, src *learnLayer, srcSource sourceFunc, sources map[string]string) {
	if src.Window != nil {
		dst.Window = src.Window
		sources["learn.window"] = srcSource("learn.window")
	}
	if src.MinApprovals != nil {
		dst.MinApprovals = src.MinApprovals
		sources["learn.min_approvals"] = srcSource("learn.min_approvals")
	}
	if src.TTL != nil {
		dst.TTL = src.TTL
		sources["learn.ttl"] = srcSource("learn.ttl")
	}
	if src.MinSessions != nil {
		dst.MinSessions = src.MinSessions
		sources["learn.min_sessions"] = srcSource("learn.min_sessions")
	}
	if src.MinTerminalTasks != nil {
		dst.MinTerminalTasks = src.MinTerminalTasks
		sources["learn.min_terminal_tasks"] = srcSource("learn.min_terminal_tasks")
	}
	if src.CostRegressionTolerance != nil {
		dst.CostRegressionTolerance = src.CostRegressionTolerance
		sources["learn.cost_regression_tolerance"] = srcSource("learn.cost_regression_tolerance")
	}
}
