package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRepoCanonicalizesRootAndSubdirectory(t *testing.T) {
	repo := newTestRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := CanonicalPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRepo(context.Background(), sub)
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveRepo = %q, want %q", got, want)
	}
}

func TestResolveRepoFallsBackOutsideGit(t *testing.T) {
	dir := t.TempDir()
	want, _ := CanonicalPath(dir)
	got, err := ResolveRepo(context.Background(), dir)
	if err != nil || got != want {
		t.Fatalf("ResolveRepo = (%q,%v), want (%q,nil)", got, err, want)
	}
}

func TestResolveRepoRejectsMissingCWD(t *testing.T) {
	if _, err := ResolveRepo(context.Background(), filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("ResolveRepo missing cwd succeeded")
	}
}
