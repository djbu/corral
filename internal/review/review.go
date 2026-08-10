// Package review owns M8's checked Git mutation boundary.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/djbu/corral/internal/clock"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
	"github.com/google/uuid"
)

type Strategy string

const (
	StrategyMerge      Strategy = "merge"
	StrategyCherryPick Strategy = "cherry-pick"
	StrategyBranch     Strategy = "branch"
)

var (
	ErrNotReviewable   = errors.New("review: task is not pending review")
	ErrPreflight       = errors.New("review: preflight blocked")
	ErrIdentityChanged = errors.New("review: git identity changed since preflight")
)

type Store interface {
	GetTask(context.Context, string) (*store.Task, error)
	GetTaskReview(context.Context, string) (*store.TaskReview, error)
	CompleteTaskReview(context.Context, string, store.ReviewStatus, string, string, string, string, string, session.EventKind, string) error
	AppendEvent(context.Context, string, session.EventKind, string) (*session.Event, error)
}

type Preflight struct {
	TaskID       string   `json:"task_id"`
	ReviewStatus string   `json:"review_status"`
	Strategy     Strategy `json:"strategy"`
	Repo         string   `json:"repo"`
	Worktree     string   `json:"worktree"`
	Branch       string   `json:"branch"`
	BaseCommit   string   `json:"base_commit"`
	TaskHead     string   `json:"task_head"`
	TargetRef    string   `json:"target_ref,omitempty"`
	TargetHead   string   `json:"target_head,omitempty"`
	Commits      []string `json:"commits"`
	TaskDirty    bool     `json:"task_dirty"`
	TargetDirty  bool     `json:"target_dirty"`
	CanApply     bool     `json:"can_apply"`
	Blockers     []string `json:"blockers"`
	Warnings     []string `json:"warnings"`
}

type ReleaseRequest struct {
	Strategy           Strategy `json:"strategy"`
	Target             string   `json:"target,omitempty"`
	ExpectedTargetHead string   `json:"expected_target_head,omitempty"`
}
type DiscardRequest struct {
	ExpectedTaskHead string `json:"expected_task_head"`
	ExpectedRepo     string `json:"expected_repo,omitempty"`
	ExpectedWorktree string `json:"expected_worktree,omitempty"`
	ExpectedBranch   string `json:"expected_branch,omitempty"`
	Force            bool   `json:"force"`
}
type Result struct {
	Review    *store.TaskReview `json:"review"`
	Preflight Preflight         `json:"preflight"`
}

type Service struct {
	st    Store
	locks sync.Map
	clk   clock.Clock
}

func New(st Store, clocks ...clock.Clock) *Service {
	clk := clock.Real()
	if len(clocks) > 0 && clocks[0] != nil {
		clk = clocks[0]
	}
	return &Service{st: st, clk: clk}
}

func validStrategy(s Strategy) bool {
	return s == StrategyMerge || s == StrategyCherryPick || s == StrategyBranch
}

