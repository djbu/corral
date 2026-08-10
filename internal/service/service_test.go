package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls          []string
	launchdLoaded  bool
	launchdActive  bool
	systemdEnabled bool
	systemdActive  bool
}

type unavailableRunner struct{ name string }

func (u unavailableRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, &exec.Error{Name: u.name, Err: exec.ErrNotFound}
}

type unavailableSessionRunner struct{}

func (unavailableSessionRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("Failed to connect to bus: No medium found")
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	switch {
	case name == "launchctl" && len(args) > 0 && args[0] == "print":
		if !f.launchdLoaded {
			return nil, errors.New("not loaded")
		}
		if f.launchdActive {
			return []byte("state = running\n"), nil
		}
		return []byte("state = waiting\n"), nil
	case name == "launchctl" && len(args) > 0 && args[0] == "bootstrap":
		f.launchdLoaded, f.launchdActive = true, true
		return nil, nil
	case name == "launchctl" && len(args) > 0 && args[0] == "bootout":
		f.launchdLoaded, f.launchdActive = false, false
		return nil, nil
	case name == "launchctl" && len(args) > 0 && args[0] == "kickstart":
		f.launchdActive = true
		return nil, nil
	case call == "systemctl --user is-enabled --quiet corral.service":
		if f.systemdEnabled {
			return nil, nil
		}
		return nil, errors.New("disabled")
	case call == "systemctl --user is-active --quiet corral.service":
		if f.systemdActive {
			return nil, nil
		}
		return nil, errors.New("inactive")
	case call == "systemctl --user enable --now corral.service":
		f.systemdEnabled, f.systemdActive = true, true
		return nil, nil
	case call == "systemctl --user restart corral.service":
		f.systemdActive = true
		return nil, nil
	case call == "systemctl --user disable --now corral.service":
		f.systemdEnabled, f.systemdActive = false, false
		return nil, nil
	case call == "systemctl --user daemon-reload":
		return nil, nil
	default:
		return nil, errors.New("unexpected command: " + call)
	}
}

func TestRenderLaunchdDeterministicEscapedAndSupervised(t *testing.T) {
	cfg := Config{
		Platform: PlatformDarwin, UID: 501,
		HomeDir: "/Users/A & B", StateDir: "/Users/A & B/.corral",
		BinaryPath: "/Users/A & B/bin/corral",
	}
	mgr := newTestManager(t, cfg, &fakeRunner{})
	first, err := mgr.Render()
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.Render()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("launchd render is not deterministic")
	}
	text := string(first)
	for _, want := range []string{
		"<string>/Users/A &amp; B/bin/corral</string>",
		"<string>daemon</string>", "<string>--foreground</string>",
		"<key>RunAtLoad</key>", "<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>", "<false/>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q:\n%s", want, text)
		}
	}
	assertNoEmbeddedSecrets(t, text)
	if runtime.GOOS == PlatformDarwin {
		path := filepath.Join(t.TempDir(), "corral.plist")
		if err := os.WriteFile(path, first, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint: %v\n%s", err, out)
		}
	}
}

func TestRenderSystemdDeterministicQuotedAndSupervised(t *testing.T) {
	cfg := Config{
		Platform: PlatformLinux, UID: 1000,
		HomeDir: "/home/a b/$work", StateDir: "/home/a b/$work/.corral",
		BinaryPath: "/home/a b/100%/corral",
	}
	mgr := newTestManager(t, cfg, &fakeRunner{})
	first, err := mgr.Render()
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.Render()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("systemd render is not deterministic")
	}
	text := string(first)
	for _, want := range []string{
		`ExecStart="/home/a b/100%%/corral" daemon --foreground`,
		`WorkingDirectory="/home/a b/$work"`,
		"Restart=on-failure", "WantedBy=default.target", "UMask=0077",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unit missing %q:\n%s", want, text)
		}
	}
	assertNoEmbeddedSecrets(t, text)
}

