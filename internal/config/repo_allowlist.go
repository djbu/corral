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
	// daemon.listen / daemon.tls_cert / daemon.tls_key: a trust-boundary
	// rule, not just an omission — a cloned repo's .corral.toml must never
	// be able to open a network port or swap the daemon's TLS cert (design
	// doc m5.md §5, step 30).
	if l.Daemon.Listen != nil {
		reject("daemon.listen")
	}
	if l.Daemon.TLSCert != nil {
		reject("daemon.tls_cert")
	}
	if l.Daemon.TLSKey != nil {
		reject("daemon.tls_key")
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
	if l.State.IdleTimeout != nil {
		reject("state.idle_timeout")
	}

	// [notify] — nothing is repo-settable (design doc §8.7): a repo's
	// .corral.toml is attacker-controlled, and a cloned repo setting
	// notify.webhook.url or notify.ntfy.token could exfiltrate blocked/
	// exited reasons to an attacker-controlled endpoint; a cloned repo
	// setting notify.ntfy.reply.topic/token could grant PTY keystroke
	// access via the reply channel. Entirely user-file/env only, resolved
	// by its own LoadNotify pipeline which never consults a repo file at
	// all. These checks exist so a repo file that sets [notify] keys
	// anyway is still reported as a Rejection rather than silently
	// ignored.
	if l.Notify.Enabled != nil {
		reject("notify.enabled")
	}
	if l.Notify.On != nil {
		reject("notify.on")
	}
	if l.Notify.Debounce != nil {
		reject("notify.debounce")
	}
	if l.Notify.Timeout != nil {
		reject("notify.timeout")
	}
	if l.Notify.Retries != nil {
		reject("notify.retries")
	}
	if l.Notify.Ntfy.Enabled != nil {
		reject("notify.ntfy.enabled")
	}
	if l.Notify.Ntfy.Server != nil {
		reject("notify.ntfy.server")
	}
	if l.Notify.Ntfy.Topic != nil {
		reject("notify.ntfy.topic")
	}
	if l.Notify.Ntfy.Token != nil {
		reject("notify.ntfy.token")
	}
	if l.Notify.Ntfy.Priority != nil {
		reject("notify.ntfy.priority")
	}
	if l.Notify.Ntfy.Reply.Enabled != nil {
		reject("notify.ntfy.reply.enabled")
	}
	if l.Notify.Ntfy.Reply.Topic != nil {
		reject("notify.ntfy.reply.topic")
	}
	if l.Notify.Ntfy.Reply.Token != nil {
		reject("notify.ntfy.reply.token")
	}
	if l.Notify.Webhook.Enabled != nil {
		reject("notify.webhook.enabled")
	}
	if l.Notify.Webhook.URL != nil {
		reject("notify.webhook.url")
	}
	if len(l.Notify.Webhook.Headers) > 0 {
		reject("notify.webhook.headers")
	}

	return out, rej
}