func (s *Service) Preflight(ctx context.Context, taskID string, strategy Strategy, target string) (Preflight, error) {
	p := Preflight{TaskID: taskID, Strategy: strategy, Commits: []string{}, Blockers: []string{}, Warnings: []string{}}
	if !validStrategy(strategy) {
		p.Blockers = append(p.Blockers, "unknown strategy")
		return p, nil
	}
	task, err := s.st.GetTask(ctx, taskID)
	if err != nil {
		return p, err
	}
	p.Repo, p.Worktree, p.Branch, p.BaseCommit = task.Repo, task.Worktree, task.Branch, task.BaseCommit
	rev, err := s.st.GetTaskReview(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return p, ErrNotReviewable
		}
		return p, err
	}
	p.ReviewStatus = string(rev.Status)
	if rev.Status != store.ReviewPending {
		p.Blockers = append(p.Blockers, "review is not pending")
	}
	if task.Status != store.TaskSucceeded {
		p.Blockers = append(p.Blockers, "task execution did not succeed")
	}
	if task.BaseCommit == "" {
		p.Blockers = append(p.Blockers, "base commit is unavailable")
	}
	if task.Worktree == "" || task.Worktree == "requested" {
		p.Blockers = append(p.Blockers, "resolved worktree is unavailable")
	}
	if !strings.HasPrefix(task.Branch, "corral/task/") {
		p.Blockers = append(p.Blockers, "branch is not owned by corral")
	}

	repoOK, worktreeOK := false, false
	repo, err := corralgit.CanonicalPath(task.Repo)
	if err != nil {
		p.Blockers = append(p.Blockers, "repository path is unavailable")
	} else {
		repoOK = true
		p.Repo = repo
	}
	wt, err := corralgit.CanonicalPath(task.Worktree)
	if err != nil {
		p.Blockers = append(p.Blockers, "worktree path is unavailable")
	} else {
		worktreeOK = true
		p.Worktree = wt
	}

	if repoOK && worktreeOK {
		worktrees, e := corralgit.ListWorktrees(ctx, task.Repo)
		if e != nil {
			p.Blockers = append(p.Blockers, "cannot list repository worktrees")
		} else {
			found := false
			for _, w := range worktrees {
				registered, _ := corralgit.CanonicalPath(w.Path)
				if registered == p.Worktree {
					found = true
					if w.Branch != task.Branch {
						p.Blockers = append(p.Blockers, "worktree branch does not match persisted branch")
					}
					break
				}
			}
			if !found {
				p.Blockers = append(p.Blockers, "persisted worktree is not registered")
			}
		}
	}
	if worktreeOK {
		if head, e := corralgit.RevParse(ctx, task.Worktree, "HEAD"); e != nil {
			p.Blockers = append(p.Blockers, "cannot resolve task HEAD")
		} else {
			p.TaskHead = head
		}
	}
	if repoOK && p.TaskHead != "" {
		if refHead, e := corralgit.RevParse(ctx, task.Repo, "refs/heads/"+task.Branch); e != nil || refHead != p.TaskHead {
			p.Blockers = append(p.Blockers, "task branch HEAD does not match worktree HEAD")
		}
	}
	if worktreeOK {
		if dirty, e := corralgit.StatusPorcelain(ctx, task.Worktree); e != nil {
			p.Blockers = append(p.Blockers, "cannot inspect task worktree")
		} else {
			p.TaskDirty = strings.TrimSpace(dirty) != ""
			if p.TaskDirty {
				p.Blockers = append(p.Blockers, "task worktree is dirty")
			}
		}
	}
	if repoOK && p.BaseCommit != "" && p.TaskHead != "" {
		if ok, e := corralgit.IsAncestor(ctx, task.Repo, p.BaseCommit, p.TaskHead); e != nil || !ok {
			p.Blockers = append(p.Blockers, "base commit is not an ancestor of task HEAD")
		} else if commits, e := corralgit.RevList(ctx, task.Repo, p.BaseCommit+".."+p.TaskHead); e != nil {
			p.Blockers = append(p.Blockers, "cannot enumerate task commits")
		} else {
			p.Commits = commits
			if len(commits) == 0 {
				p.Warnings = append(p.Warnings, "task branch contains no commits")
			}
		}
	}

	if strategy != StrategyBranch {
		if target == "" {
			p.Blockers = append(p.Blockers, "target branch is required")
		} else if repoOK {
			p.TargetRef = target
			current, e := corralgit.CurrentBranch(ctx, task.Repo)
			if e != nil || current != target {
				p.Blockers = append(p.Blockers, "target is not the repository's checked-out branch")
			}
			if head, e := corralgit.RevParse(ctx, task.Repo, "refs/heads/"+target); e != nil {
				p.Blockers = append(p.Blockers, "cannot resolve target branch")
			} else {
				p.TargetHead = head
			}
			if dirty, e := corralgit.StatusPorcelain(ctx, task.Repo); e != nil {
				p.Blockers = append(p.Blockers, "cannot inspect target worktree")
			} else {
				p.TargetDirty = strings.TrimSpace(dirty) != ""
				if p.TargetDirty {
					p.Blockers = append(p.Blockers, "target worktree is dirty")
				}
			}
			if inProgress, e := corralgit.OperationInProgress(ctx, task.Repo); e != nil {
				p.Blockers = append(p.Blockers, "cannot inspect target operation state")
			} else if inProgress {
				p.Blockers = append(p.Blockers, "target has a Git operation in progress")
			}
			if p.BaseCommit != "" && p.TargetHead != "" {
				if ok, e := corralgit.IsAncestor(ctx, task.Repo, p.BaseCommit, p.TargetHead); e != nil || !ok {
					p.Blockers = append(p.Blockers, "target does not contain the task base commit")
				}
			}
			if p.BaseCommit != "" && p.TaskHead != "" && p.TargetHead != "" {
				if out, e := corralgit.MergeTree(ctx, task.Repo, p.BaseCommit, p.TargetHead, p.TaskHead); e != nil {
					p.Blockers = append(p.Blockers, "cannot analyze conflicts")
				} else if strings.Contains(out, "<<<<<<<") || strings.Contains(out, "changed in both") {
					p.Blockers = append(p.Blockers, "task conflicts with target")
				}
			}
		}
	}
	p.CanApply = len(p.Blockers) == 0
	return p, nil
}

