package main

import "golang.org/x/sys/unix"

// getTermiosReq/setTermiosReq are darwin's ioctl request numbers for
// reading/writing termios — see rawmode_linux.go for why this needs a
// per-platform file at all (the request numbers, unlike the Termios
// struct's field layout, are not shared across unix flavors).
const (
	getTermiosReq = unix.TIOCGETA
	setTermiosReq = unix.TIOCSETA
)
