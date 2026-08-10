package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DagNode is one entry in SubmitDagRequest.Nodes: a task-to-be, identified
// within the request by Name (not yet an ID — the daemon mints the real
// task id after validation). Field tags mirror api.dagNodeRequest exactly
// (design doc m4.md §13 step 24) — this package defines its own wire
// structs rather than importing the api package, matching SessionInfo's
// convention.
type DagNode struct {
	Name           string   `json:"name"`
	Prompt         string   `json:"prompt"`
	Repo           string   `json:"repo"`
	Worktree       bool     `json:"worktree"`
	Model          string   `json:"model"`
	Template       string   `json:"template"`
	PermissionMode string   `json:"permission_mode"`
	MaxAttempts    int      `json:"max_attempts"`
	BudgetUSD      *float64 `json:"budget_usd"`
}

// DagEdge names a dependency edge by node NAME (not task id), mirroring
// api.dagEdgeRequest.
type DagEdge struct {
	Task      string `json:"task"`
	DependsOn string `json:"depends_on"`
}

// SubmitDagRequest is POST /v1/dags's body, mirroring api.createDagRequest.
type SubmitDagRequest struct {
	BudgetUSD *float64  `json:"budget_usd"`
	Nodes     []DagNode `json:"nodes"`
	Edges     []DagEdge `json:"edges"`
}

// DagTask is one task inside a DagDetail, mirroring api.dagTaskResponse
// (including the repo field, needed client-side to compute review's diff
// base).
type DagTask struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	CostUSD      float64 `json:"cost_usd"`
	Worktree     string  `json:"worktree"`
	Branch       string  `json:"branch"`
	BaseCommit   string  `json:"base_commit"`
	ReviewStatus string  `json:"review_status"`
	Model        string  `json:"model"`
	Template     string  `json:"template"`
	Attempts     int     `json:"attempts"`
	MaxAttempts  int     `json:"max_attempts"`
	SessionID    string  `json:"session_id"`
	Repo         string  `json:"repo"`
}

// ReviewPreflight is the daemon's read-only identity and conflict analysis.
type ReviewPreflight struct {
	TaskID       string   `json:"task_id"`
	ReviewStatus string   `json:"review_status"`
	Strategy     string   `json:"strategy"`
	Repo         string   `json:"repo"`
	Worktree     string   `json:"worktree"`
	Branch       string   `json:"branch"`
	BaseCommit   string   `json:"base_commit"`
	TaskHead     string   `json:"task_head"`
	TargetRef    string   `json:"target_ref"`
	TargetHead   string   `json:"target_head"`
	Commits      []string `json:"commits"`
	TaskDirty    bool     `json:"task_dirty"`
	TargetDirty  bool     `json:"target_dirty"`
	CanApply     bool     `json:"can_apply"`
	Blockers     []string `json:"blockers"`
	Warnings     []string `json:"warnings"`
}

// ReviewRecord is the durable release or discard decision for one task.
type ReviewRecord struct {
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	Strategy     string `json:"strategy"`
	TargetRef    string `json:"target_ref"`
	TargetBefore string `json:"target_before"`
	ResultCommit string `json:"result_commit"`
	RecoveryRef  string `json:"recovery_ref"`
	CreatedMs    int64  `json:"created_ms"`
	UpdatedMs    int64  `json:"updated_ms"`
}

// ReviewResult combines the durable decision with the identities it applied.
type ReviewResult struct {
	Review    *ReviewRecord   `json:"review"`
	Preflight ReviewPreflight `json:"preflight"`
}

// ReleaseReviewRequest selects an explicit integration strategy and pins the
// target identity observed by preflight.
type ReleaseReviewRequest struct {
	Strategy           string `json:"strategy"`
	Target             string `json:"target,omitempty"`
	ExpectedTargetHead string `json:"expected_target_head,omitempty"`
}

