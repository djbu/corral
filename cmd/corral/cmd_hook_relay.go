package main

import (
	"context"
	"flag"
	"io"
	"os"

	"github.com/danielbecerra/corral/internal/hookrelay"
)

// cmdHookRelay implements the hidden `corral hook-relay --event <Name>`
// subcommand (design doc §2.4). Claude Code's generated hooks{} settings
// block invokes this as the hook command itself, piping the raw hook
// payload JSON to stdin; corral never types this command by hand.
//
// It always returns exitOK, no matter what happens inside — design doc
// §2.4: "Exit 0 always, unconditionally... a non-zero hook exit is a
// signal Claude Code interprets (blocking the tool, injecting stderr as
// context). Corral is an observer in M2; an observer that can break the
// agent when the daemon is down is a liability, not a feature." That
// includes a bad --event flag: even a corral CLI bug here must not be able
// to break the agent's turn.
func cmdHookRelay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	event := fs.String("event", "", "the hook event name Claude Code invoked this command for")
	if err := fs.Parse(args); err != nil {
		return exitOK
	}

	hookrelay.Run(context.Background(), *event, os.Stdin, stdout, stderr, os.Getenv)
	return exitOK
}
