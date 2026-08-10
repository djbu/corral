package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

func TestSpawnRejectsCapacityBeforeCreatingArtifacts(t *testing.T) {
	stateDir := t.TempDir()
	r := New(nil, nil, nil, clock.Real(), Config{StateDir: stateDir, MaxInteractiveSessions: 1},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.register(&LiveSession{SessionID: "already-live"}, "already-live")

	_, err := r.Spawn(context.Background(), session.Spec{ID: "over-limit", Name: "over-limit", Mode: session.ModeInteractive})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("Spawn error = %v, want ErrCapacity", err)
	}
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Limit != 1 || capacity.InUse != 1 {
		t.Fatalf("capacity error = %+v", capacity)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "sessions")); !os.IsNotExist(err) {
		t.Fatalf("spawn created artifacts before capacity rejection: %v", err)
	}
}

func TestCapacityReservationsAreModeSpecificAndRelease(t *testing.T) {
	r := New(nil, nil, nil, clock.Real(), Config{MaxInteractiveSessions: 1, MaxHeadlessTasks: 1}, nil)
	if err := r.reserveCapacity("interactive-1", session.ModeInteractive); err != nil {
		t.Fatal(err)
	}
	if err := r.reserveCapacity("headless-1", session.ModeHeadless); err != nil {
		t.Fatal(err)
	}
	if err := r.reserveCapacity("interactive-2", session.ModeInteractive); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second interactive reservation = %v, want ErrCapacity", err)
	}
	r.releaseCapacity("interactive-1")
	if err := r.reserveCapacity("interactive-2", session.ModeInteractive); err != nil {
		t.Fatalf("reservation after release = %v", err)
	}
}

func TestCapacityReservationsAreAtomicUnderConcurrency(t *testing.T) {
	r := New(nil, nil, nil, clock.Real(), Config{MaxInteractiveSessions: 1}, nil)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- r.reserveCapacity(fmt.Sprintf("concurrent-%d", i), session.ModeInteractive)
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, ErrCapacity) {
			t.Fatalf("reservation error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful reservations = %d, want exactly 1", successes)
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
