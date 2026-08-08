package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
)

// cmdWake implements `corral wake <name-or-id>` (design doc's deferred
// "woken only on demand" half of idle-reap): POST
// /v1/sessions/{idOrName}/wake, which resumes a reaped/stopped-but-resumable
// session in place.
func cmdWake(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: corral wake <name-or-id>")
		return exitUsage
	}
	idOrName := fs.Arg(0)

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: wake: %v\n", err)
		return exitError
	}
	c := client.New(cfg.Socket, stderr)

	sess, err := c.WakeSession(context.Background(), idOrName)
	if err != nil {
		fmt.Fprintf(stderr, "corral: wake: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "woke %s (status=%s)\n", sess.Name, sess.Status)
	return exitOK
}
