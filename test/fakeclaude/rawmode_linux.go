package main

import "golang.org/x/sys/unix"

// getTermiosReq/setTermiosReq are linux's ioctl request numbers for
// reading/writing termios. Go's automatic _darwin/_linux filename build
// constraints select the right one per platform — the same pattern
// internal/procinfo already uses for StartTime, chosen here for the
// same reason: CI's matrix is macos+ubuntu (design doc §10.2's procinfo
// row), so both must ship.
const (
	getTermiosReq = unix.TCGETS
	setTermiosReq = unix.TCSETS
)
