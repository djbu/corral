package procinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// clockTicksPerSecond is the USER_HZ value /proc/<pid>/stat's starttime
// field is expressed in. The correct value is sysconf(_SC_CLK_TCK), but
// that requires cgo, which this repo's build gate explicitly forbids
// (CGO_ENABLED=0 must build, per §11's per-step green gate). 100 is the
// value on effectively every modern Linux distribution/architecture pair
// (it has been the kernel default since well before any distro this
// project targets); a system that overrides it would make StartTime's
// nanosecond value imprecise but would not break Alive's actual
// correctness guarantee, since Alive only ever compares two StartTime
// results computed with this same constant for equality — see the
// deviations list.
const clockTicksPerSecond = 100

// StartTime returns pid's start time in epoch nanoseconds, computed from
// /proc/<pid>/stat field 22 (design doc §1's "/proc/<pid>/stat field 22"),
// which is the process's start time in clock ticks since boot, plus
// /proc/stat's "btime" line (boot time, seconds since epoch).
func StartTime(pid int) (int64, error) {
	btime, err := bootTimeSec()
	if err != nil {
		return 0, err
	}

	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, fmt.Errorf("procinfo: read %s: %w", statPath, err)
	}

	ticks, err := parseStartTimeTicks(string(data))
	if err != nil {
		return 0, fmt.Errorf("procinfo: %s: %w", statPath, err)
	}

	startSec := float64(btime) + float64(ticks)/float64(clockTicksPerSecond)
	return int64(startSec * 1e9), nil
}

// parseStartTimeTicks extracts field 22 (starttime) from the raw content
// of /proc/<pid>/stat. Field 2 (comm, the executable's basename in
// parens) can itself contain spaces and parentheses — e.g. a process
// named "(sd-pam)" — so fields cannot be split naively on whitespace from
// the start of the line; this splits on the *last* ')' instead, which is
// what every correct /proc/stat parser does, since comm is the only field
// that can contain either character.
func parseStartTimeTicks(stat string) (int64, error) {
	close := strings.LastIndex(stat, ")")
	if close < 0 || close+2 > len(stat) {
		return 0, fmt.Errorf("malformed stat line (no ')')")
	}
	// rest[0] is field 3 (state); field 22 is therefore rest[22-3] = rest[19].
	rest := strings.Fields(stat[close+2:])
	const startTimeRestIndex = 22 - 3
	if len(rest) <= startTimeRestIndex {
		return 0, fmt.Errorf("too few fields after comm (%d)", len(rest))
	}
	ticks, err := strconv.ParseInt(rest[startTimeRestIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime field: %w", err)
	}
	return ticks, nil
}

// bootTimeSec reads /proc/stat's "btime" line: system boot time, in
// seconds since the Unix epoch.
func bootTimeSec() (int64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("procinfo: open /proc/stat: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("procinfo: parse /proc/stat btime: %w", err)
			}
			return n, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("procinfo: scan /proc/stat: %w", err)
	}
	return 0, fmt.Errorf("procinfo: btime not found in /proc/stat")
}
