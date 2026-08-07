package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRun_NoArgsIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)

	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q, want usage message", stderr.String())
	}
}

func TestRun_UnknownCommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)

	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr = %q, want unknown-command message", stderr.String())
	}
}

func TestRun_VersionFlag(t *testing.T) {
	for _, flag := range []string{"--version", "-version"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, &stdout, &stderr)

		if code != exitOK {
			t.Fatalf("%s: code = %d, want %d", flag, code, exitOK)
		}
		if !strings.HasPrefix(stdout.String(), "corral ") {
			t.Fatalf("%s: stdout = %q, want prefix %q", flag, stdout.String(), "corral ")
		}
		if stderr.Len() != 0 {
			t.Fatalf("%s: stderr = %q, want empty", flag, stderr.String())
		}
	}
}

func TestRun_HelpFlag(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, &stdout, &stderr)

		if code != exitOK {
			t.Fatalf("%s: code = %d, want %d", flag, code, exitOK)
		}
		if !strings.Contains(stdout.String(), "usage:") {
			t.Fatalf("%s: stdout = %q, want usage message", flag, stdout.String())
		}
	}
}

func TestRun_DispatchesKnownCommand(t *testing.T) {
	// Register a throwaway command directly in the dispatch table rather
	// than calling "config" (still a stub as of this step) so this test
	// exercises only main's dispatch mechanics.
	const name = "__test_probe__"
	var gotArgs []string
	commands[name] = func(args []string, stdout, stderr io.Writer) int {
		gotArgs = args
		return 42
	}
	defer delete(commands, name)

	var stdout, stderr bytes.Buffer
	code := run([]string{name, "a", "b"}, &stdout, &stderr)

	if code != 42 {
		t.Fatalf("code = %d, want 42", code)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "a" || gotArgs[1] != "b" {
		t.Fatalf("gotArgs = %v, want [a b]", gotArgs)
	}
}
