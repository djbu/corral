package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func OperationInProgress(ctx context.Context, repo string) (bool, error) {
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		out, err := run(ctx, repo, "rev-parse", "--git-path", name)
		if err != nil {
			return false, err
		}
		path := strings.TrimSpace(out)
		if !filepath.IsAbs(path) {
			path = filepath.Join(repo, path)
		}
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func StatusPorcelain(ctx context.Context, repo string) (string, error) {
	return run(ctx, repo, "status", "--porcelain=v1", "--untracked-files=all")
}

func CurrentBranch(ctx context.Context, repo string) (string, error) {
	out, err := run(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git: current branch in %s: %w", repo, err)
	}
	return strings.TrimSpace(out), nil
}

func IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	_, err := run(ctx, repo, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	// git's diagnostic is normally empty for the ordinary false result. A
	// malformed/missing object produces text and remains an error.
	if strings.Contains(err.Error(), "exit status 1") && !strings.Contains(err.Error(), "fatal:") {
		return false, nil
	}
	return false, err
}

func RevList(ctx context.Context, repo, rangeExpr string) ([]string, error) {
	out, err := run(ctx, repo, "rev-list", "--reverse", rangeExpr)
	if err != nil {
		return nil, fmt.Errorf("git: listing commits %s: %w", rangeExpr, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Fields(out), nil
}

func Diff(ctx context.Context, repo, base, head string, full bool) (string, error) {
	args := []string{"diff"}
	if !full {
		args = append(args, "--stat")
	}
	args = append(args, base, head)
	out, err := run(ctx, repo, args...)
	if err != nil {
		return "", fmt.Errorf("git: diff %s..%s: %w", base, head, err)
	}
	return out, nil
}

// MergeTree is the legacy, read-only three-tree form. Unlike --write-tree it
// does not create objects in the repository, so it is safe for preflight.
func MergeTree(ctx context.Context, repo, base, ours, theirs string) (string, error) {
	out, err := run(ctx, repo, "merge-tree", base, ours, theirs)
	if err != nil {
		return "", fmt.Errorf("git: merge-tree: %w", err)
	}
	return out, nil
}

func Merge(ctx context.Context, repo, commit string) error {
	_, err := run(ctx, repo, "merge", "--no-ff", "--no-edit", commit)
	if err != nil {
		return fmt.Errorf("git: merge %s: %w", commit, err)
	}
	return nil
}

func MergeAbort(ctx context.Context, repo string) { _, _ = run(ctx, repo, "merge", "--abort") }

func CherryPick(ctx context.Context, repo string, commits []string) error {
	args := append([]string{"cherry-pick"}, commits...)
	_, err := run(ctx, repo, args...)
	if err != nil {
		return fmt.Errorf("git: cherry-pick: %w", err)
	}
	return nil
}

func CherryPickAbort(ctx context.Context, repo string) {
	_, _ = run(ctx, repo, "cherry-pick", "--abort")
}

func CreateRef(ctx context.Context, repo, ref, commit string) error {
	_, err := run(ctx, repo, "update-ref", ref, commit, "0000000000000000000000000000000000000000")
	if err != nil {
		return fmt.Errorf("git: creating ref %s: %w", ref, err)
	}
	return nil
}
