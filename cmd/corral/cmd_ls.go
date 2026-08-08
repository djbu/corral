package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
)

// cmdLs implements `corral ls [--json]` (design doc §9.2): NAME STATE
// ATTACHED CWD UPTIME PID, or the raw JSON array with --json. Uptime is
// computed client-side from created_at (or started_at once running) against
// wall-clock time.Now() — a CLI-side exemption from the daemon-side
// no-time.Now() rule, since this is display-only and never persisted.
func cmdLs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the raw session array as JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: ls: %v\n", err)
		return exitError
	}
	c := client.New(cfg.Socket, stderr)

	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "corral: ls: %v\n", err)
		return exitError
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sessions); err != nil {
			fmt.Fprintf(stderr, "corral: ls: %v\n", err)
			return exitError
		}
		return exitOK
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tATTACHED\tCWD\tUPTIME\tPID")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%d\n", s.Name, s.Status, s.Attached, s.Cwd, uptime(s), s.PID)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "corral: ls: %v\n", err)
		return exitError
	}
	return exitOK
}

// uptime renders the time since started_at (falling back to created_at if
// the session never started) as a short duration string, or "-" for a
// session with neither (unreachable in practice: created_at is always set).
func uptime(s client.SessionInfo) string {
	ts := s.StartedAt
	if ts == nil {
		ts = &s.CreatedAt
	}
	t, err := time.Parse(time.RFC3339, *ts)
	if err != nil {
		return "-"
	}
	return time.Since(t).Round(time.Second).String()
}
