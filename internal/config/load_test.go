package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withHome points $HOME at a fresh temp dir for the duration of the test,
// so LoadDaemon/LoadSession's "~/.corral/config.toml" resolves somewhere
// disposable instead of the real user's home.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestLoadDaemon_DefaultsOnly(t *testing.T) {
	withHome(t)

	d, sources, err := LoadDaemon()
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", d.LogLevel, "info")
	}
	if d.ShutdownGrace != 5*time.Second {
		t.Errorf("ShutdownGrace = %v, want 5s", d.ShutdownGrace)
	}
	if sources["daemon.log_level"] != "default" {
		t.Errorf(`Sources["daemon.log_level"] = %q, want "default"`, sources["daemon.log_level"])
	}
	if d.MinFreeBytes != 256<<20 {
		t.Errorf("MinFreeBytes = %d, want %d", d.MinFreeBytes, 256<<20)
	}
	if sources["daemon.min_free_bytes"] != "default" {
		t.Errorf(`Sources["daemon.min_free_bytes"] = %q, want "default"`, sources["daemon.min_free_bytes"])
	}
}

func TestLoadDaemonMinFreeBytesEnv(t *testing.T) {
	withHome(t)
	t.Setenv("CORRAL_DAEMON_MIN_FREE_BYTES", "1GiB")
	d, sources, err := LoadDaemon()
	if err != nil {
		t.Fatal(err)
	}
	if d.MinFreeBytes != 1<<30 {
		t.Fatalf("MinFreeBytes = %d, want %d", d.MinFreeBytes, 1<<30)
	}
	if sources["daemon.min_free_bytes"] != "env CORRAL_DAEMON_MIN_FREE_BYTES" {
		t.Fatalf("source = %q", sources["daemon.min_free_bytes"])
	}
}

func TestRepoCannotSetMinFreeBytes(t *testing.T) {
	withHome(t)
	_, sub := setupRepo(t)
	repoPath := filepath.Join(sub, ".corral.toml")
	writeFile(t, repoPath, "[daemon]\nmin_free_bytes = \"0B\"\n")
	_, _, _, rejected, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 1 || rejected[0].Key != "daemon.min_free_bytes" {
		t.Fatalf("rejected = %+v", rejected)
	}
}

func TestLoadDaemon_Precedence(t *testing.T) {
	home := withHome(t)
	userPath := filepath.Join(home, ".corral", "config.toml")

	// user overrides default.
	writeFile(t, userPath, "[daemon]\nlog_level = \"debug\"\n")
	d, sources, err := LoadDaemon()
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want %q (user layer)", d.LogLevel, "debug")
	}
	if sources["daemon.log_level"] != userPath {
		t.Fatalf("Sources[daemon.log_level] = %q, want %q", sources["daemon.log_level"], userPath)
	}

	// env overrides user.
	t.Setenv("CORRAL_DAEMON_LOG_LEVEL", "warn")
	d, sources, err = LoadDaemon()
	if err != nil {
		t.Fatalf("LoadDaemon: %v", err)
	}
	if d.LogLevel != "warn" {
		t.Fatalf("LogLevel = %q, want %q (env layer)", d.LogLevel, "warn")
	}
	if sources["daemon.log_level"] != "env CORRAL_DAEMON_LOG_LEVEL" {
		t.Fatalf("Sources[daemon.log_level] = %q, want %q", sources["daemon.log_level"], "env CORRAL_DAEMON_LOG_LEVEL")
	}
}

func TestLoadDaemon_MalformedTOMLNamesFile(t *testing.T) {
	home := withHome(t)
	userPath := filepath.Join(home, ".corral", "config.toml")
	writeFile(t, userPath, "this is not [valid toml")

	_, _, err := LoadDaemon()
	if err == nil {
		t.Fatal("LoadDaemon: want error for malformed TOML, got nil")
	}
	if !strings.Contains(err.Error(), userPath) {
		t.Fatalf("error %q does not name the malformed file %q", err.Error(), userPath)
	}
}

