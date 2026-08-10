package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/djbu/corral/internal/api/client"
)

// LifecycleState is the externally visible daemon lifecycle classification.
type LifecycleState string

const (
	LifecycleRunning    LifecycleState = "running"
	LifecycleStarting   LifecycleState = "starting"
	LifecycleStopping   LifecycleState = "stopping"
	LifecycleLockHeld   LifecycleState = "lock-held"
	LifecycleStalePID   LifecycleState = "stale-pid"
	LifecycleStaleFiles LifecycleState = "stale-files"
	LifecycleStopped    LifecycleState = "stopped"
)

const lifecycleFilename = "daemon.state"

// LifecycleStatus combines authoritative lock/socket evidence with optional
// daemon metadata. PID is diagnostic only and must never be used as a signal
// target by lifecycle commands.
type LifecycleStatus struct {
	State         LifecycleState `json:"state"`
	PID           int            `json:"pid,omitempty"`
	Socket        string         `json:"socket"`
	StateDir      string         `json:"state_dir"`
	Version       string         `json:"version,omitempty"`
	APIVersion    int            `json:"api_version,omitempty"`
	SchemaVersion int            `json:"schema_version,omitempty"`
	StartedAt     string         `json:"started_at,omitempty"`
	UptimeMs      int64          `json:"uptime_ms,omitempty"`
}

// InspectLifecycle classifies the daemon without mutating its PID, socket, or
// state marker. A successful API response wins; otherwise the flock decides
// whether marker/PID files are live evidence or stale leftovers.
func InspectLifecycle(ctx context.Context, stateDir, socket string, stderr io.Writer) (LifecycleStatus, error) {
	status := LifecycleStatus{State: LifecycleStopped, Socket: socket, StateDir: stateDir}
	if stderr == nil {
		stderr = io.Discard
	}
	if v, err := client.New(socket, stderr).Version(ctx); err == nil {
		status.State = LifecycleRunning
		status.PID = v.PID
		status.Version = v.DaemonVersion
		status.APIVersion = v.APIVersion
		status.SchemaVersion = v.SchemaVersion
		status.StartedAt = v.StartedAt
		status.UptimeMs = v.UptimeMs
		return status, nil
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return LifecycleStatus{}, fmt.Errorf("daemon lifecycle: stat state_dir %s: %w", stateDir, err)
	}
	if !info.IsDir() {
		return LifecycleStatus{}, fmt.Errorf("daemon lifecycle: state_dir %s is not a directory", stateDir)
	}

	pidPath := filepath.Join(stateDir, "daemon.pid")
	pidExists := false
	if data, readErr := os.ReadFile(pidPath); readErr == nil {
		pidExists = true
		status.PID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	} else if !os.IsNotExist(readErr) {
		return LifecycleStatus{}, fmt.Errorf("daemon lifecycle: reading %s: %w", pidPath, readErr)
	}

	markerPath := filepath.Join(stateDir, lifecycleFilename)
	marker := ""
	markerExists := false
	if data, readErr := os.ReadFile(markerPath); readErr == nil {
		markerExists = true
		marker = strings.TrimSpace(string(data))
	} else if !os.IsNotExist(readErr) {
		return LifecycleStatus{}, fmt.Errorf("daemon lifecycle: reading %s: %w", markerPath, readErr)
	}

	lockHeld, err := lifecycleLockHeld(filepath.Join(stateDir, "daemon.lock"))
	if err != nil {
		return LifecycleStatus{}, err
	}
	if lockHeld {
		switch marker {
		case string(LifecycleStarting):
			status.State = LifecycleStarting
		case string(LifecycleStopping):
			status.State = LifecycleStopping
		default:
			status.State = LifecycleLockHeld
		}
		return status, nil
	}

	if pidExists {
		status.State = LifecycleStalePID
		return status, nil
	}
	if markerExists || pathExists(socket) {
		status.State = LifecycleStaleFiles
	}
	return status, nil
}

func lifecycleLockHeld(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("daemon lifecycle: opening %s: %w", path, err)
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return true, nil
		}
		return false, fmt.Errorf("daemon lifecycle: probing lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		return false, fmt.Errorf("daemon lifecycle: releasing probe lock %s: %w", path, err)
	}
	return false, nil
}

func pathExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err == nil
}

func writeLifecycleState(stateDir string, state LifecycleState) error {
	if state != LifecycleStarting && state != LifecycleRunning && state != LifecycleStopping {
		return fmt.Errorf("daemon lifecycle: invalid persisted state %q", state)
	}
	tmp, err := os.CreateTemp(stateDir, ".daemon.state-")
	if err != nil {
		return fmt.Errorf("daemon lifecycle: creating marker: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("daemon lifecycle: chmod marker: %w", err)
	}
	if _, err := fmt.Fprintf(tmp, "%s\n", state); err != nil {
		tmp.Close()
		return fmt.Errorf("daemon lifecycle: writing marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("daemon lifecycle: syncing marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("daemon lifecycle: closing marker: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(stateDir, lifecycleFilename)); err != nil {
		return fmt.Errorf("daemon lifecycle: replacing marker: %w", err)
	}
	return nil
}
