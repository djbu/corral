package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/djbu/corral/internal/api/client"
	"github.com/djbu/corral/internal/config"
)

// cmdReview implements inspection plus M8's explicit release/discard verbs.
func cmdReview(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "preflight":
			return cmdReviewPreflight(args[1:], stdout, stderr)
		case "release":
			return cmdReviewRelease(args[1:], stdout, stderr)
		case "discard":
			return cmdReviewDiscard(args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	diff := fs.Bool("diff", false, "print the full diff instead of a --stat summary")
	var dagID string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dagID = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if dagID == "" && fs.NArg() == 1 {
		dagID = fs.Arg(0)
	}
	if fs.NArg() > 1 || (dagID != "" && fs.NArg() > 0 && fs.Arg(0) != dagID) {
		fmt.Fprintln(stderr, "usage: corral review [<dag-id>] [--diff]")
		return exitUsage
	}

	// No --host here: review runs local `git` against daemon-side worktrees,
	// which only exist (and are only reachable by that name) on the daemon's
	// own filesystem.
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: review: %v\n", err)
		return exitError
	}
	c := client.New(cfg.Socket, stderr)
	ctx := context.Background()

	if dagID == "" {
		return reviewList(ctx, c, stdout, stderr)
	}
	return reviewDetail(ctx, c, stdout, stderr, dagID, *diff)
}

func cmdReviewPreflight(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: corral review preflight <task-id> --strategy merge|cherry-pick|branch [--target BRANCH] [--json]")
		return exitUsage
	}
	taskID, args := args[0], args[1:]
	fs := flag.NewFlagSet("review preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strategy := fs.String("strategy", "branch", "review strategy")
	target := fs.String("target", "", "target branch")
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	if *strategy != "merge" && *strategy != "cherry-pick" && *strategy != "branch" {
		fmt.Fprintln(stderr, "corral: review preflight: invalid strategy")
		return exitUsage
	}
	if *strategy != "branch" && *target == "" {
		fmt.Fprintln(stderr, "corral: review preflight: --target is required")
		return exitUsage
	}
	c, err := reviewClient(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: review preflight: %v\n", err)
		return exitError
	}
	p, err := c.ReviewPreflight(context.Background(), taskID, *strategy, *target)
	if err != nil {
		fmt.Fprintf(stderr, "corral: review preflight: %v\n", err)
		return exitError
	}
	if *asJSON {
		b, _ := json.Marshal(p)
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintf(stdout, "task: %s\nrepo: %s\nworktree: %s\nbranch: %s\nbase: %s\ntask HEAD: %s\n", p.TaskID, p.Repo, p.Worktree, p.Branch, p.BaseCommit, p.TaskHead)
		if p.TargetHead != "" {
			fmt.Fprintf(stdout, "target %s: %s\n", p.TargetRef, p.TargetHead)
		}
		for _, b := range p.Blockers {
			fmt.Fprintf(stdout, "blocked: %s\n", b)
		}
		for _, w := range p.Warnings {
			fmt.Fprintf(stdout, "warning: %s\n", w)
		}
		fmt.Fprintf(stdout, "can apply: %t\n", p.CanApply)
	}
	if !p.CanApply {
		return exitError
	}
	return exitOK
}

func reviewClient(stderr io.Writer) (*client.Client, error) {
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		return nil, err
	}
	return client.New(cfg.Socket, stderr), nil
}

