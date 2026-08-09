package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// TestCmdAttach_RemoteHostRejected is M5 step 31's local-only guard (design
// doc §7 rule 3): attach's PTY hijack has no remote transport, so when a
// remote target resolves — here via CORRAL_CLIENT_HOST, exactly as an
// operator's shell environment would set it — cmdAttach must fail loudly
// with a clear diagnosis instead of silently dialing the local socket (or
// worse, trying to dial the remote host and failing with a confusing
// low-level error).
//
// 192.0.2.1 is a TEST-NET-1 address (RFC 5737): guaranteed non-routable, so
// even if the local-only guard were somehow bypassed this test could never
// actually reach a network host.
func TestCmdAttach_RemoteHostRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CORRAL_CLIENT_HOST", "192.0.2.1:8443")

	var stdout, stderr bytes.Buffer
	code := cmdAttach([]string{"whatever"}, &stdout, &stderr)

	if code != exitError {
		t.Fatalf("code = %d, want exitError (%d); stderr = %q", code, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "attach requires a local daemon") {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), "attach requires a local daemon")
	}
}

// TestResolveTarget_FlagBeatsEnv pins resolveTarget's precedence (mirrored
// from config.LoadClient's own env-beats-user-file precedence, one layer up
// the stack): an explicit --host flag on the command line wins over
// CORRAL_CLIENT_HOST, exactly like the doc comment on resolveTarget
// promises.
func TestResolveTarget_FlagBeatsEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CORRAL_CLIENT_HOST", "env-host.example.com:8443")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := addClientFlags(fs)
	if err := fs.Parse([]string{"--host", "flag-host.example.com:9443"}); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	host, _, _, err := resolveTarget(cf)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if host != "flag-host.example.com:9443" {
		t.Fatalf("host = %q, want the --host flag's value, not the env var's", host)
	}
}

// TestResolveTarget_EnvWhenNoFlag confirms the other half of that
// precedence: with no --host flag given at all, CORRAL_CLIENT_HOST (routed
// through config.LoadClient) is what resolveTarget falls back to.
func TestResolveTarget_EnvWhenNoFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CORRAL_CLIENT_HOST", "env-host.example.com:8443")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := addClientFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	host, _, _, err := resolveTarget(cf)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if host != "env-host.example.com:8443" {
		t.Fatalf("host = %q, want the env var's value", host)
	}
}

// TestResolveTarget_DefaultsToLocal confirms design doc §7 rule 3's other
// direction: with neither --host nor CORRAL_CLIENT_HOST/client.host set,
// resolveTarget must resolve "" (local unix socket) — never some implicit
// remote default.
func TestResolveTarget_DefaultsToLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := addClientFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}

	host, _, _, err := resolveTarget(cf)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if host != "" {
		t.Fatalf("host = %q, want \"\" (local unix socket)", host)
	}
}
