package supervisor

import (
	"os"

	"golang.org/x/sys/unix"
)

// setPTYSize applies TIOCSWINSZ to f while holding the runtime's fd
// reference via SyscallConn, so a concurrent Close (the reaper closes
// PTYMaster the moment cmd.Wait returns) yields a clean "file already
// closed" error instead of an ioctl against a reused fd number —
// creack/pty's Setsize reads f.Fd() raw and has exactly that hazard.
func setPTYSize(f *os.File, rows, cols uint16) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := sc.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	return ioctlErr
}