func cmdReviewRelease(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: corral review release <task-id> --strategy merge|cherry-pick|branch [--target BRANCH] [--expect SHA]")
		return exitUsage
	}
	taskID, args := args[0], args[1:]
	fs := flag.NewFlagSet("review release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strategy := fs.String("strategy", "", "release strategy")
	target := fs.String("target", "", "target branch")
	expect := fs.String("expect", "", "expected target HEAD from preflight")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	if *strategy != "merge" && *strategy != "cherry-pick" && *strategy != "branch" {
		fmt.Fprintln(stderr, "corral: review release: --strategy must be merge, cherry-pick, or branch")
		return exitUsage
	}
	if *strategy != "branch" && (*target == "" || *expect == "") {
		fmt.Fprintln(stderr, "corral: review release: --target and --expect are required")
		return exitUsage
	}
	c, err := reviewClient(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: review release: %v\n", err)
		return exitError
	}
	result, err := c.ReleaseReview(context.Background(), taskID, client.ReleaseReviewRequest{Strategy: *strategy, Target: *target, ExpectedTargetHead: *expect})
	if err != nil {
		fmt.Fprintf(stderr, "corral: review release: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "released %s via %s", taskID, *strategy)
	if result.Review != nil && result.Review.ResultCommit != "" {
		fmt.Fprintf(stdout, " at %s", result.Review.ResultCommit)
	}
	fmt.Fprintln(stdout)
	return exitOK
}

func cmdReviewDiscard(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: corral review discard <task-id> --expect SHA [--force]")
		return exitUsage
	}
	taskID, args := args[0], args[1:]
	fs := flag.NewFlagSet("review discard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	expect := fs.String("expect", "", "expected task HEAD")
	force := fs.Bool("force", false, "discard uncommitted work after exact identity check")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	if *expect == "" {
		fmt.Fprintln(stderr, "corral: review discard: --expect is required")
		return exitUsage
	}
	c, err := reviewClient(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: review discard: %v\n", err)
		return exitError
	}
	p, err := c.ReviewPreflight(context.Background(), taskID, "branch", "")
	if err != nil {
		fmt.Fprintf(stderr, "corral: review discard: %v\n", err)
		return exitError
	}
	result, err := c.DiscardReview(context.Background(), taskID, client.DiscardReviewRequest{ExpectedTaskHead: *expect, ExpectedRepo: p.Repo, ExpectedWorktree: p.Worktree, ExpectedBranch: p.Branch, Force: *force})
	if err != nil {
		fmt.Fprintf(stderr, "corral: review discard: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "discarded %s", taskID)
	if result.Review != nil {
		fmt.Fprintf(stdout, "; recovery ref %s", result.Review.RecoveryRef)
	}
	fmt.Fprintln(stdout)
	return exitOK
}

// reviewList renders `corral review` with no argument: every submitted
// dag's denormalized budget/cost rollup.
func reviewList(ctx context.Context, c *client.Client, stdout, stderr io.Writer) int {
	dags, err := c.ListDAGs(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "corral: review: %v\n", err)
		return exitError
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DAG\tBUDGET\tCOST")
	for _, d := range dags {
		fmt.Fprintf(tw, "%s\t%s\t$%.4f\n", d.DAGID, budgetColumn(d.BudgetUSD), d.CostUSD)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "corral: review: %v\n", err)
		return exitError
	}
	return exitOK
}

// reviewDetail renders `corral review <dag-id>`: a per-task table, the dag
// total cost, and — for every task that ran in a worktree — a diff summary
// against that task's fork point (worktreeDiff).
func reviewDetail(ctx context.Context, c *client.Client, stdout, stderr io.Writer, dagID string, full bool) int {
	detail, err := c.GetDAG(ctx, dagID)
	if err != nil {
		fmt.Fprintf(stderr, "corral: review: %v\n", err)
		return exitError
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tREVIEW\tCOST\tBRANCH\tWORKTREE")
	for _, t := range detail.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t$%.4f\t%s\t%s\n", t.ID, t.Name, t.Status, t.ReviewStatus, t.CostUSD, t.Branch, t.Worktree)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "corral: review: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "dag %s total cost: $%.4f\n", detail.DAGID, detail.CostUSD)

	for _, t := range detail.Tasks {
		if t.Worktree == "" {
			continue
		}
		if !worktreeReady(t.Worktree) {
			fmt.Fprintf(stdout, "\n%s: (worktree not yet created)\n", t.Name)
			continue
		}
		fmt.Fprintf(stdout, "\n%s (%s):\n", t.Name, t.Worktree)
		out, err := worktreeDiff(ctx, t.Repo, t.Worktree, full)
		if err != nil {
			fmt.Fprintf(stdout, "  (diff unavailable: %v)\n", err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			fmt.Fprintln(stdout, "  (no changes)")
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
	}

	return exitOK
}

// budgetColumn renders a dag's optional budget cap, "-" when unbounded.
func budgetColumn(budgetUSD *float64) string {
	if budgetUSD == nil {
		return "-"
	}
	return fmt.Sprintf("$%.4f", *budgetUSD)
}

// worktreeReady reports whether a task's Worktree field is a real on-disk
// path rather than "requested" — handlers_dags.go's pre-launch sentinel
// (§13 step 24 handleCreate) recorded when a worktree was asked for at
// submission time but the orchestrator hasn't yet turned that into a real
// path (task still queued or running its first attempt). Feeding the
// literal string "requested" to `git -C requested ...` would otherwise
// fail with a "cannot change to 'requested'" error on every queued task.
func worktreeReady(worktree string) bool {
	return worktree != "" && worktree != "requested"
}

// worktreeDiff computes and returns a task's diff against its fork point
// from repo, NOT a bare `git -C worktree diff`.
//
// This distinction is load-bearing: agents commit their work on the task
// branch, so a plain `git -C <worktree> diff` (working tree vs branch HEAD)
// prints nothing for already-committed work — exactly the case review
// exists to surface. Instead:
//
//  1. repoHEAD  = `git -C repo rev-parse HEAD`
//  2. base      = `git -C worktree merge-base HEAD <repoHEAD>`
//  3. output    = `git -C worktree diff [--stat] base`
//
// full selects a complete diff; otherwise only a --stat summary is
// produced. All git invocations go through exec.CommandContext with an
// argv slice — never a shell, never string-interpolated — matching
// internal/git's stance (repo/worktree here are daemon-recorded task
// fields, not attacker-controlled shell text, but the no-shell rule holds
// regardless).
func worktreeDiff(ctx context.Context, repo, worktree string, full bool) (string, error) {
	repoHEAD, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD in %s: %w", repo, err)
	}
	repoHEAD = strings.TrimSpace(repoHEAD)

	base, err := runGit(ctx, worktree, "merge-base", "HEAD", repoHEAD)
	if err != nil {
		return "", fmt.Errorf("merge-base in %s: %w", worktree, err)
	}
	base = strings.TrimSpace(base)

	diffArgs := []string{"diff"}
	if !full {
		diffArgs = append(diffArgs, "--stat")
	}
	diffArgs = append(diffArgs, base)

	out, err := runGit(ctx, worktree, diffArgs...)
	if err != nil {
		return "", fmt.Errorf("diff in %s: %w", worktree, err)
	}
	return out, nil
}

// runGit execs git against dir via `-C <dir>`, never through a shell,
// mirroring internal/git's run helper.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return "", fmt.Errorf("%w: %s", err, stderrText)
		}
		return "", err
	}
	return stdout.String(), nil
}
