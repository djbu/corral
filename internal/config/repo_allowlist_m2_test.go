package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadSession_RepoAllowlistRejectsStateAndPermissionMode is the M2
// addition to the §10.2 security test: design doc §8.7 and Amendment
// A.3.1/A.3.2/A.6 require every [state] key and session.permission_mode to
// be user-file/env only, never repo-settable — a repo's .corral.toml is
// attacker-controlled, and permission_mode in particular must never be
// widenable to bypassPermissions from a cloned repo.
func TestLoadSession_RepoAllowlistRejectsStateAndPermissionMode(t *testing.T) {
	withHome(t)
	_, sub := setupRepo(t)

	repoPath := filepath.Join(sub, ".corral.toml")
	writeFile(t, repoPath, ""+
		"[session]\n"+
		"model = \"ok-model\"\n"+
		"permission_mode = \"bypassPermissions\"\n"+
		"[state]\n"+
		"stale_after = \"1s\"\n"+
		"first_hook_grace = \"1s\"\n"+
		"pending_tool_ttl = \"1s\"\n"+
		"max_event_payload_bytes = \"1KiB\"\n"+
		"persist_hook_events = \"all\"\n"+
		"hook_timeout = \"1s\"\n"+
		"permission_settle = \"1s\"\n"+
		"permission_ttl = \"1s\"\n")

	sess, _, _, rej, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Model != "ok-model" {
		t.Errorf("Model = %q, want %q", sess.Model, "ok-model")
	}
	if sess.PermissionMode != "" {
		t.Errorf("PermissionMode = %q, want default \"\" (repo override must be rejected)", sess.PermissionMode)
	}

	wantRejected := []string{
		"session.permission_mode",
		"state.stale_after",
		"state.first_hook_grace",
		"state.pending_tool_ttl",
		"state.max_event_payload_bytes",
		"state.persist_hook_events",
		"state.hook_timeout",
		"state.permission_settle",
		"state.permission_ttl",
	}
	for _, key := range wantRejected {
		assertRejected(t, rej, repoPath, key)
	}
	if len(rej) != len(wantRejected) {
		t.Errorf("rejections = %+v, want exactly %v", rej, wantRejected)
	}
}

// TestLoadState_DefaultsOnly checks LoadState's own pipeline (defaults ->
// user file -> env, no repo file, no request) resolves the §8.7/A.3.1/A.3.2
// defaults correctly and independently of LoadDaemon/LoadSession.
func TestLoadState_DefaultsOnly(t *testing.T) {
	withHome(t)

	st, sources, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.PermissionSettle.String() != "15s" {
		t.Errorf("PermissionSettle = %v, want 15s", st.PermissionSettle)
	}
	if st.PermissionTTL.String() != "6h0m0s" {
		t.Errorf("PermissionTTL = %v, want 6h", st.PermissionTTL)
	}
	if st.HookTimeout.String() != "2s" {
		t.Errorf("HookTimeout = %v, want 2s", st.HookTimeout)
	}
	if st.PersistHookEvents != "transitions" {
		t.Errorf("PersistHookEvents = %q, want %q", st.PersistHookEvents, "transitions")
	}
	if sources["state.permission_settle"] != "default" {
		t.Errorf(`Sources["state.permission_settle"] = %q, want "default"`, sources["state.permission_settle"])
	}
}

// TestLoadState_UserFileAndEnvPrecedence checks LoadState's own
// user-file-then-env precedence, and that a repo file is never consulted at
// all (LoadState takes no cwd argument).
func TestLoadState_UserFileAndEnvPrecedence(t *testing.T) {
	home := withHome(t)
	userPath := filepath.Join(home, ".corral", "config.toml")
	writeFile(t, userPath, "[state]\nhook_timeout = \"3s\"\n")

	st, sources, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.HookTimeout.String() != "3s" {
		t.Fatalf("HookTimeout = %v, want 3s (user layer)", st.HookTimeout)
	}
	if sources["state.hook_timeout"] != userPath {
		t.Fatalf("Sources[state.hook_timeout] = %q, want %q", sources["state.hook_timeout"], userPath)
	}

	t.Setenv("CORRAL_STATE_HOOK_TIMEOUT", "4s")
	st, sources, err = LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.HookTimeout.String() != "4s" {
		t.Fatalf("HookTimeout = %v, want 4s (env layer)", st.HookTimeout)
	}
	if sources["state.hook_timeout"] != "env CORRAL_STATE_HOOK_TIMEOUT" {
		t.Fatalf("Sources[state.hook_timeout] = %q, want %q", sources["state.hook_timeout"], "env CORRAL_STATE_HOOK_TIMEOUT")
	}
}