func TestLaunchdInstallUpgradeUninstallIdempotentAndPreservesData(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".corral")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stateDir, "state.db")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	cfg := Config{Platform: PlatformDarwin, UID: 501, HomeDir: home, StateDir: stateDir, BinaryPath: "/opt/corral-v1"}
	mgr := newTestManager(t, cfg, runner)

	status, changed, err := mgr.Install(context.Background())
	if err != nil || !changed || !status.Installed || !status.Enabled || !status.Active {
		t.Fatalf("first install status=%+v changed=%t err=%v", status, changed, err)
	}
	assertRegularMode(t, mgr.Path(), 0o644)
	mutations := mutationCount(runner.calls)
	if _, changed, err = mgr.Install(context.Background()); err != nil || changed {
		t.Fatalf("second install changed=%t err=%v", changed, err)
	}
	if got := mutationCount(runner.calls); got != mutations {
		t.Fatalf("idempotent install issued mutations: before=%d after=%d calls=%v", mutations, got, runner.calls)
	}
	runner.launchdActive = false
	if status, changed, err = mgr.Install(context.Background()); err != nil || changed || !status.Active {
		t.Fatalf("recover inactive launchd status=%+v changed=%t err=%v", status, changed, err)
	}
	if !containsCall(runner.calls, "launchctl kickstart -k gui/501/com.djbu.corral") {
		t.Fatalf("inactive launchd job was not kicked: %v", runner.calls)
	}

	upgraded := newTestManager(t, Config{Platform: PlatformDarwin, UID: 501, HomeDir: home, StateDir: stateDir, BinaryPath: "/opt/corral-v1", BinaryDigest: strings.Repeat("b", 64)}, runner)
	if _, changed, err = upgraded.Install(context.Background()); err != nil || !changed {
		t.Fatalf("upgrade changed=%t err=%v", changed, err)
	}
	content, err := os.ReadFile(upgraded.Path())
	if err != nil || !strings.Contains(string(content), strings.Repeat("b", 64)) {
		t.Fatalf("upgraded descriptor err=%v content=%q", err, content)
	}
	if changed, err = upgraded.Uninstall(context.Background()); err != nil || !changed {
		t.Fatalf("uninstall changed=%t err=%v", changed, err)
	}
	if _, err := os.Stat(upgraded.Path()); !os.IsNotExist(err) {
		t.Fatalf("descriptor remains after uninstall: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("user data was not preserved: %q err=%v", got, err)
	}
	if changed, err = upgraded.Uninstall(context.Background()); err != nil || changed {
		t.Fatalf("second uninstall changed=%t err=%v", changed, err)
	}
}

func TestSystemdInstallRecoveryUpgradeAndUninstall(t *testing.T) {
	home := t.TempDir()
	runner := &fakeRunner{}
	cfg := Config{Platform: PlatformLinux, UID: 1000, HomeDir: home, StateDir: filepath.Join(home, ".corral"), BinaryPath: "/opt/corral-v1"}
	mgr := newTestManager(t, cfg, runner)

	status, changed, err := mgr.Install(context.Background())
	if err != nil || !changed || !status.Enabled || !status.Active {
		t.Fatalf("install status=%+v changed=%t err=%v", status, changed, err)
	}
	mutations := mutationCount(runner.calls)
	if _, changed, err = mgr.Install(context.Background()); err != nil || changed {
		t.Fatalf("idempotent install changed=%t err=%v", changed, err)
	}
	if got := mutationCount(runner.calls); got != mutations {
		t.Fatalf("idempotent install mutations=%d want=%d calls=%v", got, mutations, runner.calls)
	}

	// A crashed/inactive unit is brought back without rewriting its descriptor.
	runner.systemdActive = false
	status, changed, err = mgr.Install(context.Background())
	if err != nil || changed || !status.Active {
		t.Fatalf("recover inactive status=%+v changed=%t err=%v", status, changed, err)
	}

	upgraded := newTestManager(t, Config{Platform: PlatformLinux, UID: 1000, HomeDir: home, StateDir: cfg.StateDir, BinaryPath: "/opt/corral-v1", BinaryDigest: strings.Repeat("b", 64)}, runner)
	if _, changed, err = upgraded.Install(context.Background()); err != nil || !changed {
		t.Fatalf("upgrade changed=%t err=%v", changed, err)
	}
	if !containsCall(runner.calls, "systemctl --user restart corral.service") {
		t.Fatalf("upgrade did not restart active unit: %v", runner.calls)
	}
	if changed, err = upgraded.Uninstall(context.Background()); err != nil || !changed {
		t.Fatalf("uninstall changed=%t err=%v", changed, err)
	}
	if runner.systemdEnabled || runner.systemdActive {
		t.Fatal("systemd unit remains enabled or active")
	}
}

