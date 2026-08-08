// Package daemon wires together config, slog, the store, the API server,
// and the setsid-relaunched process lifecycle into corral's daemon body
// (design doc §3). singleton.go implements §3.2 step 4 and §3.3: the flock
// that is the sole mutual-exclusion primitive for "only one daemon per
// state_dir", and the unix-socket acquisition with its dial-probe stale-
// socket detection.
package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ErrAlreadyRunning is returned by AcquireLock when another process
// already holds the exclusive flock on daemon.lock.
var ErrAlreadyRunning = errors.New("daemon: another corral daemon holds the lock")

// staleSocketProbeTimeout is how long Listen waits for a live daemon to
// answer a stale-looking socket before concluding it really is stale
// (design doc §3.3).
const staleSocketProbeTimeout = 500 * time.Millisecond

// AcquireLock opens (creating if necessary) <stateDir>/daemon.lock and
// takes an exclusive, non-blocking flock on it. This must succeed before
// anything else in stateDir — the socket, the store — is touched (§3.2 step
// 4: "the flock mutual-exclusion primitive; the PID file advisory only,
// must never be consulted for liveness"). The returned *os.File must be
// kept open for the flock's lifetime; closing it (ReleaseLock) releases the
// lock.
func AcquireLock(stateDir string) (*os.File, error) {
	path := filepath.Join(stateDir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: opening %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("daemon: flock %s: %w", path, err)
	}
	return f, nil
}

// ReleaseLock closes f, releasing the flock taken by AcquireLock. flock is
// released automatically on close regardless, but shutdown.go calls this
// explicitly so the release has its own visible call site (§3.6 step 6).
func ReleaseLock(f *os.File) error {
	return f.Close()
}

// Listen acquires the unix socket at sockPath (design doc §3.3). It is only
// ever called while AcquireLock's flock is held, which is what makes the
// dial-probe below decidable rather than racy: if net.Listen fails because
// the path already exists, a live listener answering it would mean the
// socket survived from a daemon that no longer holds our lock — a state
// that should be impossible under correct operation, so it is refused
// loudly rather than silently taken over. If nobody answers within
// staleSocketProbeTimeout, the socket is stale (left behind by an
// unclean shutdown, e.g. SIGKILL); it is removed and Listen retries
// exactly once.
func Listen(sockPath string) (net.Listener, error) {
	ln, err := net.Listen("unix", sockPath)
	if err == nil {
		return ln, nil
	}
	if !addrInUse(err) {
		return nil, fmt.Errorf("daemon: listening on %s: %w", sockPath, err)
	}

	if conn, derr := net.DialTimeout("unix", sockPath, staleSocketProbeTimeout); derr == nil {
		conn.Close()
		return nil, fmt.Errorf("daemon: socket %s is live but the lock is free; refusing to start", sockPath)
	}

	if rerr := os.Remove(sockPath); rerr != nil && !os.IsNotExist(rerr) {
		return nil, fmt.Errorf("daemon: removing stale socket %s: %w", sockPath, rerr)
	}
	ln, err = net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: listening on %s after removing stale socket: %w", sockPath, err)
	}
	return ln, nil
}

func addrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || os.IsExist(err)
}
