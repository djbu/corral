package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// SessionInfo is the decoded <Session> JSON shape (design doc §9.2).
type SessionInfo struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Mode            string  `json:"mode"`
	Cwd             string  `json:"cwd"`
	Status          string  `json:"status"`
	DesiredState    string  `json:"desired_state"`
	AgentState      string  `json:"agent_state"`
	Attached        bool    `json:"attached"`
	PID             int     `json:"pid"`
	Model           string  `json:"model"`
	ClaudeSessionID string  `json:"claude_session_id"`
	Rows            int     `json:"rows"`
	Cols            int     `json:"cols"`
	ResumeCount     int     `json:"resume_count"`
	CreatedAt       string  `json:"created_at"`
	StartedAt       *string `json:"started_at"`
	LastAttachedAt  *string `json:"last_attached_at"`
	EndedAt         *string `json:"ended_at"`
	ExitCode        *int    `json:"exit_code"`
	ExitSignal      *string `json:"exit_signal"`
}

type listSessionsResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// ListSessions calls GET /v1/sessions.
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	var v listSessionsResponse
	if err := decode(resp, &v); err != nil {
		return nil, err
	}
	return v.Sessions, nil
}

// CreateSessionRequest is the POST /v1/sessions request body.
type CreateSessionRequest struct {
	Name  string `json:"name,omitempty"`
	Cwd   string `json:"cwd"`
	Mode  string `json:"mode,omitempty"`
	Model string `json:"model,omitempty"`
	Rows  uint16 `json:"rows,omitempty"`
	Cols  uint16 `json:"cols,omitempty"`
}

// CreateSession calls POST /v1/sessions.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (SessionInfo, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("client: encoding create-session request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/sessions", b)
	if err != nil {
		return SessionInfo{}, err
	}
	var v SessionInfo
	if err := decode(resp, &v); err != nil {
		return SessionInfo{}, err
	}
	return v, nil
}

// GetSession calls GET /v1/sessions/{idOrName}.
func (c *Client) GetSession(ctx context.Context, idOrName string) (SessionInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(idOrName), nil)
	if err != nil {
		return SessionInfo{}, err
	}
	var v SessionInfo
	if err := decode(resp, &v); err != nil {
		return SessionInfo{}, err
	}
	return v, nil
}

// EventInfo is one decoded row of GET /v1/sessions/{idOrName}/events.
// Data is the raw stored audit payload, embedded verbatim.
type EventInfo struct {
	Seq  int64           `json:"seq"`
	Ts   string          `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type listEventsResponse struct {
	Events []EventInfo `json:"events"`
}

// ListEvents calls GET /v1/sessions/{idOrName}/events, returning the
// session's append-only event stream in seq-ascending order.
func (c *Client) ListEvents(ctx context.Context, idOrName string) ([]EventInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(idOrName)+"/events", nil)
	if err != nil {
		return nil, err
	}
	var v listEventsResponse
	if err := decode(resp, &v); err != nil {
		return nil, err
	}
	return v.Events, nil
}

// KillSession calls DELETE /v1/sessions/{idOrName}, optionally overriding
// the shutdown grace (0 omits the query param, letting the daemon use its
// configured default).
func (c *Client) KillSession(ctx context.Context, idOrName string, grace time.Duration) (SessionInfo, error) {
	path := "/v1/sessions/" + url.PathEscape(idOrName)
	if grace > 0 {
		path += "?grace=" + url.QueryEscape(grace.String())
	}
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return SessionInfo{}, err
	}
	var v SessionInfo
	if err := decode(resp, &v); err != nil {
		return SessionInfo{}, err
	}
	return v, nil
}
