package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
)

// cmdNew implements `corral new [--cwd DIR] [--name NAME] [--model MODEL]
// [--rows N] [--cols N] [--no-attach]` (design doc §9.2): resolves --cwd to
// an absolute path client-side (default $PWD), POSTs it to the daemon, and
// prints an attach hint — attaching itself is step 10's `corral attach`.
func cmdNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cwdFlag := fs.String("cwd", "", "working directory for the session (default: current directory)")
	name := fs.String("name", "", "session name (default: <basename(cwd)>-<n>)")
	model := fs.String("model", "", "claude --model override")
	rows := fs.Uint("rows", 0, "initial PTY rows (default: 40)")
	cols := fs.Uint("cols", 0, "initial PTY cols (default: 120)")
	noAttach := fs.Bool("no-attach", false, "do not attach after creating the session")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cwd := *cwdFlag
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "corral: new: %v\n", err)
			return exitError
		}
		cwd = wd
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "corral: new: %v\n", err)
		return exitError
	}

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: new: %v\n", err)
		return exitError
	}
	c := client.New(cfg.Socket, stderr)

	sess, err := c.CreateSession(context.Background(), client.CreateSessionRequest{
		Name:  *name,
		Cwd:   abs,
		Model: *model,
		Rows:  uint16(*rows),
		Cols:  uint16(*cols),
	})
	if err != nil {
		fmt.Fprintf(stderr, "corral: new: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "created %s (%s)\n", sess.Name, sess.ID)
	if !*noAttach {
		fmt.Fprintf(stdout, "created; attach with: corral attach %s\n", sess.Name)
	}
	return exitOK
}
