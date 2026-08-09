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

func cmdLearnings(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printLearningsUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "scan":
		return learningsScan(args[1:], stdout, stderr)
	case "list":
		return learningsList(args[1:], stdout, stderr)
	case "show":
		return learningsShow(args[1:], stdout, stderr)
	case "report":
		return learningsReport(args[1:], stdout, stderr)
	case "adopt":
		return learningsDecision(args[1:], stdout, stderr, "adopt")
	case "reject":
		return learningsDecision(args[1:], stdout, stderr, "reject")
	case "retire":
		return learningsDecision(args[1:], stdout, stderr, "retire")
	default:
		printLearningsUsage(stderr)
		return exitUsage
	}
}

func printLearningsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: corral learnings <scan|list|show|report|adopt|reject|retire> [flags]")
	fmt.Fprintln(w, "  corral learnings scan [--repo PATH] [--json]")
	fmt.Fprintln(w, "  corral learnings list [--repo PATH] [--status STATUS] [--json]")
	fmt.Fprintln(w, "  corral learnings show <id> [--diff] [--json]")
	fmt.Fprintln(w, "  corral learnings report <id> [--json]")
	fmt.Fprintln(w, "  corral learnings adopt <id> [--json]")
	fmt.Fprintln(w, "  corral learnings reject <id> [--reason TEXT] [--json]")
	fmt.Fprintln(w, "  corral learnings retire <id> [--json]")
}

func learningsScan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("learnings scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repository path (default: all observed repositories)")
	jsonOut := fs.Bool("json", false, "print stable JSON")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	c, err := newClient(cf, stderr)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	result, err := c.ScanLearnings(context.Background(), *repo)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	if *jsonOut {
		return printLearningJSON(stdout, stderr, result)
	}
	fmt.Fprintf(stdout, "window %s .. %s; sessions=%d eligible=%d skipped_cwds=%d proposals=%d\n",
		time.UnixMilli(result.WindowStartMs).Format(time.RFC3339),
		time.UnixMilli(result.WindowEndMs).Format(time.RFC3339),
		result.Sessions, result.Eligible, result.SkippedCWDs, len(result.Learnings))
	for _, l := range result.Learnings {
		fmt.Fprintf(stdout, "%s\t%s\t%s\tevidence=%d\t%s\n", l.ID, l.Status, learningRule(l), l.EvidenceCount, l.Repo)
	}
	return exitOK
}

func learningsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("learnings list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repository path")
	status := fs.String("status", "", "filter by status")
	jsonOut := fs.Bool("json", false, "print stable JSON")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	c, err := newClient(cf, stderr)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	rows, err := c.ListLearnings(context.Background(), *repo, *status)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	if *jsonOut {
		return printLearningJSON(stdout, stderr, struct {
			Learnings []client.LearningInfo `json:"learnings"`
		}{Learnings: rows})
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tEVIDENCE\tRULE\tREPOSITORY\tEXPIRES")
	for _, l := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", l.ID, l.Status, l.EvidenceCount,
			learningRule(l), l.Repo, time.UnixMilli(l.ExpiresMs).Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return learningCLIError(stderr, err)
	}
	return exitOK
}

func learningsShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("learnings show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	diff := fs.Bool("diff", false, "print verified settings before and after")
	jsonOut := fs.Bool("json", false, "print stable JSON")
	cf := addClientFlags(fs)
	if err := fs.Parse(interspersedFlagArgs(args, "host")); err != nil || fs.NArg() != 1 {
		return exitUsage
	}
	c, err := newClient(cf, stderr)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	l, err := c.GetLearning(context.Background(), fs.Arg(0))
	if err != nil {
		return learningCLIError(stderr, err)
	}
	if *jsonOut {
		return printLearningJSON(stdout, stderr, l)
	}
	fmt.Fprintf(stdout, "id: %s\nstatus: %s\nrepository: %s\nrule: %s\nevidence: %d\nexpires: %s\n",
		l.ID, l.Status, l.Repo, learningRule(l), l.EvidenceCount,
		time.UnixMilli(l.ExpiresMs).Format(time.RFC3339))
	if *diff {
		var verification struct {
			Before json.RawMessage `json:"before_settings_json"`
			After  json.RawMessage `json:"after_settings_json"`
		}
		if json.Unmarshal(l.Verification, &verification) != nil || len(verification.After) == 0 {
			fmt.Fprintln(stderr, "corral: learnings: no verified settings diff available")
			return exitError
		}
		fmt.Fprintf(stdout, "--- before settings.json\n%s\n+++ after settings.json\n%s\n", verification.Before, verification.After)
	}
	return exitOK
}

func learningsReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("learnings report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print stable JSON")
	cf := addClientFlags(fs)
	if err := fs.Parse(interspersedFlagArgs(args, "host")); err != nil || fs.NArg() != 1 {
		return exitUsage
	}
	c, err := newClient(cf, stderr)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	report, err := c.GetLearningReport(context.Background(), fs.Arg(0))
	if err != nil {
		return learningCLIError(stderr, err)
	}
	if *jsonOut {
		return printLearningJSON(stdout, stderr, report)
	}
	fmt.Fprintf(stdout, "%s %s %s\n", report.Learning.ID, report.Learning.Status, learningRule(report.Learning))
	if len(report.Measurements) == 0 {
		fmt.Fprintln(stdout, "measurements: none")
		return exitOK
	}
	for _, m := range report.Measurements {
		fmt.Fprintf(stdout, "%s %s %s..%s %s\n", m.Phase, m.Verdict,
			time.UnixMilli(m.WindowStartMs).Format(time.RFC3339),
			time.UnixMilli(m.WindowEndMs).Format(time.RFC3339), m.Metrics)
	}
	return exitOK
}

func learningsDecision(args []string, stdout, stderr io.Writer, action string) int {
	fs := flag.NewFlagSet("learnings "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "", "operator reason (reject only; secrets are redacted)")
	jsonOut := fs.Bool("json", false, "print stable JSON")
	cf := addClientFlags(fs)
	if err := fs.Parse(interspersedFlagArgs(args, "reason", "host")); err != nil || fs.NArg() != 1 || (action != "reject" && *reason != "") {
		return exitUsage
	}
	c, err := newClient(cf, stderr)
	if err != nil {
		return learningCLIError(stderr, err)
	}
	var updated client.LearningInfo
	switch action {
	case "adopt":
		updated, err = c.AdoptLearning(context.Background(), fs.Arg(0))
	case "reject":
		updated, err = c.RejectLearning(context.Background(), fs.Arg(0), *reason)
	case "retire":
		updated, err = c.RetireLearning(context.Background(), fs.Arg(0))
	default:
		return exitUsage
	}
	if err != nil {
		return learningCLIError(stderr, err)
	}
	if *jsonOut {
		return printLearningJSON(stdout, stderr, updated)
	}
	fmt.Fprintf(stdout, "%s %s %s\n", updated.ID, updated.Status, learningRule(updated))
	return exitOK
}

func learningRule(l client.LearningInfo) string {
	var content struct {
		Rule string `json:"rule"`
	}
	if json.Unmarshal(l.Content, &content) != nil || content.Rule == "" {
		return "-"
	}
	return content.Rule
}

func printLearningJSON(stdout, stderr io.Writer, value any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return learningCLIError(stderr, err)
	}
	return exitOK
}

func learningCLIError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "corral: learnings: %v\n", err)
	return exitError
}
