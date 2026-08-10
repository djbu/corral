// Package git is corral's only git-handling code (design doc §6): a thin
// helper over `git worktree` for per-task isolation ("branch-per-task",
// §6.1). It is deliberately narrow — add/remove/list, nothing else — and
// invokes git exactly like internal/answer treats PTY input: as opaque
// data, never shell-interpolated. All arguments are passed to
// exec.CommandContext as a []string slice; there is no shell in the
// invocation path, so a task name or path containing shell metacharacters
// cannot escape into command injection.
//
// Callers (the M4 orchestrator) construct repo/path/branch themselves —
// they are daemon-constructed strings, never derived from files inside the
// repo under management (§6.1) — so this package does not attempt to
// sanitize them beyond what git itself rejects.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Worktree is one record from `git worktree list --porcelain`, one field
// per porcelain line this package understands (§6's parse contract):
// "worktree <path>", "HEAD <sha>", "branch refs/heads/<name>", "bare",
// "detached". Unrecognized lines within a record are ignored (tolerant
// parse, matching the streamjson package's stance on unknown fields).
type Worktree struct {
	Path     string
	Branch   string
	HEAD     string
	Bare     bool
	Detached bool
}

// AddWorktree creates a new worktree checked out to a new branch, running:
//
//	git -C <repo> worktree add -b <branch> <path> HEAD
//
// It fails — and returns a wrapped error including git's stderr — if path
// or branch already exists; git itself enforces that, this function does
// not pre-check (§6.1: "AddWorktree fails if path/branch exists"). Callers
// on collision retry with a new branch name (e.g. the next attempt's
// `-a<N>` suffix, §6.1) rather than expecting this call to be idempotent.
func AddWorktree(ctx context.Context, repo, path, branch string) error {
	return AddWorktreeAt(ctx, repo, path, branch, "HEAD")
}

// AddWorktreeAt creates branch at an already-resolved commit. M8 uses this
// to persist and check the exact fork point rather than later guessing it
// from a moving target branch.
func AddWorktreeAt(ctx context.Context, repo, path, branch, commit string) error {
	_, err := run(ctx, repo, "worktree", "add", "-b", branch, path, commit)
	if err != nil {
		return fmt.Errorf("git: add worktree %s (branch %s) in %s: %w", path, branch, repo, err)
	}
	return nil
}

// RevParse resolves rev to an immutable object id in repo.
func RevParse(ctx context.Context, repo, rev string) (string, error) {
	out, err := run(ctx, repo, "rev-parse", "--verify", rev)
	if err != nil {
		return "", fmt.Errorf("git: resolving %s in %s: %w", rev, repo, err)
	}
	return strings.TrimSpace(out), nil
}

// RemoveWorktree deletes a worktree, running:
//
//	git -C <repo> worktree remove --force <path>
//
// --force discards any uncommitted changes in the worktree rather than
// refusing (design doc §6.1's lifecycle: corral, not the human, owns
// removal timing — a failed task's worktree is retained for inspection and
// never reaches this call until the review gate releases or discards it,
// §10).
func RemoveWorktree(ctx context.Context, repo, path string) error {
	_, err := run(ctx, repo, "worktree", "remove", "--force", path)
	if err != nil {
		return fmt.Errorf("git: remove worktree %s in %s: %w", path, repo, err)
	}
	return nil
}

// ListWorktrees runs `git -C <repo> worktree list --porcelain` and parses
// the result into one Worktree per record. Porcelain records are separated
// by a blank line; this function is tolerant of trailing whitespace and a
// missing final blank line (git omits the trailing separator after the
// last record).
func ListWorktrees(ctx context.Context, repo string) ([]Worktree, error) {
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git: list worktrees in %s: %w", repo, err)
	}
	return parsePorcelain(out), nil
}

// parsePorcelain decodes `git worktree list --porcelain` output. Each
// record is a run of lines terminated by a blank line (or end of input);
// within a record, lines are one of:
//
//	worktree <path>
//	HEAD <sha>
//	branch refs/heads/<name>
//	bare
//	detached
//
// Unknown lines are ignored rather than erroring, matching the tolerant
// parse stance elsewhere in this milestone (streamjson §4.3): a future git
// version adding a new porcelain line must not break this parser.
func parsePorcelain(out string) []Worktree {
	var worktrees []Worktree
	cur := Worktree{}
	have := false // true once cur has seen at least one "worktree" line

	flush := func() {
		if have {
			worktrees = append(worktrees, cur)
		}
		cur = Worktree{}
		have = false
	}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		switch {
		case line == "bare":
			cur.Bare = true
		case line == "detached":
			cur.Detached = true
		case strings.HasPrefix(line, "worktree "):
			cur.Path = strings.TrimPrefix(line, "worktree ")
			have = true
		case strings.HasPrefix(line, "HEAD "):
			cur.HEAD = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()

	return worktrees
}

// run execs git with args against repo via `-C <repo>`, never through a
// shell (exec.CommandContext takes args as a []string — there is no
// string-interpolated command line for a hostile path/branch to escape
// out of). It captures stdout and stderr separately, returning stdout on
// success and folding stderr into the error on failure so callers get
// git's actual diagnostic rather than just an exit code.
func run(ctx context.Context, repo string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repo}, args...)
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
