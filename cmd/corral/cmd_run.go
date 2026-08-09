package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/danielbecerra/corral/internal/api/client"
	"github.com/danielbecerra/corral/internal/config"
)

// terminalTaskStatuses is the set of task statuses that end waitForDAG's
// poll loop. "cancelled" is terminal alongside "succeeded"/"failed" — the
// budget-trip path cancels remaining tasks, and a loop that waited only for
// succeeded/failed would hang forever on an over-budget dag.
var terminalTaskStatuses = map[string]bool{
	"succeeded": true,
	"failed":    true,
	"cancelled": true,
}

// dagFile is the TOML shape accepted by `corral run --file`. It
// deliberately has NO permission_mode field (see dagFileNode) and is
// parsed by a dedicated strict decode — never routed through the config
// package or repo_allowlist — since a dag.toml may live inside a
// repo under management and is therefore repo-authored, untrusted content.
type dagFile struct {
	BudgetUSD *float64      `toml:"budget_usd"`
	Node      []dagFileNode `toml:"node"`
}

// dagFileNode is one [[node]] table in a dag.toml.
//
// CRITICAL SECURITY: this struct has NO permission_mode field, on purpose.
// permission_mode may only be set via the --permission-mode CLI flag in
// bare mode (a legitimate user-origin argv value) — never via a file that
// could be checked into, and thus supplied by, a repo under management.
// parseDagFile's md.Undecoded() check rejects a permission_mode key (or
// any other unknown key, e.g. a [notify]/[state]/[daemon]/[attach] table)
// outright rather than silently ignoring it. Do NOT add a permission_mode
// field here to "fix" a rejected file — that is the point of this type.
type dagFileNode struct {
	Name        string   `toml:"name"`
	Prompt      string   `toml:"prompt"`
	Repo        string   `toml:"repo"`
	Worktree    bool     `toml:"worktree"`
	Model       string   `toml:"model"`
	MaxAttempts int      `toml:"max_attempts"`
	BudgetUSD   *float64 `toml:"budget_usd"`
	DependsOn   []string `toml:"depends_on"`
}

// parseDagFile decodes path as a dagFile and converts it into a
// client.SubmitDagRequest, failing fast (before ever talking to the
// daemon) on any structural problem the server would also reject: unknown
// keys (the strict-decode security gate), zero nodes, an empty/duplicate
// node name, or a missing prompt/repo.
func parseDagFile(path string) (client.SubmitDagRequest, error) {
	var f dagFile
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return client.SubmitDagRequest{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return client.SubmitDagRequest{}, fmt.Errorf("%s: unknown key(s) (permission_mode may not be set from a file): %s", path, strings.Join(keys, ", "))
	}

	if len(f.Node) == 0 {
		return client.SubmitDagRequest{}, fmt.Errorf("%s: dag must have at least one [[node]]", path)
	}

	seen := make(map[string]bool, len(f.Node))
	nodes := make([]client.DagNode, 0, len(f.Node))
	var edges []client.DagEdge
	for _, n := range f.Node {
		if n.Name == "" {
			return client.SubmitDagRequest{}, fmt.Errorf("%s: node name must not be empty", path)
		}
		if seen[n.Name] {
			return client.SubmitDagRequest{}, fmt.Errorf("%s: duplicate node name %q", path, n.Name)
		}
		seen[n.Name] = true
		if n.Prompt == "" {
			return client.SubmitDagRequest{}, fmt.Errorf("%s: node %q: prompt must not be empty", path, n.Name)
		}
		if n.Repo == "" {
			return client.SubmitDagRequest{}, fmt.Errorf("%s: node %q: repo must not be empty", path, n.Name)
		}

		nodes = append(nodes, client.DagNode{
			Name:           n.Name,
			Prompt:         n.Prompt,
			Repo:           n.Repo,
			Worktree:       n.Worktree,
			Model:          n.Model,
			PermissionMode: "",
			MaxAttempts:    n.MaxAttempts,
			BudgetUSD:      n.BudgetUSD,
		})
		for _, dep := range n.DependsOn {
			edges = append(edges, client.DagEdge{Task: n.Name, DependsOn: dep})
		}
	}

	return client.SubmitDagRequest{BudgetUSD: f.BudgetUSD, Nodes: nodes, Edges: edges}, nil
}