func TestLoadDaemon_InvalidDurationNamesKey(t *testing.T) {
	home := withHome(t)
	writeFile(t, filepath.Join(home, ".corral", "config.toml"), "[daemon]\nshutdown_grace = \"not-a-duration\"\n")

	_, _, err := LoadDaemon()
	if err == nil {
		t.Fatal("LoadDaemon: want error for invalid duration, got nil")
	}
	if !strings.Contains(err.Error(), "daemon.shutdown_grace") {
		t.Fatalf("error %q does not name daemon.shutdown_grace", err.Error())
	}
}

// setupRepo builds tempHome/.git-rooted/sub as a git-root repo, with an
// optional .corral.toml at the given relative path (usually "sub" or
// "" for the root itself). It returns the git root and the sub directory
// session config should be loaded "from" (cwd).
func setupRepo(t *testing.T) (root, sub string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	sub = filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll sub: %v", err)
	}
	return root, sub
}

func TestLoadSession_DefaultsOnly(t *testing.T) {
	withHome(t)
	_, sub := setupRepo(t)

	sess, att, sources, rej, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.ClaudeBin != "claude" || sess.Model != "" || sess.ScrollbackLines != 2000 {
		t.Fatalf("unexpected defaults-only session: %+v", sess)
	}
	if att.DetachKey != "d" {
		t.Fatalf("unexpected defaults-only attach: %+v", att)
	}
	if len(rej) != 0 {
		t.Fatalf("rejections = %v, want none", rej)
	}
	if sources["session.claude_bin"] != "default" {
		t.Fatalf(`Sources[session.claude_bin] = %q, want "default"`, sources["session.claude_bin"])
	}
}

func TestLoadSession_FullPrecedenceChain(t *testing.T) {
	home := withHome(t)
	root, sub := setupRepo(t)

	userPath := filepath.Join(home, ".corral", "config.toml")
	writeFile(t, userPath, ""+
		"[session]\n"+
		"model = \"user-model\"\n"+
		"scrollback_lines = 500\n"+
		"claude_bin = \"user-claude-bin\"\n")

	repoPath := filepath.Join(sub, ".corral.toml")
	writeFile(t, repoPath, ""+
		"[session]\n"+
		"model = \"repo-model\"\n"+ // allowed: overrides user
		"term = \"repo-term\"\n"+ // allowed
		"claude_bin = \"repo-claude-bin\"\n"+ // NOT allowed: must be rejected
		"[daemon]\n"+
		"socket = \"/tmp/evil.sock\"\n") // NOT allowed: must be rejected

	// Step 1: defaults -> user -> repo (no env, no request).
	sess, _, sources, rej, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Model != "repo-model" {
		t.Errorf("Model = %q, want %q (repo layer)", sess.Model, "repo-model")
	}
	if sources["session.model"] != repoPath {
		t.Errorf("Sources[session.model] = %q, want %q", sources["session.model"], repoPath)
	}
	if sess.Term != "repo-term" {
		t.Errorf("Term = %q, want %q (repo layer)", sess.Term, "repo-term")
	}
	if sess.ScrollbackLines != 500 {
		t.Errorf("ScrollbackLines = %d, want 500 (user layer, untouched by repo)", sess.ScrollbackLines)
	}
	// claude_bin: repo's attempt must be rejected; user's value must still
	// win over the default, proving rejection isn't "ignore the whole file".
	if sess.ClaudeBin != "user-claude-bin" {
		t.Errorf("ClaudeBin = %q, want %q (repo override rejected, user layer wins)", sess.ClaudeBin, "user-claude-bin")
	}
	if sources["session.claude_bin"] != userPath {
		t.Errorf("Sources[session.claude_bin] = %q, want %q", sources["session.claude_bin"], userPath)
	}
	assertRejected(t, rej, repoPath, "session.claude_bin")
	assertRejected(t, rej, repoPath, "daemon.socket")

	// Step 2: env overrides repo.
	t.Setenv("CORRAL_SESSION_MODEL", "env-model")
	sess, _, sources, _, err = LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Model != "env-model" {
		t.Errorf("Model = %q, want %q (env layer)", sess.Model, "env-model")
	}
	if sources["session.model"] != "env CORRAL_SESSION_MODEL" {
		t.Errorf("Sources[session.model] = %q, want %q", sources["session.model"], "env CORRAL_SESSION_MODEL")
	}

	// Step 3: request overrides env.
	req := &layer{Session: sessionLayer{Model: strPtr("request-model")}}
	sess, _, sources, _, err = LoadSession(sub, req)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Model != "request-model" {
		t.Errorf("Model = %q, want %q (request layer)", sess.Model, "request-model")
	}
	if sources["session.model"] != "request" {
		t.Errorf("Sources[session.model] = %q, want %q", sources["session.model"], "request")
	}

	_ = root
}

