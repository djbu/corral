package config

import "fmt"

// EffectiveResult is the fully-resolved daemon+session+attach config for a
// given cwd, flattened into the same "section.key" -> value shape both
// `corral config` and GET /v1/config render (design doc §8.4, §9.2).
type EffectiveResult struct {
	Values  map[string]string
	Sources map[string]string
	Ignored []Rejection
}

// Effective resolves daemon config (defaults -> user file -> env) and
// session/attach config for cwd (defaults -> user file -> repo file ->
// env), merging both pipelines' source maps and their structs into one
// flat result. It is the single place this flattening logic lives, so
// `corral config` (cmd/corral/cmd_config.go) and the daemon's GET
// /v1/config handler render byte-identical output.
func Effective(cwd string) (EffectiveResult, error) {
	daemon, daemonSources, err := LoadDaemon()
	if err != nil {
		return EffectiveResult{}, err
	}
	sess, attach, sessionSources, rejections, err := LoadSession(cwd, nil)
	if err != nil {
		return EffectiveResult{}, err
	}

	values := map[string]string{
		"daemon.socket":                daemon.Socket,
		"daemon.state_dir":             daemon.StateDir,
		"daemon.log_level":             daemon.LogLevel,
		"daemon.log_format":            daemon.LogFormat,
		"daemon.shutdown_grace":        daemon.ShutdownGrace.String(),
		"session.claude_bin":           sess.ClaudeBin,
		"session.model":                sess.Model,
		"session.setting_sources":      sess.SettingSources,
		"session.env_passthrough":      fmt.Sprintf("%v", sess.EnvPassthrough),
		"session.term":                 sess.Term,
		"session.scrollback_lines":     fmt.Sprintf("%d", sess.ScrollbackLines),
		"session.output_log_max_bytes": fmt.Sprintf("%d", sess.OutputLogMaxBytes),
		"attach.prefix_key":            attach.PrefixKey,
		"attach.detach_key":            attach.DetachKey,
		"attach.ping_interval":         attach.PingInterval.String(),
		"attach.ping_timeout":          attach.PingTimeout.String(),
	}

	sources := make(map[string]string, len(daemonSources)+len(sessionSources))
	for k, v := range daemonSources {
		sources[k] = v
	}
	for k, v := range sessionSources {
		sources[k] = v
	}

	return EffectiveResult{Values: values, Sources: sources, Ignored: rejections}, nil
}
