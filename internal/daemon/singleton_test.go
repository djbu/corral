package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestAcquireLock_SecondDaemonRefused: holding the flock in-process (as the
// first daemon would) must make a second AcquireLock on the same state_dir
// fail with ErrAlreadyRunning, never silently succeed (design doc §3.2 step
// 4: the flock is the sole mutual-exclusion primitive).
func TestAcquireLock_SecondDaemonRefused(t *testing.T) {
	dir := t.TempDir()

	f1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer ReleaseLock(f1)

	_, err = AcquireLock(dir)
	if err != ErrAlreadyRunning {
		t.Fatalf("second AcquireLock error = %v, want ErrAlreadyRunning", err)
	}
}

// TestAcquireLock_ReleasedLockIsReacquirable makes sure releasing really
// frees the flock rather than merely appearing to.
func TestAcquireLock_ReleasedLockIsReacquirable(t *testing.T) {
	dir := t.TempDir()

	f1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if err := ReleaseLock(f1); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	f2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	ReleaseLock(f2)
}

// TestListen_StaleSocketIsRemovedAndReplaced: a socket file left behind by
// an unclean shutdown (nothing listening on it) must be transparently
// cleaned up and replaced, not treated as "another daemon is running"
// (design doc §3.3's dial-probe).
func TestListen_StaleSocketIsRemovedAndReplaced(t *testing.T) {
	sockPath := filepath.Join(shortTempDir(t), "corral.sock")

	// Create a stale socket file the way an unclean shutdown (e.g.
	// SIGKILL) would leave one: a bound AF_UNIX socket special file with
	// nothing listening on it. net.Listen's own Close() unlinks the file
	// it created (Go's default SetUnlinkOnClose), so that path can't
	// simulate this — bind and close via the raw syscalls instead, which
	// leaves the file on disk exactly like a daemon that never got the
	// chance to clean up after itself.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket(2): %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: sockPath}); err != nil {
		unix.Close(fd)
		t.Fatalf("bind(2): %v", err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatalf("close(2): %v", err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("expected stale socket file to still exist on disk: %v", err)
	}

	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	defer ln.Close()

	// It must actually be live now.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dialing replacement socket: %v", err)
	}
	conn.Close()
}

// TestListen_LiveSocketWithoutLockIsRefused: if something is actually
// listening on sockPath (a state that should be impossible under correct
// operation since Listen is only ever called under the flock), Listen must
// refuse loudly rather than silently taking over.
func TestListen_LiveSocketWithoutLockIsRefused(t *testing.T) {
	sockPath := filepath.Join(shortTempDir(t), "corral.sock")

	live, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("creating live socket: %v", err)
	}
	defer live.Close()

	_, err = Listen(sockPath)
	if err == nil {
		t.Fatalf("Listen over a live socket: want error, got nil")
	}
}
