package config

import (
	"path/filepath"
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
