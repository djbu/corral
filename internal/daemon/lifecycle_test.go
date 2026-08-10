package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectLifecycleStoppedAndStaleFiles(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "missing")
	socket := filepath.Join(root, "corral.sock")

	status := inspectLifecycleForTest(t, stateDir, socket)
	if status.State != LifecycleStopped {
		t.Fatalf("missing state dir = %q, want stopped", status.State)
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, lifecycleFilename), []byte("running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status = inspectLifecycleForTest(t, stateDir, socket)
	if status.State != LifecycleStaleFiles {
		t.Fatalf("stale marker = %q, want stale-files", status.State)
	}

	if err := os.WriteFile(filepath.Join(stateDir, "daemon.pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status = inspectLifecycleForTest(t, stateDir, socket)
	if status.State != LifecycleStalePID || status.PID != 4242 {
		t.Fatalf("stale pid = %+v, want stale-pid pid=4242", status)
	}
}

func TestInspectLifecycleHeldLockUsesTrustedMarker(t *testing.T) {
	for _, tc := range []struct {
		marker string
		want   LifecycleState
	}{
		{"starting", LifecycleStarting},
		{"stopping", LifecycleStopping},
		{"running", LifecycleLockHeld},
		{"garbage", LifecycleLockHeld},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			stateDir := t.TempDir()
			lock, err := AcquireLock(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer ReleaseLock(lock)
			if err := os.WriteFile(filepath.Join(stateDir, lifecycleFilename), []byte(tc.marker+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			status := inspectLifecycleForTest(t, stateDir, filepath.Join(stateDir, "missing.sock"))
			if status.State != tc.want {
				t.Fatalf("state = %q, want %q", status.State, tc.want)
			}
		})
	}
}

func TestWriteLifecycleStateAtomicAndRestricted(t *testing.T) {
	stateDir := t.TempDir()
	for _, state := range []LifecycleState{LifecycleStarting, LifecycleRunning, LifecycleStopping} {
		if err := writeLifecycleState(stateDir, state); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(stateDir, lifecycleFilename)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(data), string(state)+"\n"; got != want {
			t.Fatalf("marker = %q, want %q", got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode = %o, want 600", got)
		}
	}
	if err := writeLifecycleState(stateDir, LifecycleStalePID); err == nil {
		t.Fatal("invalid persisted state unexpectedly succeeded")
	}
}

func inspectLifecycleForTest(t *testing.T, stateDir, socket string) LifecycleStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	status, err := InspectLifecycle(ctx, stateDir, socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	return status
}
