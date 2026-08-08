package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
)

// TestWriteDBFixtures regenerates testdata/db/v1.db and testdata/db/v2.db.
// It is gated behind CORRAL_WRITE_FIXTURES=1 and skipped otherwise: these
// two files are checked-in artifacts, not something every `go test` run
// should touch. Run it once and commit the results whenever v1Fixture (in
// migrate_0002_test.go) changes, or when a future milestone needs a new
// "vN.db" fixture for its own migration tests (§6.3 step 7 — v2.db exists
// for M3 to migrate from).
//
//	CORRAL_WRITE_FIXTURES=1 go test ./internal/store -run TestWriteDBFixtures -v
func TestWriteDBFixtures(t *testing.T) {
	if os.Getenv("CORRAL_WRITE_FIXTURES") != "1" {
		t.Skip("set CORRAL_WRITE_FIXTURES=1 to regenerate testdata/db/{v1,v2}.db")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	outDir := filepath.Join(repoRoot, "testdata", "db")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}

	v1Path := filepath.Join(outDir, "v1.db")
	for _, p := range []string{v1Path, v1Path + "-wal", v1Path + "-shm"} {
		os.Remove(p)
	}
	writeV1Fixture(t, v1Path)
	if err := os.Chmod(v1Path, 0o644); err != nil {
		t.Fatalf("chmod v1.db: %v", err)
	}

	// v2.db: v1's data migrated to schema 2, plus a little M2-shaped data
	// (a hook_count bump and a non-nil BlockedReasonJSON) so a future
	// milestone's migration test has something to assert survived, not
	// just zero-valued new columns.
	v2Path := filepath.Join(outDir, "v2.db")
	for _, p := range []string{v2Path, v2Path + "-wal", v2Path + "-shm"} {
		os.Remove(p)
	}
	writeV1Fixture(t, v2Path)

	clk := clocktest.NewFake(time.Unix(1700000100, 0))
	st, err := Open(v2Path, clk)
	if err != nil {
		t.Fatalf("Open v2.db (applying migration 0002): %v", err)
	}
	ctx := context.Background()
	sinceMs := int64(1700000050000)
	if _, err := st.UpdateSession(ctx, "sess-running", func(s *session.Session) {
		s.AgentState = session.AgentWorking
		s.AgentStateSinceMs = &sinceMs
		s.HookCount = 3
		s.LastHookAtMs = &sinceMs
		s.PermissionMode = "acceptEdits"
	}); err != nil {
		t.Fatalf("seeding v2.db M2 data: %v", err)
	}
	if _, err := st.UpdateSession(ctx, "sess-graceful", func(s *session.Session) {
		s.AgentState = session.AgentBlocked
		s.BlockedReasonJSON = `{"kind":"permission","tool_name":"Bash"}`
	}); err != nil {
		t.Fatalf("seeding v2.db M2 data: %v", err)
	}
	if err := st.WalCheckpointTruncate(ctx); err != nil {
		t.Fatalf("wal_checkpoint(TRUNCATE) on v2.db: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing v2.db: %v", err)
	}
	if err := os.Chmod(v2Path, 0o644); err != nil {
		t.Fatalf("chmod v2.db: %v", err)
	}

	t.Logf("wrote %s and %s", v1Path, v2Path)
}
