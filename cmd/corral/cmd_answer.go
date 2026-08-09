package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/danielbecerra/corral/internal/api/client"
)

// cmdAnswer implements `corral answer <session> "text"` and
// `corral answer <session> --key <name>` (design doc §7): POST
// /v1/sessions/{idOrName}/answer, which writes bytes to the session's live
// PTY master as if a human had typed them. It never changes agent_state —
// a session leaves "blocked" only when a later hook proves it, so there is
// deliberately no --approve sugar here.
func cmdAnswer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "send a named key (enter, esc, up, down, tab, ctrl-c) instead of text")
	noNewline := fs.Bool("no-newline", false, "do not append a trailing newline after text (ignored for --key)")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	const usage = "usage: corral answer <session> \"text\" | corral answer <session> --key <name>"

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	session := fs.Arg(0)

	var text string
	if *key == "" {
		if fs.NArg() != 2 {
			fmt.Fprintln(stderr, usage)
			return exitUsage
		}
		text = fs.Arg(1)
	} else {
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "corral: answer: cannot pass both a text argument and --key")
			fmt.Fprintln(stderr, usage)
			return exitUsage
		}
	}

	c, err := newClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: answer: %v\n", err)
		return exitError
	}

	_, err = c.Answer(context.Background(), session, text, *key, !*noNewline)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == client.CodeSessionNotLive {
			fmt.Fprintf(stderr, "corral: session %s is not live\n", session)
			return exitError
		}
		fmt.Fprintf(stderr, "corral: answer: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "answered %s\n", session)
	return exitOK
}
