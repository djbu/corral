package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
)

// cmdKill implements `corral kill <name-or-id> [--grace DURATION]` (design
// doc §9.2): DELETE /v1/sessions/{idOrName}, which sets desired_state
// "stopped" before signalling so a daemon crash mid-kill never resurrects
// the session on restart.
func cmdKill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	grace := fs.Duration("grace", 0, "override the SIGTERM-to-SIGKILL grace period for this kill")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: corral kill <name-or-id> [--grace DURATION]")
		return exitUsage
	}
	idOrName := fs.Arg(0)

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: kill: %v\n", err)
		return exitError
	}
	c := client.New(cfg.Socket, stderr)

	sess, err := c.KillSession(context.Background(), idOrName, *grace)
	if err != nil {
		fmt.Fprintf(stderr, "corral: kill: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "killed %s (status=%s)\n", sess.Name, sess.Status)
	return exitOK
}
