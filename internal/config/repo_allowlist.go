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
	// PermissionMode is user/env-settable only, never repo-settable (design
	// doc §8.7, Amendment A.6): a repo's .corral.toml must never be able to
	// widen a session's approval posture to bypassPermissions.
	if l.Session.PermissionMode != nil {
		reject("session.permission_mode")
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

	// [state] — nothing is repo-settable (design doc §8.7, Amendment
	// A.3.1/A.3.2): entirely user-file/env only, resolved by its own
	// LoadState pipeline which never consults a repo file at all. These
	// checks exist so a repo file that sets [state] keys anyway is still
	// reported as a Rejection rather than silently ignored.
	if l.State.StaleAfter != nil {
		reject("state.stale_after")
	}
	if l.State.FirstHookGrace != nil {
		reject("state.first_hook_grace")
	}
	if l.State.PendingToolTTL != nil {
		reject("state.pending_tool_ttl")
	}
	if l.State.MaxEventPayloadBytes != nil {
		reject("state.max_event_payload_bytes")
	}
	if l.State.PersistHookEvents != nil {
		reject("state.persist_hook_events")
	}
	if l.State.HookTimeout != nil {
		reject("state.hook_timeout")
	}
	if l.State.PermissionSettle != nil {
		reject("state.permission_settle")
	}
	if l.State.PermissionTTL != nil {
		reject("state.permission_ttl")
	}

	return out, rej
}
