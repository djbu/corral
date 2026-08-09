package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type LearningInfo struct {
	ID            string                 `json:"id"`
	Repo          string                 `json:"repo"`
	Kind          string                 `json:"kind"`
	Fingerprint   string                 `json:"fingerprint"`
	Status        string                 `json:"status"`
	Content       json.RawMessage        `json:"content"`
	EvidenceCount int                    `json:"evidence_count"`
	Baseline      json.RawMessage        `json:"baseline"`
	Verification  json.RawMessage        `json:"verification"`
	CreatedMs     int64                  `json:"created_ms"`
	UpdatedMs     int64                  `json:"updated_ms"`
	VerifiedMs    *int64                 `json:"verified_ms"`
	ProposedMs    *int64                 `json:"proposed_ms"`
	AdoptedMs     *int64                 `json:"adopted_ms"`
	RejectedMs    *int64                 `json:"rejected_ms"`
	ExpiresMs     int64                  `json:"expires_ms"`
	Evidence      []LearningEvidenceInfo `json:"evidence"`
}

type LearningEvidenceInfo struct {
	EventSeq int64  `json:"event_seq"`
	Role     string `json:"role"`
}

type ScanLearningsResult struct {
	WindowStartMs int64          `json:"window_start_ms"`
	WindowEndMs   int64          `json:"window_end_ms"`
	Sessions      int            `json:"sessions"`
	SkippedCWDs   int            `json:"skipped_cwds"`
	Eligible      int            `json:"eligible_requests"`
	Learnings     []LearningInfo `json:"learnings"`
}

type LearningMeasurementInfo struct {
	ID            string          `json:"id"`
	Phase         string          `json:"phase"`
	WindowStartMs int64           `json:"window_start_ms"`
	WindowEndMs   int64           `json:"window_end_ms"`
	Metrics       json.RawMessage `json:"metrics"`
	Verdict       string          `json:"verdict"`
	CreatedMs     int64           `json:"created_ms"`
}

type LearningReport struct {
	Learning     LearningInfo              `json:"learning"`
	Measurements []LearningMeasurementInfo `json:"measurements"`
}

func (c *Client) ScanLearnings(ctx context.Context, repo string) (ScanLearningsResult, error) {
	body, err := json.Marshal(struct {
		Repo string `json:"repo"`
	}{Repo: repo})
	if err != nil {
		return ScanLearningsResult{}, fmt.Errorf("client: encoding learning scan: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/learnings/scan", body)
	if err != nil {
		return ScanLearningsResult{}, err
	}
	var out ScanLearningsResult
	if err := decode(resp, &out); err != nil {
		return ScanLearningsResult{}, err
	}
	return out, nil
}

func (c *Client) ListLearnings(ctx context.Context, repo, status string) ([]LearningInfo, error) {
	query := url.Values{}
	if repo != "" {
		query.Set("repo", repo)
	}
	if status != "" {
		query.Set("status", status)
	}
	path := "/v1/learnings"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Learnings []LearningInfo `json:"learnings"`
	}
	if err := decode(resp, &out); err != nil {
		return nil, err
	}
	return out.Learnings, nil
}

func (c *Client) GetLearning(ctx context.Context, id string) (LearningInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/learnings/"+url.PathEscape(id), nil)
	if err != nil {
		return LearningInfo{}, err
	}
	var out LearningInfo
	if err := decode(resp, &out); err != nil {
		return LearningInfo{}, err
	}
	return out, nil
}

func (c *Client) GetLearningReport(ctx context.Context, id string) (LearningReport, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/learnings/"+url.PathEscape(id)+"/report", nil)
	if err != nil {
		return LearningReport{}, err
	}
	var out LearningReport
	if err := decode(resp, &out); err != nil {
		return LearningReport{}, err
	}
	return out, nil
}