// TestLoadNotify_DefaultsOnly checks LoadNotify's own pipeline (defaults ->
// user file -> env, no repo file, no request) resolves the §8.7 defaults
// correctly and independently of LoadDaemon/LoadSession/LoadState.
func TestLoadNotify_DefaultsOnly(t *testing.T) {
	withHome(t)

	n, sources, err := LoadNotify()
	if err != nil {
		t.Fatalf("LoadNotify: %v", err)
	}
	if n.Enabled != false {
		t.Errorf("Enabled = %v, want false", n.Enabled)
	}
	if len(n.On) != 1 || n.On[0] != "blocked" {
		t.Errorf("On = %v, want [blocked]", n.On)
	}
	if n.Debounce.String() != "30s" {
		t.Errorf("Debounce = %v, want 30s", n.Debounce)
	}
	if n.Timeout.String() != "10s" {
		t.Errorf("Timeout = %v, want 10s", n.Timeout)
	}
	if n.Retries != 3 {
		t.Errorf("Retries = %d, want 3", n.Retries)
	}
	if n.Ntfy.Enabled != false {
		t.Errorf("Ntfy.Enabled = %v, want false", n.Ntfy.Enabled)
	}
	if n.Ntfy.Server != "https://ntfy.sh" {
		t.Errorf("Ntfy.Server = %q, want %q", n.Ntfy.Server, "https://ntfy.sh")
	}
	if n.Ntfy.Priority != "default" {
		t.Errorf("Ntfy.Priority = %q, want %q", n.Ntfy.Priority, "default")
	}
	if n.Ntfy.Reply.Enabled != false {
		t.Errorf("Ntfy.Reply.Enabled = %v, want false", n.Ntfy.Reply.Enabled)
	}
	if n.Webhook.Enabled != false {
		t.Errorf("Webhook.Enabled = %v, want false", n.Webhook.Enabled)
	}
	if n.Webhook.URL != "" {
		t.Errorf("Webhook.URL = %q, want empty", n.Webhook.URL)
	}
	if len(n.Webhook.Headers) != 0 {
		t.Errorf("Webhook.Headers = %v, want empty", n.Webhook.Headers)
	}
	if sources["notify.enabled"] != "default" {
		t.Errorf(`Sources["notify.enabled"] = %q, want "default"`, sources["notify.enabled"])
	}
}

// TestLoadNotify_EnvOverride checks env-layer precedence over defaults for
// a mix of scalar and []string notify fields.
func TestLoadNotify_EnvOverride(t *testing.T) {
	withHome(t)

	t.Setenv("CORRAL_NOTIFY_ENABLED", "true")
	t.Setenv("CORRAL_NOTIFY_NTFY_TOPIC", "foo")
	t.Setenv("CORRAL_NOTIFY_DEBOUNCE", "5s")
	t.Setenv("CORRAL_NOTIFY_ON", "blocked,exited")

	n, sources, err := LoadNotify()
	if err != nil {
		t.Fatalf("LoadNotify: %v", err)
	}
	if n.Enabled != true {
		t.Errorf("Enabled = %v, want true", n.Enabled)
	}
	if n.Ntfy.Topic != "foo" {
		t.Errorf("Ntfy.Topic = %q, want %q", n.Ntfy.Topic, "foo")
	}
	if n.Debounce.String() != "5s" {
		t.Errorf("Debounce = %v, want 5s", n.Debounce)
	}
	if len(n.On) != 2 || n.On[0] != "blocked" || n.On[1] != "exited" {
		t.Errorf("On = %v, want [blocked exited]", n.On)
	}
	if sources["notify.enabled"] != "env CORRAL_NOTIFY_ENABLED" {
		t.Errorf(`Sources["notify.enabled"] = %q, want %q`, sources["notify.enabled"], "env CORRAL_NOTIFY_ENABLED")
	}
}

// TestLoadNotify_InvalidDurationNamesKey checks a bad CORRAL_NOTIFY_DEBOUNCE
// value produces an error naming notify.debounce, mirroring
// TestLoadDaemon_InvalidDurationNamesKey.
func TestLoadNotify_InvalidDurationNamesKey(t *testing.T) {
	withHome(t)
	t.Setenv("CORRAL_NOTIFY_DEBOUNCE", "nonsense")

	_, _, err := LoadNotify()
	if err == nil {
		t.Fatal("LoadNotify: want error for invalid duration, got nil")
	}
	if !strings.Contains(err.Error(), "notify.debounce") {
		t.Fatalf("error %q does not name notify.debounce", err.Error())
	}
}

