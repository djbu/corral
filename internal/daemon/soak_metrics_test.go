package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendSoakSample(t *testing.T) {
	dir := t.TempDir()
	wal := filepath.Join(dir, "corral.db-wal")
	if err := os.WriteFile(wal, make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "soak-daemon.jsonl")
	if err := appendSoakSample(path, wal); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got soakSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode sample %q: %v", raw, err)
	}
	if got.Goroutines < 1 || got.WALBytes != 17 || got.At.IsZero() {
		t.Fatalf("sample = %+v, want timestamp/goroutines/WAL", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("metrics permissions = %o, want 600", info.Mode().Perm())
	}
}