// DiscardReviewRequest pins the task identity before removing its worktree.
type DiscardReviewRequest struct {
	ExpectedTaskHead string `json:"expected_task_head"`
	ExpectedRepo     string `json:"expected_repo,omitempty"`
	ExpectedWorktree string `json:"expected_worktree,omitempty"`
	ExpectedBranch   string `json:"expected_branch,omitempty"`
	Force            bool   `json:"force"`
}

// DagEdgeResp is one dependency edge in a DagDetail, by task ID (unlike
// DagEdge, which is by node name), mirroring api.dagEdgeResponse.
type DagEdgeResp struct {
	Task      string `json:"task"`
	DependsOn string `json:"depends_on"`
}

// DagDetail is the decoded body of POST /v1/dags's 201 and GET
// /v1/dags/{id}'s 200, mirroring api.dagDetailResponse. BudgetUSD is nil
// when the dag is unbounded.
type DagDetail struct {
	DAGID     string        `json:"dag_id"`
	BudgetUSD *float64      `json:"budget_usd"`
	CostUSD   float64       `json:"cost_usd"`
	Tasks     []DagTask     `json:"tasks"`
	Edges     []DagEdgeResp `json:"edges"`
}

// DagSummary is one entry in ListDAGs's result, mirroring
// api.dagSummaryResponse.
type DagSummary struct {
	DAGID     string   `json:"dag_id"`
	BudgetUSD *float64 `json:"budget_usd"`
	CostUSD   float64  `json:"cost_usd"`
}

type listDagsResponse struct {
	Dags []DagSummary `json:"dags"`
}

// SubmitDAG calls POST /v1/dags.
func (c *Client) SubmitDAG(ctx context.Context, req SubmitDagRequest) (DagDetail, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return DagDetail{}, fmt.Errorf("client: encoding submit-dag request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/dags", b)
	if err != nil {
		return DagDetail{}, err
	}
	var v DagDetail
	if err := decode(resp, &v); err != nil {
		return DagDetail{}, err
	}
	return v, nil
}

// GetDAG calls GET /v1/dags/{id}.
func (c *Client) GetDAG(ctx context.Context, id string) (DagDetail, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/dags/"+url.PathEscape(id), nil)
	if err != nil {
		return DagDetail{}, err
	}
	var v DagDetail
	if err := decode(resp, &v); err != nil {
		return DagDetail{}, err
	}
	return v, nil
}

// ListDAGs calls GET /v1/dags, unwrapping the {dags:[]} envelope.
func (c *Client) ListDAGs(ctx context.Context) ([]DagSummary, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/dags", nil)
	if err != nil {
		return nil, err
	}
	var v listDagsResponse
	if err := decode(resp, &v); err != nil {
		return nil, err
	}
	return v.Dags, nil
}

// ReviewPreflight calls the task review preflight endpoint.
func (c *Client) ReviewPreflight(ctx context.Context, taskID, strategy, target string) (ReviewPreflight, error) {
	q := url.Values{"strategy": []string{strategy}}
	if target != "" {
		q.Set("target", target)
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/review/preflight?"+q.Encode(), nil)
	if err != nil {
		return ReviewPreflight{}, err
	}
	var v ReviewPreflight
	if err := decode(resp, &v); err != nil {
		return v, err
	}
	return v, nil
}

// ReleaseReview calls the checked task review release endpoint.
func (c *Client) ReleaseReview(ctx context.Context, taskID string, req ReleaseReviewRequest) (ReviewResult, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return ReviewResult{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/review/release", b)
	if err != nil {
		return ReviewResult{}, err
	}
	var v ReviewResult
	if err := decode(resp, &v); err != nil {
		return v, err
	}
	return v, nil
}

// DiscardReview calls the recoverable task review discard endpoint.
func (c *Client) DiscardReview(ctx context.Context, taskID string, req DiscardReviewRequest) (ReviewResult, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return ReviewResult{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/review/discard", b)
	if err != nil {
		return ReviewResult{}, err
	}
	var v ReviewResult
	if err := decode(resp, &v); err != nil {
		return v, err
	}
	return v, nil
}
