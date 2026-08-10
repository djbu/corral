package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Env var rule, exactly one form (§8.3): CORRAL_<SECTION>_<KEY>, uppercased,
// "."->"_". No aliases, no parsing of arbitrary CORRAL_* variables — each
// key's exact variable name is listed explicitly below, forward-mapped,
// because keys like output_log_max_bytes already contain underscores and so
// are not unambiguously reversible from the variable name alone.

// envSpec describes how to decode one CORRAL_<SECTION>_<KEY> variable into
// its layer field.
type envSpec struct {
	key    string // "section.key", matches merge()'s Sources keys.
	envVar string // "CORRAL_SECTION_KEY"
	set    func(l *layer, raw string) error
}

var envSpecs = []envSpec{
	{"daemon.socket", "CORRAL_DAEMON_SOCKET", func(l *layer, v string) error {
		l.Daemon.Socket = strPtr(v)
		return nil
	}},
	{"daemon.state_dir", "CORRAL_DAEMON_STATE_DIR", func(l *layer, v string) error {
		l.Daemon.StateDir = strPtr(v)
		return nil
	}},
	{"daemon.log_level", "CORRAL_DAEMON_LOG_LEVEL", func(l *layer, v string) error {
		l.Daemon.LogLevel = strPtr(v)
		return nil
	}},
	{"daemon.log_format", "CORRAL_DAEMON_LOG_FORMAT", func(l *layer, v string) error {
		l.Daemon.LogFormat = strPtr(v)
		return nil
	}},
	{"daemon.shutdown_grace", "CORRAL_DAEMON_SHUTDOWN_GRACE", func(l *layer, v string) error {
		l.Daemon.ShutdownGrace = strPtr(v)
		return nil
	}},
	{"daemon.listen", "CORRAL_DAEMON_LISTEN", func(l *layer, v string) error {
		l.Daemon.Listen = strPtr(v)
		return nil
	}},
	{"daemon.tls_cert", "CORRAL_DAEMON_TLS_CERT", func(l *layer, v string) error {
		l.Daemon.TLSCert = strPtr(v)
		return nil
	}},
	{"daemon.tls_key", "CORRAL_DAEMON_TLS_KEY", func(l *layer, v string) error {
		l.Daemon.TLSKey = strPtr(v)
		return nil
	}},
	{"daemon.min_free_bytes", "CORRAL_DAEMON_MIN_FREE_BYTES", func(l *layer, v string) error {
		l.Daemon.MinFreeBytes = strPtr(v)
		return nil
	}},
	{"daemon.max_interactive_sessions", "CORRAL_DAEMON_MAX_INTERACTIVE_SESSIONS", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_DAEMON_MAX_INTERACTIVE_SESSIONS=%q: %w", v, err)
		}
		l.Daemon.MaxInteractiveSessions = intPtr(n)
		return nil
	}},
	{"daemon.max_headless_tasks", "CORRAL_DAEMON_MAX_HEADLESS_TASKS", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_DAEMON_MAX_HEADLESS_TASKS=%q: %w", v, err)
		}
		l.Daemon.MaxHeadlessTasks = intPtr(n)
		return nil
	}},
	{"daemon.max_pending_dag_tasks", "CORRAL_DAEMON_MAX_PENDING_DAG_TASKS", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_DAEMON_MAX_PENDING_DAG_TASKS=%q: %w", v, err)
		}
		l.Daemon.MaxPendingDAGTasks = intPtr(n)
		return nil
	}},

	{"session.claude_bin", "CORRAL_SESSION_CLAUDE_BIN", func(l *layer, v string) error {
		l.Session.ClaudeBin = strPtr(v)
		return nil
	}},
	{"session.template", "CORRAL_SESSION_TEMPLATE", func(l *layer, v string) error {
		l.Session.Template = strPtr(v)
		return nil
	}},
	{"session.claude_version", "CORRAL_SESSION_CLAUDE_VERSION", func(l *layer, v string) error {
		l.Session.ClaudeVersion = strPtr(v)
		return nil
	}},
	{"session.model", "CORRAL_SESSION_MODEL", func(l *layer, v string) error {
		l.Session.Model = strPtr(v)
		return nil
	}},
	{"session.setting_sources", "CORRAL_SESSION_SETTING_SOURCES", func(l *layer, v string) error {
		l.Session.SettingSources = strPtr(v)
		return nil
	}},
	{"session.env_passthrough", "CORRAL_SESSION_ENV_PASSTHROUGH", func(l *layer, v string) error {
		// Not specified by the doc; comma-separated list of extra env var
		// NAMES is the obvious encoding for a []string in a single env var.
		// Noted in the deviations list.
		l.Session.EnvPassthrough = strsPtr(splitCommaList(v))
		return nil
	}},
	{"session.term", "CORRAL_SESSION_TERM", func(l *layer, v string) error {
		l.Session.Term = strPtr(v)
		return nil
	}},
	{"session.scrollback_lines", "CORRAL_SESSION_SCROLLBACK_LINES", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_SESSION_SCROLLBACK_LINES=%q: %w", v, err)
		}
		l.Session.ScrollbackLines = intPtr(n)
		return nil
	}},
	{"session.output_log_max_bytes", "CORRAL_SESSION_OUTPUT_LOG_MAX_BYTES", func(l *layer, v string) error {
		l.Session.OutputLogMaxBytes = strPtr(v)
		return nil
	}},

	{"attach.prefix_key", "CORRAL_ATTACH_PREFIX_KEY", func(l *layer, v string) error {
		l.Attach.PrefixKey = strPtr(v)
		return nil
	}},
	{"attach.detach_key", "CORRAL_ATTACH_DETACH_KEY", func(l *layer, v string) error {
		l.Attach.DetachKey = strPtr(v)
		return nil
	}},
	{"attach.ping_interval", "CORRAL_ATTACH_PING_INTERVAL", func(l *layer, v string) error {
		l.Attach.PingInterval = strPtr(v)
		return nil
	}},
	{"attach.ping_timeout", "CORRAL_ATTACH_PING_TIMEOUT", func(l *layer, v string) error {
		l.Attach.PingTimeout = strPtr(v)
		return nil
	}},

	{"session.permission_mode", "CORRAL_SESSION_PERMISSION_MODE", func(l *layer, v string) error {
		l.Session.PermissionMode = strPtr(v)
		return nil
	}},

	{"state.stale_after", "CORRAL_STATE_STALE_AFTER", func(l *layer, v string) error {
		l.State.StaleAfter = strPtr(v)
		return nil
	}},
	{"state.first_hook_grace", "CORRAL_STATE_FIRST_HOOK_GRACE", func(l *layer, v string) error {
		l.State.FirstHookGrace = strPtr(v)
		return nil
	}},
	{"state.pending_tool_ttl", "CORRAL_STATE_PENDING_TOOL_TTL", func(l *layer, v string) error {
		l.State.PendingToolTTL = strPtr(v)
		return nil
	}},
	{"state.max_event_payload_bytes", "CORRAL_STATE_MAX_EVENT_PAYLOAD_BYTES", func(l *layer, v string) error {
		l.State.MaxEventPayloadBytes = strPtr(v)
		return nil
	}},
	{"state.persist_hook_events", "CORRAL_STATE_PERSIST_HOOK_EVENTS", func(l *layer, v string) error {
		l.State.PersistHookEvents = strPtr(v)
		return nil
	}},
	{"state.hook_timeout", "CORRAL_STATE_HOOK_TIMEOUT", func(l *layer, v string) error {
		l.State.HookTimeout = strPtr(v)
		return nil
	}},
	{"state.permission_settle", "CORRAL_STATE_PERMISSION_SETTLE", func(l *layer, v string) error {
		l.State.PermissionSettle = strPtr(v)
		return nil
	}},
	{"state.permission_ttl", "CORRAL_STATE_PERMISSION_TTL", func(l *layer, v string) error {
		l.State.PermissionTTL = strPtr(v)
		return nil
	}},
	{"state.idle_timeout", "CORRAL_STATE_IDLE_TIMEOUT", func(l *layer, v string) error {
		l.State.IdleTimeout = strPtr(v)
		return nil
	}},

	{"notify.enabled", "CORRAL_NOTIFY_ENABLED", func(l *layer, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_NOTIFY_ENABLED=%q: %w", v, err)
		}
		l.Notify.Enabled = boolPtr(b)
		return nil
	}},
	{"notify.on", "CORRAL_NOTIFY_ON", func(l *layer, v string) error {
		l.Notify.On = strsPtr(splitCommaList(v))
		return nil
	}},
	{"notify.debounce", "CORRAL_NOTIFY_DEBOUNCE", func(l *layer, v string) error {
		l.Notify.Debounce = strPtr(v)
		return nil
	}},
	{"notify.timeout", "CORRAL_NOTIFY_TIMEOUT", func(l *layer, v string) error {
		l.Notify.Timeout = strPtr(v)
		return nil
	}},
	{"notify.retries", "CORRAL_NOTIFY_RETRIES", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_NOTIFY_RETRIES=%q: %w", v, err)
		}
		l.Notify.Retries = intPtr(n)
		return nil
	}},
	{"notify.ntfy.enabled", "CORRAL_NOTIFY_NTFY_ENABLED", func(l *layer, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_NOTIFY_NTFY_ENABLED=%q: %w", v, err)
		}
		l.Notify.Ntfy.Enabled = boolPtr(b)
		return nil
	}},
	{"notify.ntfy.server", "CORRAL_NOTIFY_NTFY_SERVER", func(l *layer, v string) error {
		l.Notify.Ntfy.Server = strPtr(v)
		return nil
	}},
	{"notify.ntfy.topic", "CORRAL_NOTIFY_NTFY_TOPIC", func(l *layer, v string) error {
		l.Notify.Ntfy.Topic = strPtr(v)
		return nil
	}},
	{"notify.ntfy.token", "CORRAL_NOTIFY_NTFY_TOKEN", func(l *layer, v string) error {
		l.Notify.Ntfy.Token = strPtr(v)
		return nil
	}},
	{"notify.ntfy.priority", "CORRAL_NOTIFY_NTFY_PRIORITY", func(l *layer, v string) error {
		l.Notify.Ntfy.Priority = strPtr(v)
		return nil
	}},
	{"notify.ntfy.reply.enabled", "CORRAL_NOTIFY_NTFY_REPLY_ENABLED", func(l *layer, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_NOTIFY_NTFY_REPLY_ENABLED=%q: %w", v, err)
		}
		l.Notify.Ntfy.Reply.Enabled = boolPtr(b)
		return nil
	}},
	{"notify.ntfy.reply.topic", "CORRAL_NOTIFY_NTFY_REPLY_TOPIC", func(l *layer, v string) error {
		l.Notify.Ntfy.Reply.Topic = strPtr(v)
		return nil
	}},
	{"notify.ntfy.reply.token", "CORRAL_NOTIFY_NTFY_REPLY_TOKEN", func(l *layer, v string) error {
		l.Notify.Ntfy.Reply.Token = strPtr(v)
		return nil
	}},
	{"notify.webhook.enabled", "CORRAL_NOTIFY_WEBHOOK_ENABLED", func(l *layer, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_NOTIFY_WEBHOOK_ENABLED=%q: %w", v, err)
		}
		l.Notify.Webhook.Enabled = boolPtr(b)
		return nil
	}},
	{"notify.webhook.url", "CORRAL_NOTIFY_WEBHOOK_URL", func(l *layer, v string) error {
		l.Notify.Webhook.URL = strPtr(v)
		return nil
	}},

	// [client] (design doc m5.md §7): client.token and client.cacert are
	// deliberately env/user-file only, never CLI flags — see cmd/corral/
	// remote.go's clientFlags doc comment for why.
	{"client.host", "CORRAL_CLIENT_HOST", func(l *layer, v string) error {
		l.Client.Host = strPtr(v)
		return nil
	}},
	{"client.token", "CORRAL_CLIENT_TOKEN", func(l *layer, v string) error {
		l.Client.Token = strPtr(v)
		return nil
	}},
	{"client.cacert", "CORRAL_CLIENT_CACERT", func(l *layer, v string) error {
		l.Client.CACert = strPtr(v)
		return nil
	}},

	{"learn.window", "CORRAL_LEARN_WINDOW", func(l *layer, v string) error {
		l.Learn.Window = strPtr(v)
		return nil
	}},
	{"learn.min_approvals", "CORRAL_LEARN_MIN_APPROVALS", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_LEARN_MIN_APPROVALS=%q: %w", v, err)
		}
		l.Learn.MinApprovals = intPtr(n)
		return nil
	}},
	{"learn.ttl", "CORRAL_LEARN_TTL", func(l *layer, v string) error {
		l.Learn.TTL = strPtr(v)
		return nil
	}},
	{"learn.min_sessions", "CORRAL_LEARN_MIN_SESSIONS", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_LEARN_MIN_SESSIONS=%q: %w", v, err)
		}
		l.Learn.MinSessions = intPtr(n)
		return nil
	}},
	{"learn.min_terminal_tasks", "CORRAL_LEARN_MIN_TERMINAL_TASKS", func(l *layer, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: CORRAL_LEARN_MIN_TERMINAL_TASKS=%q: %w", v, err)
		}
		l.Learn.MinTerminalTasks = intPtr(n)
		return nil
	}},
	{"learn.cost_regression_tolerance", "CORRAL_LEARN_COST_REGRESSION_TOLERANCE", func(l *layer, v string) error {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: CORRAL_LEARN_COST_REGRESSION_TOLERANCE=%q: %w", v, err)
		}
		l.Learn.CostRegressionTolerance = floatPtr(n)
		return nil
	}},
}

func splitCommaList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// lookupFunc mirrors os.LookupEnv's signature. Injected so tests decode
// against a fake environment instead of mutating the process's.
type lookupFunc func(key string) (string, bool)

// layerFromEnv builds a layer from every CORRAL_<SECTION>_<KEY> variable
// present according to lookup, along with the per-key source label
// ("env CORRAL_X") merge() should record for each.
func layerFromEnv(lookup lookupFunc) (*layer, map[string]string, error) {
	l := newLayer()
	sources := make(map[string]string)
	for _, spec := range envSpecs {
		raw, ok := lookup(spec.envVar)
		if !ok {
			continue
		}
		if err := spec.set(l, raw); err != nil {
			return nil, nil, err
		}
		sources[spec.key] = "env " + spec.envVar
	}
	return l, sources, nil
}
