package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/danielbecerra/corral/internal/daemon"
)

// fdReadyPipe is fd 3: cmd_daemon.go's launcher passes the write end of its
// ready pipe as this subcommand's third inherited file descriptor via
// exec.Cmd.ExtraFiles (design doc §3.1).
const fdReadyPipe = 3

// cmdDaemonRun implements the hidden `corral daemon-run` subcommand: the
// detached daemon body itself, always launched by cmd_daemon.go (never
// meant to be typed by a human). It opens fd 3 as the ready pipe and hands
// off entirely to daemon.Main, which runs the full startup sequence,
// writes "OK\n"/"ERR: <msg>\n" to that pipe, and then blocks in the signal
// loop until a graceful shutdown completes.
func cmdDaemonRun(args []string, stdout, stderr io.Writer) int {
	readyW := os.NewFile(uintptr(fdReadyPipe), "ready-pipe")
	if readyW == nil {
		fmt.Fprintln(stderr, "corral: daemon-run: fd 3 (ready pipe) is not open; this subcommand is only meant to be launched by `corral daemon`")
		return exitError
	}

	if err := daemon.Main(context.Background(), readyW); err != nil {
		return exitError
	}
	return exitOK
}
