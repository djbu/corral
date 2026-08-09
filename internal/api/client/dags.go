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
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	CostUSD     float64 `json:"cost_usd"`
	Worktree    string  `json:"worktree"`
	Branch      string  `json:"branch"`
	Model       string  `json:"model"`
	Attempts    int     `json:"attempts"`
	MaxAttempts int     `json:"max_attempts"`
	SessionID   string  `json:"session_id"`
	Repo        string  `json:"repo"`
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
