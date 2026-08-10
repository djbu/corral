package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/store"
)

func TestBackupAndRestoreCLIWithRollback(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORRAL_DAEMON_STATE_DIR", stateDir)
	t.Setenv("CORRAL_DAEMON_SOCKET", filepath.Join(stateDir, "corral.sock"))
	dbPath := filepath.Join(stateDir, "corral.db")
	st, err := store.Open(dbPath, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	db := openCLIDataTestDB(t, dbPath)
	if _, err := db.Exec("INSERT INTO meta(key,value) VALUES ('sentinel','before')"); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(stateDir, "manual", "snapshot.db")
	var stdout, stderr bytes.Buffer
	if code := cmdBackup([]string{"--output", backup}, &stdout, &stderr); code != exitOK {
		t.Fatalf("backup code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "integrity=ok") {
		t.Fatalf("backup output = %q", stdout.String())
	}
	if _, err := db.Exec("UPDATE meta SET value='after' WHERE key='sentinel'"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdRestore([]string{"--replace", backup}, &stdout, &stderr); code != exitOK {
		t.Fatalf("restore code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "rollback=") {
		t.Fatalf("restore did not report rollback: %q", stdout.String())
	}
	restored := openCLIDataTestDB(t, dbPath)
	defer restored.Close()
	var value string
	if err := restored.QueryRow("SELECT value FROM meta WHERE key='sentinel'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "before" {
		t.Fatalf("restored sentinel = %q, want before", value)
	}
}

func TestGCCLIDryRunThenApplyPreservesReferencedSession(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORRAL_DAEMON_STATE_DIR", stateDir)
	t.Setenv("CORRAL_DAEMON_SOCKET", filepath.Join(stateDir, "corral.sock"))
	dbPath := filepath.Join(stateDir, "corral.db")
	st, err := store.Open(dbPath, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	db := openCLIDataTestDB(t, dbPath)
	insertMinimalSession(t, db, "referenced")
	db.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(stateDir, "sessions")
	for _, id := range []string{"referenced", "orphan"} {
		if err := os.MkdirAll(filepath.Join(root, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, id, "output.log"), []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "orphan"), old, old); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"--dry-run", "--older-than", "24h", "--max-bytes", "1GiB"}
	if code := cmdGC(args, &stdout, &stderr); code != exitOK {
		t.Fatalf("gc dry code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "orphan")); err != nil {
		t.Fatalf("dry-run removed orphan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "daemon.lock")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created maintenance lock: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	args[0] = "--apply"
	if code := cmdGC(args, &stdout, &stderr); code != exitOK {
		t.Fatalf("gc apply code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "orphan")); !os.IsNotExist(err) {
		t.Fatalf("orphan remains after apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "referenced")); err != nil {
		t.Fatalf("referenced session removed: %v", err)
	}
}

func openCLIDataTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func insertMinimalSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO sessions (id,name,mode,cwd,claude_bin,argv_json,env_keys_json,settings_path,setting_sources,desired_state,status,rows,cols,created_at_ms,updated_at_ms,last_activity_ms,agent_state,hook_count)
		VALUES (?,?, 'interactive','/repo','/bin/true','[]','[]','/settings','user','stopped','exited',24,80,1,1,1,'exited',0)
	`, id, id)
	if err != nil {
		t.Fatal(err)
	}
}
