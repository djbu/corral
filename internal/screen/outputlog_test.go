package screen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutputLogModeAndContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")

	l, err := OpenOutputLog(path, 1024, discardLogger())
	if err != nil {
		t.Fatalf("OpenOutputLog: %v", err)
	}
	defer l.Close()

	if _, err := l.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := l.Write([]byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("contents = %q, want %q", got, "hello world")
	}
}

func TestOutputLogStopsAtCapNoRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")

	l, err := OpenOutputLog(path, 10, discardLogger())
	if err != nil {
		t.Fatalf("OpenOutputLog: %v", err)
	}
	defer l.Close()

	// First write exactly reaches the cap.
	n, err := l.Write([]byte("0123456789"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 10 {
		t.Fatalf("n = %d, want 10", n)
	}

	// Further writes must be silently accepted (no error) but dropped —
	// diagnostics only, never propagated into Feed's path (§7.4: "stop
	// writing and log one warning at the cap, no rotation").
	n, err = l.Write([]byte("overflow"))
	if err != nil {
		t.Fatalf("Write after cap returned error: %v", err)
	}
	if n != len("overflow") {
		t.Fatalf("n = %d, want %d (caller-facing n must still equal len(p))", n, len("overflow"))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "0123456789" {
		t.Errorf("contents = %q, want exactly the first 10 bytes with nothing appended after the cap", got)
	}
}

func TestOutputLogPartialWriteAtCapBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")

	l, err := OpenOutputLog(path, 5, discardLogger())
	if err != nil {
		t.Fatalf("OpenOutputLog: %v", err)
	}
	defer l.Close()

	// A single write straddling the cap: only the first 5 bytes should
	// land on disk.
	if _, err := l.Write([]byte("abcdefgh")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "abcde" {
		t.Errorf("contents = %q, want %q", got, "abcde")
	}
}

func TestOutputLogTightensPreexistingLooseMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")

	// Pre-create at a loose mode, as a repo checkout or umask oddity might.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l, err := OpenOutputLog(path, 1024, discardLogger())
	if err != nil {
		t.Fatalf("OpenOutputLog: %v", err)
	}
	defer l.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 (pre-existing 0644 must be tightened)", info.Mode().Perm())
	}
}
