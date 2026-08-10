package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/review"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/version"
)

func reviewGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func reviewHTTPFixture(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0o755)
	reviewGit(t, repo, "init", "-q", "-b", "main")
	reviewGit(t, repo, "config", "user.email", "test@example.com")
	reviewGit(t, repo, "config", "user.name", "corral test")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644)
	reviewGit(t, repo, "add", "a.txt")
	reviewGit(t, repo, "commit", "-q", "-m", "base")
	base := reviewGit(t, repo, "rev-parse", "HEAD")
	wt := filepath.Join(root, "wt")
	branch := "corral/task/http-a1"
	if err := corralgit.AddWorktreeAt(context.Background(), repo, wt, branch, base); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("change\n"), 0o644)
	reviewGit(t, wt, "add", "b.txt")
	reviewGit(t, wt, "commit", "-q", "-m", "change")
	st := openMetaTestStore(t)
	task, err := st.CreateTask(context.Background(), store.CreateTaskParams{ID: "task-http", DAGID: "dag-http", Name: "http", Prompt: "p", Repo: repo, Cwd: wt, Worktree: wt, Branch: branch, Status: store.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateTask(context.Background(), task.ID, func(x *store.Task) { x.BaseCommit = base })
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskSucceeded(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	srv := New()
	srv.RegisterDags(DagsDeps{Store: st, Review: review.New(st)})
	return srv, st, task.ID
}

func TestReviewHTTPPreflightDiffAndBranchRelease(t *testing.T) {
	srv, _, taskID := reviewHTTPFixture(t)
	pre := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/tasks/"+taskID+"/review/preflight?strategy=branch", nil)
	if pre.Code != http.StatusOK {
		t.Fatalf("preflight=%d %s", pre.Code, pre.Body.String())
	}
	var p review.Preflight
	if err := json.Unmarshal(pre.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !p.CanApply || p.TaskHead == "" {
		t.Fatalf("preflight=%+v", p)
	}
	diff := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/tasks/"+taskID+"/review/diff?full=1", nil)
	if diff.Code != http.StatusOK || !strings.Contains(diff.Body.String(), "b.txt") {
		t.Fatalf("diff=%d %s", diff.Code, diff.Body.String())
	}
	body := mustMarshal(t, review.ReleaseRequest{Strategy: review.StrategyBranch})
	released := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tasks/"+taskID+"/review/release", body)
	if released.Code != http.StatusOK {
		t.Fatalf("release=%d %s", released.Code, released.Body.String())
	}
}

func TestReviewMutationRejectsSessionScopedTokenBeforeBody(t *testing.T) {
	srv, _, taskID := reviewHTTPFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/review/discard", strings.NewReader(`{}`))
	req.Header.Set("Corral-Api-Version", fmtInt(version.APIVersion))
	req = req.WithContext(contextWithToken(req.Context(), store.TokenRow{ID: "scoped", Scope: "session", SessionID: "none"}))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func fmtInt(v int) string { return strconv.Itoa(v) }
