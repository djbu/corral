package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CanonicalPath returns an absolute, cleaned physical path. The path must
// exist: callers mining historical sessions use that failure to skip stale
// cwd rows instead of assigning their evidence to a guessed repository.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("git: resolving absolute path %s: %w", path, err)
	}
	physical, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("git: resolving physical path %s: %w", abs, err)
	}
	return filepath.Clean(physical), nil
}

// ResolveRepo scopes a cwd to its canonical git toplevel. A real directory
// outside git scopes to itself. Git is invoked directly with argv, never via
// a shell.
func ResolveRepo(ctx context.Context, cwd string) (string, error) {
	physicalCWD, err := CanonicalPath(cwd)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", physicalCWD, "rev-parse", "--show-toplevel")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		root := strings.TrimSpace(string(out))
		if root == "" {
			return "", fmt.Errorf("git: empty toplevel for %s", physicalCWD)
		}
		return CanonicalPath(root)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// rev-parse exits non-zero for an ordinary non-repository directory.
		return physicalCWD, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("git: executable unavailable: %w", err)
	}
	return "", fmt.Errorf("git: resolving repository for %s: %w (%s)", physicalCWD, err, strings.TrimSpace(stderr.String()))
}
