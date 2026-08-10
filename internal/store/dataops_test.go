package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
)

func TestBackupRestorePreservesOperationalData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "corral.db")
	st, err := Open(source, clocktest.NewFake(time.Unix(1, 0)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, `
		INSERT INTO sessions (id,name,mode,cwd,claude_bin,argv_json,env_keys_json,settings_path,setting_sources,desired_state,status,rows,cols,created_at_ms,updated_at_ms,last_activity_ms,agent_state,hook_count)
		VALUES ('s1','one','interactive','/repo','/bin/true','[]','[]','/settings','user','stopped','exited',24,80,1,1,1,'exited',0);
		INSERT INTO events (session_id,ts_ms,kind,data_json) VALUES ('s1',1,'permission.requested','{}');
		INSERT INTO tasks (id,dag_id,name,prompt,repo,cwd,status,session_id,created_ms,updated_ms)
		VALUES ('t1','d1','task','prompt','/repo','/repo','succeeded','s1',1,1);
		INSERT INTO dag_budgets (dag_id,budget_usd,cost_usd) VALUES ('d1',10,2);
		INSERT INTO api_tokens (id,token_hash,label,scope,created_ms,revoked_ms)
		VALUES ('tok1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','phone','admin',1,2);
		INSERT INTO learnings (id,repo,kind,fingerprint,status,content_json,evidence_count,baseline_json,created_ms,updated_ms,expires_ms)
		VALUES ('l1','/repo','permission_rule','fp','adopted','{}',1,'{}',1,1,999);
		INSERT INTO learning_evidence (learning_id,event_seq,role) VALUES ('l1',1,'support');
	`)
	if err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(dir, "backups", "snapshot.db")
	info, err := BackupDatabase(ctx, source, backup)
	if err != nil {
		t.Fatalf("BackupDatabase: %v", err)
	}
	if info.SchemaVersion != 7 || !info.Integrity {
		t.Fatalf("backup info = %+v", info)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	restored := filepath.Join(dir, "restored.db")
	if _, err := RestoreDatabase(ctx, backup, restored); err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	db, err := openReadOnly(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{
		"sessions": 1, "tasks": 1, "dag_budgets": 1, "api_tokens": 1,
		"learnings": 1, "learning_evidence": 1, "events": 1,
	} {
		var got int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("count %s = %d, want %d", table, got, want)
		}
	}
}

func TestBackupNoClobberAndRestoreRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "corral.db")
	st, err := Open(source, clocktest.NewFake(time.Unix(1, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dest := filepath.Join(dir, "exists.db")
	if err := os.WriteFile(dest, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupDatabase(ctx, source, dest); err == nil {
		t.Fatal("backup unexpectedly clobbered destination")
	}
	if got, _ := os.ReadFile(dest); string(got) != "keep" {
		t.Fatalf("destination changed: %q", got)
	}

	newer := filepath.Join(dir, "newer.db")
	if _, err := st.db.ExecContext(ctx, "VACUUM INTO ?", newer); err != nil {
		t.Fatal(err)
	}
	db, err := sqlOpenTest(newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = RestoreDatabase(ctx, newer, filepath.Join(dir, "target.db"))
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("restore error = %v, want ErrSchemaTooNew", err)
	}
}

func TestMaintainCheckpointsAndVacuumsOnlyAboveThreshold(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "corral.db")
	st, err := Open(path, clocktest.NewFake(time.Unix(1, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.db.ExecContext(ctx, "CREATE TABLE churn(value BLOB)"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, "INSERT INTO churn(value) VALUES (zeroblob(2097152))"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, "DELETE FROM churn"); err != nil {
		t.Fatal(err)
	}
	preview, err := st.Maintain(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ReclaimableBytes == 0 || preview.Vacuumed {
		t.Fatalf("maintenance preview = %+v", preview)
	}
	skipped, err := st.Maintain(ctx, true, preview.ReclaimableBytes+1)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Vacuumed {
		t.Fatal("VACUUM ran below threshold")
	}
	run, err := st.Maintain(ctx, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Vacuumed {
		t.Fatal("VACUUM did not run at threshold")
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL size after maintenance = %d", info.Size())
	}
}

func sqlOpenTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path)
}
