//go:build darwin || linux

package diskspace

import (
	"fmt"
	"syscall"
)

// FreeBytes returns bytes available to the current unprivileged user on the
// filesystem containing path.
func FreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("diskspace: statfs %s: %w", path, err)
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1), nil
	}
	return int64(free), nil
}
