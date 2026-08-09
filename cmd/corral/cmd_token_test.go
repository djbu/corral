package main

import (
	"bytes"
	"testing"
	"time"
)

// TestCmdToken_NoSubverb covers `corral token` with no sub-verb: usage to
// stderr, exitUsage.
func TestCmdToken_NoSubverb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdToken(nil, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("code = %d, want exitUsage(%d)", code, exitUsage)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want a usage message")
	}
}

// TestCmdToken_UnknownSubverb covers an unrecognized sub-verb.
func TestCmdToken_UnknownSubverb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdToken([]string{"frobnicate"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("code = %d, want exitUsage(%d)", code, exitUsage)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want a usage message")
	}
}

// TestCmdToken_CreateMissingLabel covers `corral token create` with no
// --label: exitUsage, no daemon dial attempted (this path returns before
// config.LoadDaemon is ever called).
func TestCmdToken_CreateMissingLabel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdToken([]string{"create"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("code = %d, want exitUsage(%d)", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestCmdToken_RevokeMissingID covers `corral token revoke` with no id
// argument.
func TestCmdToken_RevokeMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdToken([]string{"revoke"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("code = %d, want exitUsage(%d)", code, exitUsage)
	}
}

// TestFormatTokenMs pins the ms->display formatting helper: 0 (NULL in the
// store) renders as "-"; a known ms value renders as RFC3339.
func TestFormatTokenMs(t *testing.T) {
	if got := formatTokenMs(0); got != "-" {
		t.Fatalf("formatTokenMs(0) = %q, want %q", got, "-")
	}

	// formatTokenMs renders in the local zone (time.UnixMilli's convention,
	// matching cmd_run.go's waitForDAG precedent), so the assertion below
	// round-trips through time.Parse rather than comparing against a
	// hardcoded zone offset that would be test-machine-dependent.
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ms := ts.UnixMilli()
	got := formatTokenMs(ms)
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("formatTokenMs(%d) = %q, not valid RFC3339: %v", ms, got, err)
	}
	if !parsed.Equal(ts) {
		t.Fatalf("formatTokenMs(%d) = %q, parses to %v, want %v", ms, got, parsed, ts)
	}
}

// TestEmptyColumn pins the "" -> "-" helper used for the SESSION column.
func TestEmptyColumn(t *testing.T) {
	if got := emptyColumn(""); got != "-" {
		t.Fatalf("emptyColumn(\"\") = %q, want %q", got, "-")
	}
	if got := emptyColumn("sess-1"); got != "sess-1" {
		t.Fatalf("emptyColumn(%q) = %q, want unchanged", "sess-1", got)
	}
}
