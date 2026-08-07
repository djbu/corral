package procinfo

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StartTime returns pid's start time in epoch nanoseconds, read via
// sysctl(KERN_PROC_PID) (design doc §1's "sysctl KERN_PROC_PID").
// golang.org/x/sys/unix.SysctlKinfoProc wraps the mib lookup;
// kp.Proc.P_starttime is the kernel's kinfo_proc.p_starttime (a struct
// timeval), which the kernel sets once at fork and never updates for the
// life of the process — exactly the stability Alive depends on.
func StartTime(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("procinfo: sysctl kern.proc.pid %d: %w", pid, err)
	}
	tv := kp.Proc.P_starttime
	return tv.Sec*1e9 + int64(tv.Usec)*1e3, nil
}
