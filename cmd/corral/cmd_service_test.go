package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djbu/corral/internal/service"
)

type unusedServiceRunner struct{}

func (unusedServiceRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

func TestServiceUnknownCommandReturnsUsageBeforeTouchingHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdService([]string{"not-a-command"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code=%d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "usage: corral service") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestExecutableDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corral")
	if err := os.WriteFile(path, []byte("abc"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := executableDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("digest=%q, want %q", got, want)
	}
}

func TestInstalledServiceDetectionIsIsolatedAndSkipsEnvDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"CORRAL_DAEMON_STATE_DIR", "CORRAL_DAEMON_SOCKET"} {
		value, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	mgr, installed, err := installedServiceManager()
	if err != nil || installed || mgr == nil {
		t.Fatalf("absent service mgr=%v installed=%t err=%v", mgr, installed, err)
	}

	t.Setenv("CORRAL_DAEMON_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	mgr, installed, err = installedServiceManager()
	if err != nil || installed || mgr != nil {
		t.Fatalf("env-overridden daemon mgr=%v installed=%t err=%v", mgr, installed, err)
	}
}

func TestServiceInstallRejectsEphemeralDaemonLocation(t *testing.T) {
	home := t.TempDir()
	mgr, err := service.New(service.Config{
		Platform: service.PlatformDarwin, UID: 501, HomeDir: home,
		StateDir: filepath.Join(home, ".corral"), BinaryPath: "/opt/corral",
		BinaryDigest: strings.Repeat("a", 64),
	}, unusedServiceRunner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORRAL_DAEMON_STATE_DIR", filepath.Join(home, "ephemeral"))
	var stdout, stderr bytes.Buffer
	if code := serviceInstall(mgr, nil, &stdout, &stderr); code != exitError {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must be persisted") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