func assertRejected(t *testing.T, rej []Rejection, file, key string) {
	t.Helper()
	for _, r := range rej {
		if r.File == file && r.Key == key {
			return
		}
	}
	t.Fatalf("rejections %+v do not contain {File:%q Key:%q}", rej, file, key)
}

// TestLoadSession_RepoAllowlistRejection is the dedicated §10.2 test: a repo
// file setting claude_bin, env_passthrough, and an entire [daemon] table
// must have every one of those keys rejected, while the allowed keys still
// apply.
func TestLoadSession_RepoAllowlistRejection(t *testing.T) {
	withHome(t)
	_, sub := setupRepo(t)

	repoPath := filepath.Join(sub, ".corral.toml")
	writeFile(t, repoPath, ""+
		"[session]\n"+
		"model = \"ok-model\"\n"+
		"claude_bin = \"/evil/claude\"\n"+
		"env_passthrough = [\"EVIL_VAR\"]\n"+
		"[daemon]\n"+
		"socket = \"/evil.sock\"\n"+
		"state_dir = \"/evil\"\n"+
		"log_level = \"debug\"\n"+
		"log_format = \"json\"\n"+
		"shutdown_grace = \"1s\"\n"+
		"listen = \"0.0.0.0:1337\"\n"+
		"tls_cert = \"/evil/cert.pem\"\n"+
		"tls_key = \"/evil/key.pem\"\n"+
		"[attach]\n"+
		"prefix_key = \"x\"\n"+
		"[client]\n"+
		"host = \"evil.example.com:443\"\n"+
		"token = \"crl_evil\"\n"+
		"cacert = \"/evil/ca.pem\"\n")

	sess, _, _, rej, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Model != "ok-model" {
		t.Errorf("Model = %q, want %q", sess.Model, "ok-model")
	}
	if sess.ClaudeBin != "claude" {
		t.Errorf("ClaudeBin = %q, want default %q (repo override must be rejected)", sess.ClaudeBin, "claude")
	}
	if len(sess.EnvPassthrough) != 0 {
		t.Errorf("EnvPassthrough = %v, want empty (repo override must be rejected)", sess.EnvPassthrough)
	}

	wantRejected := []string{
		"session.claude_bin",
		"session.env_passthrough",
		"daemon.socket",
		"daemon.state_dir",
		"daemon.log_level",
		"daemon.log_format",
		"daemon.shutdown_grace",
		"daemon.listen",
		"daemon.tls_cert",
		"daemon.tls_key",
		"attach.prefix_key",
		"client.host",
		"client.token",
		"client.cacert",
	}
	for _, key := range wantRejected {
		assertRejected(t, rej, repoPath, key)
	}
	if len(rej) != len(wantRejected) {
		t.Errorf("rejections = %+v, want exactly %v", rej, wantRejected)
	}
}

