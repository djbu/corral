package dataops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type GCCandidate struct {
	Path    string
	Bytes   int64
	ModTime time.Time
	Reason  string
}

type GCPlan struct {
	OrphanBytes int64
	Candidates  []GCCandidate
}

// PlanGC considers only direct child directories of stateDir/sessions whose
// names are absent from knownIDs. Symlinks fail closed and no other state tree
// is eligible.
func PlanGC(stateDir string, knownIDs map[string]struct{}, now time.Time, olderThan time.Duration, maxOrphanBytes int64) (GCPlan, error) {
	root := filepath.Join(stateDir, "sessions")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return GCPlan{}, nil
	}
	if err != nil {
		return GCPlan{}, fmt.Errorf("gc: read sessions directory: %w", err)
	}
	var orphans []GCCandidate
	for _, entry := range entries {
		if _, referenced := knownIDs[entry.Name()]; referenced {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return GCPlan{}, fmt.Errorf("gc: inspect %s: %w", entry.Name(), err)
		}
		path := filepath.Join(root, entry.Name())
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return GCPlan{}, fmt.Errorf("gc: refusing unexpected orphan entry: %s", path)
		}
		bytes, err := safeTreeSize(path)
		if err != nil {
			return GCPlan{}, err
		}
		orphans = append(orphans, GCCandidate{Path: path, Bytes: bytes, ModTime: info.ModTime()})
	}
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].ModTime.Equal(orphans[j].ModTime) {
			return orphans[i].Path < orphans[j].Path
		}
		return orphans[i].ModTime.Before(orphans[j].ModTime)
	})
	plan := GCPlan{}
	selected := make(map[string]struct{})
	for i := range orphans {
		plan.OrphanBytes += orphans[i].Bytes
		if olderThan > 0 && now.Sub(orphans[i].ModTime) >= olderThan {
			orphans[i].Reason = "age"
			plan.Candidates = append(plan.Candidates, orphans[i])
			selected[orphans[i].Path] = struct{}{}
		}
	}
	remaining := plan.OrphanBytes
	for _, candidate := range plan.Candidates {
		remaining -= candidate.Bytes
	}
	if maxOrphanBytes >= 0 {
		for i := range orphans {
			if remaining <= maxOrphanBytes {
				break
			}
			if _, ok := selected[orphans[i].Path]; ok {
				continue
			}
			orphans[i].Reason = "size"
			plan.Candidates = append(plan.Candidates, orphans[i])
			selected[orphans[i].Path] = struct{}{}
			remaining -= orphans[i].Bytes
		}
	}
	return plan, nil
}

func ApplyGC(stateDir string, candidates []GCCandidate) (int64, error) {
	root := filepath.Join(stateDir, "sessions")
	var removed int64
	for _, candidate := range candidates {
		rel, err := filepath.Rel(root, candidate.Path)
		if err != nil || rel == "." || filepath.IsAbs(rel) || filepath.Dir(rel) != "." {
			return removed, fmt.Errorf("gc: candidate escaped sessions root: %s", candidate.Path)
		}
		info, err := os.Lstat(candidate.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("gc: inspect candidate %s: %w", candidate.Path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return removed, fmt.Errorf("gc: candidate changed type: %s", candidate.Path)
		}
		if err := os.RemoveAll(candidate.Path); err != nil {
			return removed, fmt.Errorf("gc: remove %s: %w", candidate.Path, err)
		}
		removed += candidate.Bytes
	}
	return removed, nil
}

func safeTreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("gc: refusing symlink inside candidate: %s", path)
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("gc: size %s: %w", root, err)
	}
	return total, nil
}
