package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/djbu/corral/internal/claude/automode"
	"github.com/djbu/corral/internal/claude/settings"
	"github.com/djbu/corral/internal/config"
)

// cmdDoctor implements `corral doctor`: a read-only local diagnostic of the
// two things a supervised session's observability depends on but corral does
// not control — foreign hooks configured outside corral, and whether Claude
// Code's Auto Mode is active (design doc Amendments A.5/A.6).
//
// Deliberately local, not a daemon endpoint: the failures doctor exists to
// diagnose (hooks not firing, daemon trouble) are exactly when the daemon may
// be unreachable, and both inputs (~/.claude/settings.json, `claude auto-mode
// config`) are local reads. doctor never mutates ~/.claude — "read-mostly
// sacred" (RUNBOOK).
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "corral: doctor: resolving home dir: %v\n", err)
		return exitError
	}
	claudeHome := filepath.Join(home, ".claude")

	// Resolve claude_bin the same way a session would (user/env config, no
	// repo layer — a cloned repo must not redirect the probed binary), falling
	// back to the "claude" default LoadSession bakes in when config fails.
	bin := "claude"
	cwd, _ := os.Getwd()
	if sess, _, _, _, err := config.LoadSession(cwd, nil); err == nil && sess.ClaudeBin != "" {
		bin = sess.ClaudeBin
	}

	foreign := settings.ForeignHookEvents(claudeHome)
	res := automode.Detect(context.Background(), bin)
	writeDoctorReport(stdout, foreign, res)
	return exitOK
}

// writeDoctorReport renders the doctor report, split from cmdDoctor's
// (env-dependent) gathering so the branch-heavy formatting — foreign hooks
// none/listed, Auto Mode active/unknown — is unit-testable without a home dir
// or a real claude binary.
func writeDoctorReport(w io.Writer, foreign []string, res automode.Result) {
	fmt.Fprintln(w, "corral doctor")

	fmt.Fprintln(w, "\nForeign hooks (user scope, ~/.claude/settings.json):")
	if len(foreign) == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		for _, ev := range foreign {
			fmt.Fprintf(w, "  - %s\n", ev)
		}
		fmt.Fprintln(w, "  These run inside supervised sessions; corral does not manage them.")
	}
	fmt.Fprintln(w, "  (project/local setting_sources not inspected)")

	fmt.Fprintln(w, "\nAuto Mode:")
	if res.Status == automode.StatusActive {
		fmt.Fprintf(w, "  %s\n", automode.ActiveMessage)
		fmt.Fprintf(w, "  policy: allow=%d soft_deny=%d hard_deny=%d\n",
			res.AllowCount, res.SoftDenyCount, res.HardDenyCount)
	} else {
		fmt.Fprintln(w, "  could not determine (claude not found, or `auto-mode config` unavailable/unrecognized)")
		if res.Raw != "" {
			fmt.Fprintf(w, "  raw: %s\n", res.Raw)
		}
	}
}
