package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

func TestSpawnRejectsLowDiskBeforeCreatingArtifacts(t *testing.T) {
	stateDir := t.TempDir()
	r := New(nil, nil, nil, clock.Real(), Config{
		StateDir:     stateDir,
		MinFreeBytes: 256 << 20,
		FreeBytes: func(path string) (int64, error) {
			if path != stateDir {
				t.Fatalf("free-space path = %q, want %q", path, stateDir)
			}
			return 128 << 20, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := r.Spawn(context.Background(), session.Spec{ID: "low-disk", Name: "low-disk"})
	if !errors.Is(err, ErrLowDisk) {
		t.Fatalf("Spawn error = %v, want ErrLowDisk", err)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "sessions")); !os.IsNotExist(err) {
		t.Fatalf("spawn created artifacts before disk rejection: %v", err)
	}
}

func TestLowDiskPersistsFailedAttemptAndEvent(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	st, err := store.Open(filepath.Join(stateDir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.CreateSession(ctx, store.CreateSessionParams{
		ID: "low-disk", Name: "low-disk", Mode: session.ModeInteractive,
		Cwd: stateDir, ClaudeBin: "/bin/true", Argv: []string{}, EnvKeys: []string{},
		SettingsPath: "/settings", SettingSources: "user",
		DesiredState: session.DesiredRunning, Status: session.StatusStarting,
		Rows: 24, Cols: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, nil, nil, clock.Real(), Config{
		StateDir: stateDir, MinFreeBytes: 2, FreeBytes: func(string) (int64, error) { return 1, nil },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r.Spawn(ctx, session.Spec{ID: "low-disk", Name: "low-disk"}); !errors.Is(err, ErrLowDisk) {
		t.Fatalf("Spawn error = %v", err)
	}
	rec, err := st.GetSession(ctx, "low-disk")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != session.StatusFailed {
		t.Fatalf("status = %s, want failed", rec.Status)
	}
	events, err := st.ListEvents(ctx, "low-disk")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != session.EventSessionSpawnFailed {
		t.Fatalf("events = %+v", events)
	}
}