func TestNewRejectsUnsafeConfigurations(t *testing.T) {
	base := Config{Platform: PlatformLinux, UID: 1000, HomeDir: "/home/me", StateDir: "/home/me/.corral", BinaryPath: "/home/me/bin/corral"}
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"root", func(c *Config) { c.UID = 0 }},
		{"unsupported", func(c *Config) { c.Platform = "windows" }},
		{"relative home", func(c *Config) { c.HomeDir = "home/me" }},
		{"relative state", func(c *Config) { c.StateDir = ".corral" }},
		{"relative binary", func(c *Config) { c.BinaryPath = "corral" }},
		{"unclean path", func(c *Config) { c.HomeDir = "/home/me/../other" }},
		{"newline", func(c *Config) { c.BinaryPath = "/bin/corral\nExecStart=/tmp/x" }},
		{"bad digest", func(c *Config) { c.BinaryDigest = "not-sha256" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if _, err := New(cfg, &fakeRunner{}); err == nil {
				t.Fatal("New succeeded; want rejection")
			}
		})
	}
}

func TestInstallRestrictsExistingStateDirectory(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".corral")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := newTestManager(t, Config{Platform: PlatformLinux, UID: 1000, HomeDir: home, StateDir: stateDir, BinaryPath: "/opt/corral"}, &fakeRunner{})
	if _, _, err := mgr.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("state dir mode=%o, want 700", got)
	}
}

func TestInstallRejectsSymlinkDescriptor(t *testing.T) {
	home := t.TempDir()
	mgr := newTestManager(t, Config{Platform: PlatformDarwin, UID: 501, HomeDir: home, StateDir: filepath.Join(home, ".corral"), BinaryPath: "/opt/corral"}, &fakeRunner{})
	if err := os.MkdirAll(filepath.Dir(mgr.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "do-not-touch")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, mgr.Path()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("Install err=%v, want non-regular rejection", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "safe" {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
}

func TestStatusReportsMissingUserManager(t *testing.T) {
	for _, platform := range []string{PlatformDarwin, PlatformLinux} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			mgr := newTestManager(t, Config{Platform: platform, UID: 501, HomeDir: home, StateDir: filepath.Join(home, ".corral"), BinaryPath: "/opt/corral"}, unavailableRunner{name: platform})
			if _, err := mgr.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("Status err=%v, want manager unavailable", err)
			}
		})
	}
}

func TestStatusReportsUnavailableUserManagerSession(t *testing.T) {
	home := t.TempDir()
	mgr := newTestManager(t, Config{Platform: PlatformLinux, UID: 1000, HomeDir: home, StateDir: filepath.Join(home, ".corral"), BinaryPath: "/opt/corral"}, unavailableSessionRunner{})
	if _, err := mgr.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "user manager is unavailable") {
		t.Fatalf("Status err=%v, want unavailable user manager", err)
	}
}

func newTestManager(t *testing.T, cfg Config, runner Runner) *Manager {
	t.Helper()
	if cfg.BinaryDigest == "" {
		cfg.BinaryDigest = strings.Repeat("a", 64)
	}
	mgr, err := New(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func assertRegularMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != want {
		t.Fatalf("mode=%v, want regular %v", info.Mode(), want)
	}
}

func assertNoEmbeddedSecrets(t *testing.T, descriptor string) {
	t.Helper()
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "Environment=", "EnvironmentVariables"} {
		if strings.Contains(descriptor, forbidden) {
			t.Fatalf("descriptor embeds secret-bearing field %q", forbidden)
		}
	}
}

func mutationCount(calls []string) int {
	n := 0
	for _, call := range calls {
		if !strings.Contains(call, " print ") && !strings.Contains(call, " is-enabled ") && !strings.Contains(call, " is-active ") {
			n++
		}
	}
	return n
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}