func (s *Service) Diff(ctx context.Context, taskID string, full bool) (string, error) {
	task, err := s.st.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if _, err := s.st.GetTaskReview(ctx, taskID); err != nil {
		return "", ErrNotReviewable
	}
	if task.BaseCommit == "" || task.Worktree == "" || task.Worktree == "requested" {
		return "", ErrNotReviewable
	}
	head, err := corralgit.RevParse(ctx, task.Worktree, "HEAD")
	if err != nil {
		return "", err
	}
	return corralgit.Diff(ctx, task.Repo, task.BaseCommit, head, full)
}

func (s *Service) repoLock(repo string) *sync.Mutex {
	v, _ := s.locks.LoadOrStore(repo, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Service) Release(ctx context.Context, taskID string, req ReleaseRequest) (Result, error) {
	task, err := s.st.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	lockKey, err := corralgit.CanonicalPath(task.Repo)
	if err != nil {
		return Result{}, err
	}
	mu := s.repoLock(lockKey)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.Preflight(ctx, taskID, req.Strategy, req.Target)
	if err != nil {
		return Result{}, err
	}
	if !p.CanApply {
		s.failed(ctx, session.EventReviewReleaseFailed, p, "preflight blocked")
		return Result{Preflight: p}, ErrPreflight
	}
	if req.Strategy != StrategyBranch && req.ExpectedTargetHead != p.TargetHead {
		s.failed(ctx, session.EventReviewReleaseFailed, p, "target identity changed")
		return Result{Preflight: p}, ErrIdentityChanged
	}
	resultCommit := p.TaskHead
	switch req.Strategy {
	case StrategyMerge:
		if err := corralgit.Merge(ctx, p.Repo, p.TaskHead); err != nil {
			corralgit.MergeAbort(ctx, p.Repo)
			s.failed(ctx, session.EventReviewReleaseFailed, p, err.Error())
			return Result{Preflight: p}, err
		}
		resultCommit, err = corralgit.RevParse(ctx, p.Repo, "HEAD")
	case StrategyCherryPick:
		if len(p.Commits) > 0 {
			if err := corralgit.CherryPick(ctx, p.Repo, p.Commits); err != nil {
				corralgit.CherryPickAbort(ctx, p.Repo)
				s.failed(ctx, session.EventReviewReleaseFailed, p, err.Error())
				return Result{Preflight: p}, err
			}
		}
		resultCommit, err = corralgit.RevParse(ctx, p.Repo, "HEAD")
	}
	if err != nil {
		s.failed(ctx, session.EventReviewReleaseFailed, p, err.Error())
		return Result{Preflight: p}, err
	}
	data := auditJSON(p, "", resultCommit, false, "")
	if err := s.st.CompleteTaskReview(ctx, taskID, store.ReviewReleased, string(req.Strategy), req.Target, p.TargetHead, resultCommit, "", session.EventReviewReleaseSucceeded, data); err != nil {
		s.failed(ctx, session.EventReviewReleaseFailed, p, "git succeeded but review persistence failed: "+err.Error())
		return Result{Preflight: p}, err
	}
	rev, err := s.st.GetTaskReview(ctx, taskID)
	return Result{Review: rev, Preflight: p}, err
}

func (s *Service) Discard(ctx context.Context, taskID string, req DiscardRequest) (Result, error) {
	task, err := s.st.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	lockKey, err := corralgit.CanonicalPath(task.Repo)
	if err != nil {
		return Result{}, err
	}
	mu := s.repoLock(lockKey)
	mu.Lock()
	defer mu.Unlock()
	p, err := s.Preflight(ctx, taskID, StrategyBranch, "")
	if err != nil {
		return Result{}, err
	}
	// Dirty state is the sole blocker force may override; every identity
	// blocker remains fatal.
	if req.Force && p.TaskDirty {
		filtered := p.Blockers[:0]
		for _, b := range p.Blockers {
			if b != "task worktree is dirty" {
				filtered = append(filtered, b)
			}
		}
		p.Blockers = filtered
		p.CanApply = len(filtered) == 0
	}
	if !p.CanApply {
		s.failed(ctx, session.EventReviewDiscardFailed, p, "preflight blocked")
		return Result{Preflight: p}, ErrPreflight
	}
	if req.ExpectedTaskHead != p.TaskHead {
		s.failed(ctx, session.EventReviewDiscardFailed, p, "task identity changed")
		return Result{Preflight: p}, ErrIdentityChanged
	}
	if req.Force && (req.ExpectedRepo != p.Repo || req.ExpectedWorktree != p.Worktree || req.ExpectedBranch != p.Branch) {
		s.failed(ctx, session.EventReviewDiscardFailed, p, "forced discard identity mismatch")
		return Result{Preflight: p}, ErrIdentityChanged
	}
	ref := fmt.Sprintf("refs/corral/recovery/%s/%d-%s", taskID, s.clk.Now().UTC().UnixMilli(), uuid.NewString()[:8])
	if err := corralgit.CreateRef(ctx, p.Repo, ref, p.TaskHead); err != nil {
		s.failed(ctx, session.EventReviewDiscardFailed, p, err.Error())
		return Result{Preflight: p}, err
	}
	if err := corralgit.RemoveWorktree(ctx, p.Repo, p.Worktree); err != nil {
		s.failed(ctx, session.EventReviewDiscardFailed, p, err.Error())
		return Result{Preflight: p}, err
	}
	data := auditJSON(p, ref, p.TaskHead, req.Force, "")
	if err := s.st.CompleteTaskReview(ctx, taskID, store.ReviewDiscarded, "discard", "", "", p.TaskHead, ref, session.EventReviewDiscardSucceeded, data); err != nil {
		s.failed(ctx, session.EventReviewDiscardFailed, p, "git succeeded but review persistence failed: "+err.Error())
		return Result{Preflight: p}, err
	}
	rev, err := s.st.GetTaskReview(ctx, taskID)
	return Result{Review: rev, Preflight: p}, err
}

func auditJSON(p Preflight, recovery, result string, force bool, reason string) string {
	b, _ := json.Marshal(map[string]any{"task_id": p.TaskID, "repo": p.Repo, "worktree": p.Worktree, "branch": p.Branch, "base_commit": p.BaseCommit, "task_head": p.TaskHead, "target_ref": p.TargetRef, "target_before": p.TargetHead, "strategy": p.Strategy, "result_commit": result, "recovery_ref": recovery, "force": force, "reason": reason})
	return string(b)
}
func (s *Service) failed(ctx context.Context, kind session.EventKind, p Preflight, reason string) {
	_, _ = s.st.AppendEvent(ctx, "", kind, auditJSON(p, "", "", false, reason))
}
