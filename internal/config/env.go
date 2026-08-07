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

	{"session.claude_bin", "CORRAL_SESSION_CLAUDE_BIN", func(l *layer, v string) error {
		l.Session.ClaudeBin = strPtr(v)
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
