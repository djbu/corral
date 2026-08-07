package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/session"
)

func openTestStore(t *testing.T) (*Store, *clocktest.FakeClock) {
	t.Helper()
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := Open(filepath.Join(dir, "corral.db"), fc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, fc
}

func sampleCreateParams(id, name string) CreateSessionParams {
	return CreateSessionParams{
		ID:             id,
		Name:           name,
		Mode:           session.ModeInteractive,
		Cwd:            "/tmp/work",
		ClaudeBin:      "/usr/local/bin/claude",
		Model:          "",
		Argv:           []string{"/usr/local/bin/claude", "--session-id", id},
		EnvKeys:        []string{"HOME", "PATH", "ANTHROPIC_API_KEY"},
		SettingsPath:   "/tmp/state/sessions/" + id + "/settings.json",
		SettingSources: "user,project,local",
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusStarting,
		PID:            1234,
		PGID:           1234,
		ProcStartNs:    987654321,
		Rows:           40,
		Cols:           120,
	}
}

func TestStore_CreateAndGetSession(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	got, err := st.CreateSession(ctx, sampleCreateParams("id-1", "work-1"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if got.ID != "id-1" || got.Name != "work-1" {
		t.Fatalf("unexpected created session: %+v", got)
	}
	if got.Model != "" {
		t.Fatalf("Model = %q, want empty (NULL round-trip)", got.Model)
	}
	if got.ClaudeSessionID != "" {
		t.Fatalf("ClaudeSessionID = %q, want empty (NULL round-trip)", got.ClaudeSessionID)
	}
	if got.ExitCode != nil {
		t.Fatalf("ExitCode = %v, want nil (NULL round-trip)", got.ExitCode)
	}
	if got.StartedAtMs != nil || got.LastAttachedAtMs != nil || got.EndedAtMs != nil {
		t.Fatalf("expected all nullable *_at_ms nil on create, got started=%v last=%v ended=%v",
			got.StartedAtMs, got.LastAttachedAtMs, got.EndedAtMs)
	}
	wantMs := fc.Now().UnixMilli()
	if got.CreatedAtMs != wantMs || got.UpdatedAtMs != wantMs {
		t.Fatalf("CreatedAtMs/UpdatedAtMs = %d/%d, want %d (from Store's Clock)", got.CreatedAtMs, got.UpdatedAtMs, wantMs)
	}
	if len(got.Argv) != 3 || got.Argv[1] != "--session-id" {
		t.Fatalf("Argv round-trip = %v", got.Argv)
	}
	if len(got.EnvKeys) != 3 {
		t.Fatalf("EnvKeys round-trip = %v", got.EnvKeys)
	}

	fetched, err := st.GetSession(ctx, "id-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if fetched.Name != "work-1" {
		t.Fatalf("GetSession returned %+v", fetched)
	}

	byName, err := st.GetSessionByName(ctx, "work-1")
	if err != nil {
		t.Fatalf("GetSessionByName: %v", err)
	}
	if byName.ID != "id-1" {
		t.Fatalf("GetSessionByName returned %+v", byName)
	}
}

func TestStore_GetSession_NotFound(t *testing.T) {
	st, _ := openTestStore(t)
	_, err := st.GetSession(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSession: err = %v, want ErrNotFound", err)
	}

	_, err = st.GetSessionByName(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSessionByName: err = %v, want ErrNotFound", err)
	}
}

func TestStore_CreateSession_DuplicateNameIsTypedError(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateSession(ctx, sampleCreateParams("id-1", "dup-name")); err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}
	_, err := st.CreateSession(ctx, sampleCreateParams("id-2", "dup-name"))
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("second CreateSession: err = %v, want ErrDuplicateName", err)
	}
}

func TestStore_ListSessions_OrderedByCreation(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateSession(ctx, sampleCreateParams("id-1", "first")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	fc.Advance(time.Second)
	if _, err := st.CreateSession(ctx, sampleCreateParams("id-2", "second")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	fc.Advance(time.Second)
	if _, err := st.CreateSession(ctx, sampleCreateParams("id-3", "third")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	list, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len(list) = %d, want 3", len(list))
	}
	for i, wantName := range []string{"first", "second", "third"} {
		if list[i].Name != wantName {
			t.Fatalf("list[%d].Name = %q, want %q", i, list[i].Name, wantName)
		}
	}
}

func TestStore_UpdateSession(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	created, err := st.CreateSession(ctx, sampleCreateParams("id-1", "to-update"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	fc.Advance(5 * time.Minute)

	updated, err := st.UpdateSession(ctx, "id-1", func(s *session.Session) {
		s.Status = session.StatusRunning
		code := 0
		s.ExitCode = &code
		s.ExitSignal = ""
		now := fc.Now().UnixMilli()
		s.StartedAtMs = &now
	})
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}

	if updated.Status != session.StatusRunning {
		t.Fatalf("Status = %q, want running", updated.Status)
	}
	if updated.ExitCode == nil || *updated.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want pointer to 0", updated.ExitCode)
	}
	if updated.StartedAtMs == nil {
		t.Fatalf("StartedAtMs = nil, want set")
	}
	if updated.CreatedAtMs != created.CreatedAtMs {
		t.Fatalf("CreatedAtMs changed: got %d, want unchanged %d", updated.CreatedAtMs, created.CreatedAtMs)
	}
	if updated.UpdatedAtMs != fc.Now().UnixMilli() {
		t.Fatalf("UpdatedAtMs = %d, want %d (bumped to current clock time)", updated.UpdatedAtMs, fc.Now().UnixMilli())
	}
	if updated.ID != "id-1" {
		t.Fatalf("ID = %q, want unchanged id-1", updated.ID)
	}

	// Re-fetch to make sure the write actually persisted, not just the
	// in-memory returned struct.
	refetched, err := st.GetSession(ctx, "id-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if refetched.Status != session.StatusRunning {
		t.Fatalf("persisted Status = %q, want running", refetched.Status)
	}
}

func TestStore_AppendAndListEvents_Order(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateSession(ctx, sampleCreateParams("id-1", "ev-session")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	kinds := []session.EventKind{
		session.EventSessionCreated,
		session.EventSessionSpawned,
		session.EventSessionAttached,
		session.EventSessionDetached,
	}
	for _, k := range kinds {
		fc.Advance(time.Second)
		if _, err := st.AppendEvent(ctx, "id-1", k, ""); err != nil {
			t.Fatalf("AppendEvent(%s): %v", k, err)
		}
	}

	events, err := st.ListEvents(ctx, "id-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != len(kinds) {
		t.Fatalf("len(events) = %d, want %d", len(events), len(kinds))
	}
	var lastSeq int64
	for i, ev := range events {
		if ev.Kind != kinds[i] {
			t.Fatalf("events[%d].Kind = %q, want %q (append order)", i, ev.Kind, kinds[i])
		}
		if ev.Seq <= lastSeq {
			t.Fatalf("events[%d].Seq = %d, not strictly increasing after %d", i, ev.Seq, lastSeq)
		}
		lastSeq = ev.Seq
		if ev.DataJSON != "{}" {
			t.Fatalf("events[%d].DataJSON = %q, want default {}", i, ev.DataJSON)
		}
	}
}

func TestStore_AppendEvent_DaemonScoped(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	if _, err := st.AppendEvent(ctx, "", session.EventDaemonStarted, ""); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	events, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != session.EventDaemonStarted {
		t.Fatalf("daemon-scoped events = %+v", events)
	}
	if events[0].SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for daemon-scoped event", events[0].SessionID)
	}
}

func TestStore_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Now())
	path := filepath.Join(dir, "corral.db")

	st, err := Open(path, fc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Force a write so WAL/SHM siblings actually exist, then re-chmod (Open
	// already chmods once right after migrations, but migrations
	// themselves are enough to create -wal in WAL mode; assert regardless).
	if _, err := st.AppendEvent(context.Background(), "", session.EventDaemonStarted, ""); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := chmodDBFiles(path); err != nil {
		t.Fatalf("chmodDBFiles: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := path + suffix
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 0600", p, info.Mode().Perm())
		}
	}
}

func TestStore_EnvKeysJSON_NeverContainsSecretValues(t *testing.T) {
	// §10.3 security test: env_keys_json in the DB contains ANTHROPIC_API_KEY
	// as a NAME, but the raw DB file bytes must never contain a plausible
	// secret VALUE.
	dir := t.TempDir()
	fc := clocktest.NewFake(time.Now())
	path := filepath.Join(dir, "corral.db")
	st, err := Open(path, fc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const secretValue = "sk-ant-super-secret-value-should-never-be-persisted"
	params := sampleCreateParams("id-1", "secret-test")
	params.EnvKeys = []string{"HOME", "PATH", "ANTHROPIC_API_KEY"}
	if _, err := st.CreateSession(context.Background(), params); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	st.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(raw, []byte("ANTHROPIC_API_KEY")) {
		t.Fatalf("expected env_keys_json to contain the key NAME ANTHROPIC_API_KEY")
	}
	if bytes.Contains(raw, []byte(secretValue)) {
		t.Fatalf("DB file bytes contain a secret VALUE, must contain names only")
	}
}
