package config

// Repo-file trust boundary (§8.2): a repo's .corral.toml is attacker-
// controlled content in any cloned repository, and the daemon executes a
// binary path and other behavior from session config. A repo file may set
// only these three keys; every other key present in a repo file is dropped
// and reported as a Rejection instead of being merged.
//
// In particular session.claude_bin, session.env_passthrough,
// session.setting_sources, and the entire [daemon] and [attach] tables are
// user-file/env only, never repo-file.

// filterRepoLayer returns a copy of l with every field not on the repo
// allowlist cleared to nil, plus one Rejection per cleared (i.e.
// originally-set) field, naming file and the "section.key" that was
// rejected.
func filterRepoLayer(file string, l *layer) (*layer, []Rejection) {
	var rej []Rejection
	reject := func(key string) {
		rej = append(rej, Rejection{File: file, Key: key})
	}

	out := &layer{
		Session: sessionLayer{
			Model:           l.Session.Model,
			ScrollbackLines: l.Session.ScrollbackLines,
			Term:            l.Session.Term,
		},
	}

	// [daemon] — nothing is repo-settable.
	if l.Daemon.Socket != nil {
		reject("daemon.socket")
	}
	if l.Daemon.StateDir != nil {
		reject("daemon.state_dir")
	}
	if l.Daemon.LogLevel != nil {
		reject("daemon.log_level")
	}
	if l.Daemon.LogFormat != nil {
		reject("daemon.log_format")
	}
	if l.Daemon.ShutdownGrace != nil {
		reject("daemon.shutdown_grace")
	}

	// [session] — only model, scrollback_lines, term are repo-settable.
	if l.Session.ClaudeBin != nil {
		reject("session.claude_bin")
	}
	if l.Session.SettingSources != nil {
		reject("session.setting_sources")
	}
	if l.Session.EnvPassthrough != nil {
		reject("session.env_passthrough")
	}
	if l.Session.OutputLogMaxBytes != nil {
		reject("session.output_log_max_bytes")
	}

	// [attach] — nothing is repo-settable.
	if l.Attach.PrefixKey != nil {
		reject("attach.prefix_key")
	}
	if l.Attach.DetachKey != nil {
		reject("attach.detach_key")
	}
	if l.Attach.PingInterval != nil {
		reject("attach.ping_interval")
	}
	if l.Attach.PingTimeout != nil {
		reject("attach.ping_timeout")
	}

	return out, rej
}
