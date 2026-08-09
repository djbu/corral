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
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	c, err := newClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: ls: %v\n", err)
		return exitError
	}

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
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%d\n", s.Name, stateColumn(s), s.Attached, s.Cwd, uptime(s), s.PID)
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

// stateColumn renders the STATE cell from agent_state with the §4.5 display
// decorations: unknown gets the "hooks not firing?" hint (the failure that
// otherwise leaves the user staring at a silent session), a working session
// past stale_after renders "working?", and blocked appends the blocked_reason
// summary so the user sees WHAT is blocked without a second command. Falls
// back to the process status when agent_state is empty (NoopEngine, or a
// session with no hook history yet).
func stateColumn(s client.SessionInfo) string {
	st := s.AgentState
	if st == "" {
		return s.Status
	}
	switch st {
	case "unknown":
		return "unknown (hooks not firing?)"
	case "working":
		if s.Stale {
			return "working?"
		}
		return "working"
	case "blocked":
		if summary := blockedSummary(s.BlockedReason); summary != "" {
			return "blocked: " + summary
		}
		return "blocked"
	default:
		return st
	}
}

// blockedSummary pulls the human-readable summary out of the raw blocked_reason
// JSON (§4.3). Best-effort: any decode error yields "" and the caller renders
// a bare "blocked".
func blockedSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return ""
	}
	return r.Summary
}
