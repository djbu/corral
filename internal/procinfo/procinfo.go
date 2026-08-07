// Package procinfo quarantines the platform-specific process bookkeeping
// corral needs to safely resume and tear down supervised sessions across a
// daemon restart (design doc §7.3): a session's persisted pid alone is not
// enough to know "is this still my child", because pids are recycled by
// the OS. StartTime(pid) — implemented per-platform in procinfo_darwin.go
// (sysctl KERN_PROC_PID) and procinfo_linux.go (/proc/<pid>/stat field
// 22) — gives a value that is stable for a process's entire lifetime and
// changes if pid is reused by a later, unrelated process. Alive and
// KillGroup, defined here once, build the two operations the supervisor
// actually needs on top of that primitive.
package procinfo

import (
	"fmt"
	"syscall"
)

// StartTime is implemented per-platform (procinfo_darwin.go,
// procinfo_linux.go — each restricted to its OS purely by the "_darwin"/
// "_linux" filename suffix, no explicit build tag needed) and returns
// pid's start time as a monotonically-ordered value in nanoseconds
// (darwin: wall-clock epoch nanoseconds, from the kernel's
// kinfo_proc.p_starttime; linux: /proc/<pid>/stat field 22 converted from
// boot-relative clock ticks to epoch nanoseconds via /proc/stat's btime —
// see procinfo_linux.go's clockTicksPerSecond note on why that conversion
// is approximate). Callers must only ever compare two StartTime results
// for equality (as Alive does) or store the value for a later such
// comparison — never treat it as an authoritative wall-clock timestamp.

// Alive reports whether pid is still running the exact process that had
// startNs as its recorded start time — not merely whether some process
// currently has that pid. This is what makes a persisted (pid, startNs)
// pair from before a daemon restart safe to act on: if the OS reused pid
// for an unrelated process in the meantime, StartTime(pid) now returns a
// different value (or an error, if pid is not running at all) and Alive
// correctly reports false rather than mistaking a stranger for our child.
func Alive(pid int, startNs int64) bool {
	if startNs == 0 {
		// A zero recorded start time can only mean "never actually
		// observed" (StartTime itself never returns 0 without an error
		// alongside it) — never treat it as a value that could
		// legitimately match.
		return false
	}
	cur, err := StartTime(pid)
	if err != nil {
		return false
	}
	return cur == startNs
}

// KillGroup sends sig to every process in group pgid (via kill(-pgid,
// sig), the standard "signal a process group" idiom), refusing to do so
// for two cases that would otherwise be catastrophic if a caller ever
// passed a zero-value or miscomputed pgid by mistake:
//   - pgid <= 1: 0 and negative values address something other than an
//     intended child's group under kill(2)'s own rules, and 1 is init's
//     process group on both darwin and linux — signaling it can bring
//     down the entire machine's process tree.
//   - pgid == the caller's own process group: corral's daemon process
//     itself, which would signal (and, for SIGKILL, terminate) the
//     supervising daemon rather than the session it meant to tear down.
func KillGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("procinfo: refusing to signal process group %d (<=1)", pgid)
	}
	if own := syscall.Getpgrp(); pgid == own {
		return fmt.Errorf("procinfo: refusing to signal our own process group %d", pgid)
	}
	if err := syscall.Kill(-pgid, sig); err != nil {
		return fmt.Errorf("procinfo: kill process group %d with %v: %w", pgid, sig, err)
	}
	return nil
}
