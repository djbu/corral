// Package versionprobe reads the version of an operator-selected Claude Code
// binary and evaluates the deliberately small semver constraint language used
// by repository compatibility policy. It never resolves or executes a value
// originating in a repository.
package versionprobe

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var semverRE = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func parse(raw string) (Version, error) {
	m := semverRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("no semantic version in %q", strings.TrimSpace(raw))
	}
	parts := [3]int{}
	for i := range parts {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, err
		}
		parts[i] = n
	}
	return Version{parts[0], parts[1], parts[2]}, nil
}

// Probe executes only bin (which callers must obtain from operator config),
// with a short timeout. Raw output is intentionally not persisted.
func Probe(ctx context.Context, bin string) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return Version{}, fmt.Errorf("claude --version: %w", err)
	}
	v, err := parse(string(out))
	if err != nil {
		return Version{}, fmt.Errorf("claude --version output: %w", err)
	}
	return v, nil
}

func compare(a, b Version) int {
	if a.Major != b.Major {
		if a.Major < b.Major {
			return -1
		}
		return 1
	}
	if a.Minor != b.Minor {
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	}
	if a.Patch < b.Patch {
		return -1
	}
	if a.Patch > b.Patch {
		return 1
	}
	return 0
}

// Compatible accepts an empty constraint, an exact version, or whitespace
// separated comparators such as ">=2.1.0 <2.2.0". This intentionally avoids
// a broad package-manager grammar: policy remains easy to audit in a repo.
func Compatible(observed Version, constraint string) (bool, error) {
	for _, term := range strings.Fields(constraint) {
		op := "="
		for _, candidate := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(term, candidate) {
				op, term = candidate, strings.TrimPrefix(term, candidate)
				break
			}
		}
		want, err := parse(term)
		if err != nil {
			return false, fmt.Errorf("invalid Claude compatibility term: %w", err)
		}
		cmp := compare(observed, want)
		ok := map[string]bool{"=": cmp == 0, ">": cmp > 0, ">=": cmp >= 0, "<": cmp < 0, "<=": cmp <= 0}[op]
		if !ok {
			return false, nil
		}
	}
	return true, nil
}
