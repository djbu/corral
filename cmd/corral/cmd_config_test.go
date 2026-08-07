package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCmdConfig_PrintsResolvedKeysWithSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "session.model = ") {
		t.Fatalf("stdout missing session.model line: %q", out)
	}
	if !strings.Contains(out, "# from default") {
		t.Fatalf("stdout missing source annotation: %q", out)
	}
}

func TestCmdConfig_CwdFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "--cwd", dir}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "daemon.socket = ") {
		t.Fatalf("stdout missing daemon.socket line: %q", stdout.String())
	}
}