// cmdRun implements `corral run` (m4.md §13 step 24) in its two
// mutually-exclusive modes:
//
//	corral run "<prompt>" --repo <path> [--worktree] [--model M] [--name N]
//	           [--budget-usd X] [--max-attempts K] [--permission-mode MODE] [--detach]
//	corral run --file dag.toml [--detach]
//
// Both modes submit one SubmitDagRequest and then, unless --detach, block
// polling the dag until every task reaches a terminal status.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repo path (bare mode)")
	worktree := fs.Bool("worktree", false, "run in an isolated git worktree (bare mode)")
	model := fs.String("model", "", "claude --model override (bare mode)")
	name := fs.String("name", "", `task name (bare mode, default "task")`)
	budgetUSD := fs.Float64("budget-usd", 0, "dag budget cap in USD")
	maxAttempts := fs.Int("max-attempts", 0, "max retry attempts (bare mode)")
	permissionMode := fs.String("permission-mode", "", "claude permission mode override (bare mode only — the sole channel by which permission_mode may be set)")
	detach := fs.Bool("detach", false, "submit and return immediately without waiting for completion")
	file := fs.String("file", "", "path to a dag.toml describing a multi-node dag")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	var req client.SubmitDagRequest

	if *file != "" {
		if fs.NArg() > 0 {
			fmt.Fprintln(stderr, "corral: run: a positional prompt and --file are mutually exclusive")
			return exitUsage
		}
		r, err := parseDagFile(*file)
		if err != nil {
			fmt.Fprintf(stderr, "corral: run: %v\n", err)
			return exitUsage
		}
		req = r
	} else {
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, `usage: corral run "<prompt>" --repo <path> [flags]`)
			fmt.Fprintln(stderr, "       corral run --file dag.toml [--detach]")
			return exitUsage
		}
		if *repo == "" {
			fmt.Fprintln(stderr, "corral: run: --repo is required")
			return exitUsage
		}
		taskName := *name
		if taskName == "" {
			taskName = "task"
		}

		// The top-level dag budget is only set if --budget-usd was
		// actually passed on argv — fs.Visit only calls back for flags
		// the user set, so an unset flag correctly yields a nil
		// *float64 (an unbounded dag) rather than a spurious $0 cap.
		var dagBudget *float64
		fs.Visit(func(fl *flag.Flag) {
			if fl.Name == "budget-usd" {
				v := *budgetUSD
				dagBudget = &v
			}
		})

		req = client.SubmitDagRequest{
			BudgetUSD: dagBudget,
			Nodes: []client.DagNode{{
				Name:           taskName,
				Prompt:         fs.Arg(0),
				Repo:           *repo,
				Worktree:       *worktree,
				Model:          *model,
				PermissionMode: *permissionMode,
				MaxAttempts:    *maxAttempts,
				BudgetUSD:      nil,
			}},
		}
	}

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: run: %v\n", err)
		return exitError
	}
	c := client.New(cfg.Socket, stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	detail, err := c.SubmitDAG(ctx, req)
	if err != nil {
		fmt.Fprintf(stderr, "corral: run: %v\n", err)
		return exitError
	}

	if *detach {
		fmt.Fprintln(stdout, detail.DAGID)
		return exitOK
	}

	return waitForDAG(ctx, c, stdout, stderr, detail.DAGID, detail)
}

// waitForDAG polls GET /v1/dags/{id} once a second (CLI-side wall-clock
// display only — never persisted, so this is exempt from the daemon-side
// no-time.Now()-ish rule) until every task in the dag reaches a terminal
// status (terminalTaskStatuses), printing each task's status the first
// time it's observed to change. initial is the SubmitDAG response, used as
// the first observation so the loop doesn't issue a redundant GetDAG at
// t=0. Honors ctx cancellation (SIGINT) by returning exitError immediately.
//
// Returns exitError if any task ended failed or cancelled, else exitOK.
func waitForDAG(ctx context.Context, c *client.Client, stdout, stderr io.Writer, dagID string, initial client.DagDetail) int {
	lastStatus := make(map[string]string, len(initial.Tasks))
	printChanges := func(d client.DagDetail) {
		for _, t := range d.Tasks {
			if lastStatus[t.ID] != t.Status {
				fmt.Fprintf(stdout, "%s: %s\n", t.Name, t.Status)
				lastStatus[t.ID] = t.Status
			}
		}
	}
	allTerminal := func(d client.DagDetail) bool {
		if len(d.Tasks) == 0 {
			return false
		}
		for _, t := range d.Tasks {
			if !terminalTaskStatuses[t.Status] {
				return false
			}
		}
		return true
	}

	detail := initial
	printChanges(detail)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for !allTerminal(detail) {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "corral: run: interrupted")
			return exitError
		case <-ticker.C:
			d, err := c.GetDAG(ctx, dagID)
			if err != nil {
				fmt.Fprintf(stderr, "corral: run: %v\n", err)
				return exitError
			}
			detail = d
			printChanges(detail)
		}
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tCOST")
	failedOrCancelled := false
	for _, t := range detail.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t$%.4f\n", t.Name, t.Status, t.CostUSD)
		if t.Status == "failed" || t.Status == "cancelled" {
			failedOrCancelled = true
		}
	}
	tw.Flush()
	fmt.Fprintf(stdout, "dag %s total cost: $%.4f\n", dagID, detail.CostUSD)

	if failedOrCancelled {
		return exitError
	}
	return exitOK
}