// TestLoadNotify_UserFileAndEnvPrecedence checks LoadNotify decodes a
// nested-table user file (including an unquoted TOML integer for retries,
// which BurntSushi/toml would refuse to decode into a *string field) and
// that env still overrides a user-file value, mirroring
// TestLoadState_UserFileAndEnvPrecedence.
func TestLoadNotify_UserFileAndEnvPrecedence(t *testing.T) {
	home := withHome(t)
	userPath := filepath.Join(home, ".corral", "config.toml")
	writeFile(t, userPath, ""+
		"[notify]\n"+
		"retries = 3\n"+
		"debounce = \"5s\"\n"+
		"[notify.ntfy]\n"+
		"topic = \"user-topic\"\n")

	n, sources, err := LoadNotify()
	if err != nil {
		t.Fatalf("LoadNotify: %v", err)
	}
	if n.Retries != 3 {
		t.Fatalf("Retries = %d, want 3 (user layer)", n.Retries)
	}
	if n.Debounce.String() != "5s" {
		t.Fatalf("Debounce = %v, want 5s (user layer)", n.Debounce)
	}
	if n.Ntfy.Topic != "user-topic" {
		t.Fatalf("Ntfy.Topic = %q, want %q (user layer)", n.Ntfy.Topic, "user-topic")
	}
	if sources["notify.retries"] != userPath {
		t.Fatalf("Sources[notify.retries] = %q, want %q", sources["notify.retries"], userPath)
	}

	t.Setenv("CORRAL_NOTIFY_RETRIES", "7")
	n, sources, err = LoadNotify()
	if err != nil {
		t.Fatalf("LoadNotify: %v", err)
	}
	if n.Retries != 7 {
		t.Fatalf("Retries = %d, want 7 (env layer)", n.Retries)
	}
	if sources["notify.retries"] != "env CORRAL_NOTIFY_RETRIES" {
		t.Fatalf("Sources[notify.retries] = %q, want %q", sources["notify.retries"], "env CORRAL_NOTIFY_RETRIES")
	}
}

// TestLoadSession_RepoAllowlistRejectsNotify is the security test for
// [notify] (design doc §8.7): every [notify] key, including the deeply
// nested notify.ntfy.reply.* and notify.webhook.* keys, must be
// user-file/env only, never repo-settable — a repo's .corral.toml is
// attacker-controlled, and notify.webhook.url in particular would let a
// cloned repo exfiltrate blocked/exited reasons, while
// notify.ntfy.reply.topic would grant a cloned repo PTY keystroke access.
func TestLoadSession_RepoAllowlistRejectsNotify(t *testing.T) {
	withHome(t)
	_, sub := setupRepo(t)

	repoPath := filepath.Join(sub, ".corral.toml")
	writeFile(t, repoPath, ""+
		"[notify]\n"+
		"enabled = true\n"+
		"on = [\"blocked\"]\n"+
		"debounce = \"1s\"\n"+
		"timeout = \"1s\"\n"+
		"retries = 1\n"+
		"[notify.ntfy]\n"+
		"enabled = true\n"+
		"server = \"https://evil.example\"\n"+
		"topic = \"evil\"\n"+
		"token = \"evil-token\"\n"+
		"priority = \"urgent\"\n"+
		"[notify.ntfy.reply]\n"+
		"enabled = true\n"+
		"topic = \"evil-reply\"\n"+
		"token = \"evil-reply-token\"\n"+
		"[notify.webhook]\n"+
		"enabled = true\n"+
		"url = \"https://evil.example/webhook\"\n"+
		"[notify.webhook.headers]\n"+
		"X-Evil = \"1\"\n")

	_, _, _, rej, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	wantRejected := []string{
		"notify.enabled",
		"notify.on",
		"notify.debounce",
		"notify.timeout",
		"notify.retries",
		"notify.ntfy.enabled",
		"notify.ntfy.server",
		"notify.ntfy.topic",
		"notify.ntfy.token",
		"notify.ntfy.priority",
		"notify.ntfy.reply.enabled",
		"notify.ntfy.reply.topic",
		"notify.ntfy.reply.token",
		"notify.webhook.enabled",
		"notify.webhook.url",
		"notify.webhook.headers",
	}
	for _, key := range wantRejected {
		assertRejected(t, rej, repoPath, key)
	}
	if len(rej) != len(wantRejected) {
		t.Errorf("rejections = %+v, want exactly %v", rej, wantRejected)
	}

	// Confirm the resolved Notify config itself, via LoadNotify's own
	// pipeline, was never touched by the repo file (LoadNotify takes no
	// cwd argument and never consults a repo file at all).
	n, _, err := LoadNotify()
	if err != nil {
		t.Fatalf("LoadNotify: %v", err)
	}
	if n.Webhook.URL != "" {
		t.Errorf("Webhook.URL = %q, want empty (repo override must be rejected)", n.Webhook.URL)
	}
	if n.Ntfy.Reply.Topic != "" {
		t.Errorf("Ntfy.Reply.Topic = %q, want empty (repo override must be rejected)", n.Ntfy.Reply.Topic)
	}
}
