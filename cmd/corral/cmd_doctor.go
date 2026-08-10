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
	"github.com/djbu/corral/internal/claude/versionprobe"
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
	constraint := ""
	cwd, _ := os.Getwd()
	if sess, _, _, _, err := config.LoadSession(cwd, nil); err == nil && sess.ClaudeBin != "" {
		bin = sess.ClaudeBin
		constraint = sess.ClaudeVersion
	}

	foreign := settings.ForeignHookEvents(claudeHome)
	res := automode.Detect(context.Background(), bin)
	writeDoctorReport(stdout, foreign, res)
	writeClaudeCompatibility(stdout, bin, constraint, cwd)
	return exitOK
}

func writeClaudeCompatibility(w io.Writer, bin, constraint, cwd string) {
	fmt.Fprintln(w, "\nClaude Code compatibility:")
	observed, err := versionprobe.Probe(context.Background(), bin)
	if err != nil {
		fmt.Fprintln(w, "  could not determine Claude Code version")
		if constraint != "" {
			fmt.Fprintf(w, "  policy: %s (cannot verify)\n", constraint)
		}
		return
	}
	fmt.Fprintf(w, "  observed: %s\n", observed)
	golden := filepath.Join(cwd, "test", "contract", "testdata", "claude-golden", observed.String()+".json")
	if _, err := os.Stat(golden); err != nil {
		fmt.Fprintf(w, "  golden corpus missing: %s\n", observed)
	} else {
		fmt.Fprintf(w, "  golden corpus: %s\n", observed)
	}
	if constraint == "" {
		fmt.Fprintln(w, "  policy: none")
		return
	}
	compatible, err := versionprobe.Compatible(observed, constraint)
	if err != nil {
		fmt.Fprintf(w, "  policy invalid: %s\n", constraint)
		return
	}
	if compatible {
		fmt.Fprintf(w, "  policy: %s (compatible)\n", constraint)
	} else {
		fmt.Fprintf(w, "  drift: observed %s is outside %s\n", observed, constraint)
	}
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
