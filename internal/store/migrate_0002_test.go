package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
)

// v1Session is one row written by writeV1Fixture, chosen to exercise every
// desired_state/status combination migration 0002's partial unique index
// (sessions_name_active) and agent_state backfill care about (§6.3).
type v1Session struct {
	id, name, desiredState, status string
}

var v1Fixture = []v1Session{
	{"sess-starting", "alpha", "running", "starting"},
	{"sess-running", "bravo", "running", "running"},
	// Graceful shutdown in flight: desired_state hasn't flipped to
	// 'stopped' yet, so this name must still be reserved post-migration.
	{"sess-graceful", "charlie", "running", "exited"},
	// True terminal: name must become reusable post-migration.
	{"sess-stopped-exited", "delta", "stopped", "exited"},
	{"sess-stopped-failed", "echo", "stopped", "failed"},
}

// writeV1Fixture creates a fresh SQLite file at path containing only
// 0001_init.sql's schema (schema_version 1), populated with v1Fixture's
// sessions rows plus one events row per session — the events rows are what
// makes this fixture useful for the ON DELETE CASCADE regression guard
// (§6.2, §6.3): if migration 0002 (or store.Open's migration handle) ever
// regresses to running with foreign_keys ON, these rows disappear silently.
func writeV1Fixture(t *testing.T, path string) {
	t.Helper()

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("writeV1Fixture: open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	sqlText, err := os.ReadFile(filepath.Join("migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("writeV1Fixture: reading 0001_init.sql: %v", err)
	}
	if _, err := db.Exec(string(sqlText)); err != nil {
		t.Fatalf("writeV1Fixture: applying 0001_init.sql: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("writeV1Fixture: setting user_version: %v", err)
	}

	now := int64(1700000000000)
	for i, s := range v1Fixture {
		_, err := db.Exec(`
			INSERT INTO sessions (
				id, name, mode, cwd, claude_bin, argv_json, env_keys_json,
				settings_path, setting_sources, desired_state, status,
				rows, cols, resume_count, created_at_ms, updated_at_ms
			) VALUES (?, ?, 'interactive', '/tmp', '/usr/bin/claude', '[]', '[]',
				'/tmp/settings.json', 'user,project', ?, ?, 24, 80, 0, ?, ?)
		`, s.id, s.name, s.desiredState, s.status, now+int64(i), now+int64(i))
		if err != nil {
			t.Fatalf("writeV1Fixture: inserting session %s: %v", s.name, err)
		}
		_, err = db.Exec(`
			INSERT INTO events (session_id, ts_ms, kind, data_json)
			VALUES (?, ?, 'session_created', '{}')
		`, s.id, now+int64(i))
		if err != nil {
			t.Fatalf("writeV1Fixture: inserting event for %s: %v", s.name, err)
		}
	}

	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("writeV1Fixture: wal_checkpoint: %v", err)
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("counting rows in %s: %v", table, err)
	}
	return n
}

func TestMigration0002_PreservesDataAndEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	writeV1Fixture(t, path)

	clk := clocktest.NewFake(time.Unix(1700000000, 0))
	st, err := Open(path, clk)
	if err != nil {
		t.Fatalf("Open (running migration 0002): %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	v, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 2 {
		t.Fatalf("SchemaVersion = %d, want 2", v)
	}

	if n := countRows(t, st.db, "sessions"); n != len(v1Fixture) {
		t.Fatalf("sessions row count = %d, want %d (data must survive the rebuild)", n, len(v1Fixture))
	}
	// The cascade-regression guard: if migration ran with foreign_keys ON,
	// dropping the renamed-away old sessions table would have cascaded
	// deletes into events for every row.
	if n := countRows(t, st.db, "events"); n != len(v1Fixture) {
		t.Fatalf("events row count = %d, want %d (ON DELETE CASCADE must not have fired during migration)", n, len(v1Fixture))
	}

	var eventsSchema string
	if err := st.db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='events'").Scan(&eventsSchema); err != nil {
		t.Fatalf("reading events schema: %v", err)
	}
	if want := "REFERENCES sessions(id) ON DELETE CASCADE"; !strings.Contains(eventsSchema, want) {
		t.Fatalf("events schema = %q, want it to still contain %q", eventsSchema, want)
	}

	wantAgentState := map[string]string{
		"alpha":   "starting",
		"bravo":   "running",
		"charlie": "exited",
		"delta":   "exited",
		"echo":    "failed",
	}
	for name, want := range wantAgentState {
		sess, err := st.GetSessionByName(ctx, name)
		if err != nil {
			t.Fatalf("GetSessionByName(%s): %v", name, err)
		}
		if string(sess.AgentState) != want {
			t.Errorf("session %s: AgentState = %s, want %s (backfilled from Status)", name, sess.AgentState, want)
		}
		if sess.HookCount != 0 {
			t.Errorf("session %s: HookCount = %d, want 0", name, sess.HookCount)
		}
		if sess.AgentStateSinceMs != nil {
			t.Errorf("session %s: AgentStateSinceMs = %v, want nil", name, sess.AgentStateSinceMs)
		}
		if sess.PermissionMode != "" {
			t.Errorf("session %s: PermissionMode = %q, want \"\"", name, sess.PermissionMode)
		}
	}
}

func TestMigration0002_PartialUniqueIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	writeV1Fixture(t, path)

	clk := clocktest.NewFake(time.Unix(1700000000, 0))
	st, err := Open(path, clk)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// "bravo" is active (running/running): a second active session can't
	// take its name.
	_, err = st.CreateSession(ctx, sampleCreateParams("new-bravo", "bravo"))
	if err == nil {
		t.Fatal("CreateSession with active name \"bravo\": want error, got nil")
	}

	// "charlie" is a graceful shutdown in flight (running/exited): still
	// reserved.
	_, err = st.CreateSession(ctx, sampleCreateParams("new-charlie", "charlie"))
	if err == nil {
		t.Fatal("CreateSession with in-flight-shutdown name \"charlie\": want error, got nil")
	}

	// "delta" is truly terminal (stopped/exited): name is free to reuse.
	if _, err := st.CreateSession(ctx, sampleCreateParams("new-delta", "delta")); err != nil {
		t.Fatalf("CreateSession with terminal name \"delta\": want success, got %v", err)
	}

	// "echo" is truly terminal (stopped/failed): also free to reuse.
	if _, err := st.CreateSession(ctx, sampleCreateParams("new-echo", "echo")); err != nil {
		t.Fatalf("CreateSession with terminal name \"echo\": want success, got %v", err)
	}
}