func TestLoadSession_NoRepoFileFound(t *testing.T) {
	withHome(t)
	// A cwd with no .git and no .corral.toml anywhere above it up to the
	// filesystem root must resolve with zero rejections and no error.
	dir := t.TempDir()
	sess, _, _, rej, err := LoadSession(dir, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(rej) != 0 {
		t.Fatalf("rejections = %v, want none", rej)
	}
	if sess.ClaudeBin != "claude" {
		t.Fatalf("ClaudeBin = %q, want default", sess.ClaudeBin)
	}
}

func TestLoadSession_RepoSearchStopsAtGitRootInclusive(t *testing.T) {
	withHome(t)
	root, sub := setupRepo(t)

	// A .corral.toml ABOVE the git root must never be consulted.
	aboveRoot := filepath.Dir(root)
	writeFile(t, filepath.Join(aboveRoot, ".corral.toml"), "[session]\nterm = \"above-root-term\"\n")
	defer os.Remove(filepath.Join(aboveRoot, ".corral.toml"))

	// A .corral.toml AT the git root itself (inclusive) must be consulted.
	writeFile(t, filepath.Join(root, ".corral.toml"), "[session]\nterm = \"git-root-term\"\n")

	sess, _, sources, _, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Term != "git-root-term" {
		t.Errorf("Term = %q, want %q (git-root file, found walking up from sub)", sess.Term, "git-root-term")
	}
	if sources["session.term"] != filepath.Join(root, ".corral.toml") {
		t.Errorf("Sources[session.term] = %q, want the git-root file", sources["session.term"])
	}
}

// TestLoadClient mirrors TestLoadDaemon_Precedence's structure for the
// [client] pipeline (design doc m5.md §7): defaults (all empty, meaning
// "local unix socket") -> user file -> env, plus the home-expansion rule
// that applies to client.cacert (a file path) but not client.host (a
// network address) or client.token (a secret, not a path at all).
func TestLoadClient(t *testing.T) {
	home := withHome(t)
	userPath := filepath.Join(home, ".corral", "config.toml")

	// Defaults only: everything empty, never falling back to some implicit
	// remote target.
	cl, sources, err := LoadClient()
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cl.Host != "" || cl.Token != "" || cl.CACert != "" {
		t.Fatalf("defaults-only Client = %+v, want all empty", cl)
	}
	if sources["client.host"] != "default" {
		t.Fatalf(`Sources["client.host"] = %q, want "default"`, sources["client.host"])
	}

	// User file overrides defaults; client.cacert's leading "~" must expand
	// to the fake HOME, exactly like daemon.tls_cert does in resolveDaemon.
	writeFile(t, userPath, ""+
		"[client]\n"+
		"host = \"corral.example.com:8443\"\n"+
		"token = \"crl_user_token\"\n"+
		"cacert = \"~/certs/ca.pem\"\n")
	cl, sources, err = LoadClient()
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cl.Host != "corral.example.com:8443" {
		t.Fatalf("Host = %q, want %q (user layer)", cl.Host, "corral.example.com:8443")
	}
	if cl.Token != "crl_user_token" {
		t.Fatalf("Token = %q, want %q (user layer)", cl.Token, "crl_user_token")
	}
	wantCACert := filepath.Join(home, "certs", "ca.pem")
	if cl.CACert != wantCACert {
		t.Fatalf("CACert = %q, want %q (~ expanded to fake HOME)", cl.CACert, wantCACert)
	}
	if sources["client.host"] != userPath {
		t.Fatalf("Sources[client.host] = %q, want %q", sources["client.host"], userPath)
	}

	// Env overrides user file.
	t.Setenv("CORRAL_CLIENT_HOST", "env.example.com:9443")
	t.Setenv("CORRAL_CLIENT_TOKEN", "crl_env_token")
	t.Setenv("CORRAL_CLIENT_CACERT", "~/env-certs/ca.pem")
	cl, sources, err = LoadClient()
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cl.Host != "env.example.com:9443" {
		t.Fatalf("Host = %q, want %q (env layer)", cl.Host, "env.example.com:9443")
	}
	if cl.Token != "crl_env_token" {
		t.Fatalf("Token = %q, want %q (env layer)", cl.Token, "crl_env_token")
	}
	wantEnvCACert := filepath.Join(home, "env-certs", "ca.pem")
	if cl.CACert != wantEnvCACert {
		t.Fatalf("CACert = %q, want %q (env layer, ~ expanded)", cl.CACert, wantEnvCACert)
	}
	if sources["client.host"] != "env CORRAL_CLIENT_HOST" {
		t.Fatalf("Sources[client.host] = %q, want %q", sources["client.host"], "env CORRAL_CLIENT_HOST")
	}
}

func TestLoadSession_OutputLogMaxBytesParseError(t *testing.T) {
	home := withHome(t)
	writeFile(t, filepath.Join(home, ".corral", "config.toml"), "[session]\noutput_log_max_bytes = \"not-a-size\"\n")

	dir := t.TempDir()
	_, _, _, _, err := LoadSession(dir, nil)
	if err == nil {
		t.Fatal("LoadSession: want error for invalid byte size, got nil")
	}
	if !strings.Contains(err.Error(), "session.output_log_max_bytes") {
		t.Fatalf("error %q does not name session.output_log_max_bytes", err.Error())
	}
}
