package dataops

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanAndApplyGCOnlyOrphans(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "sessions")
	now := time.Unix(2_000_000, 0)
	for name, size := range map[string]int{"referenced": 10, "old": 20, "large": 30} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "output.log"), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "old"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanGC(stateDir, map[string]struct{}{"referenced": {}}, now, 30*24*time.Hour, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want old by age and large by size", plan.Candidates)
	}
	if _, err := os.Stat(filepath.Join(root, "old")); err != nil {
		t.Fatalf("dry plan mutated old: %v", err)
	}
	removed, err := ApplyGC(stateDir, plan.Candidates)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 50 {
		t.Fatalf("removed bytes = %d, want 50", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "referenced")); err != nil {
		t.Fatalf("referenced directory removed: %v", err)
	}
}

func TestPlanGCRejectsSymlink(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "orphan")); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanGC(stateDir, nil, time.Now(), time.Hour, 0); err == nil {
		t.Fatal("PlanGC accepted symlink candidate")
	}
}
