package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// CreateTokenRequest is POST /v1/tokens's body, mirroring
// api.createTokenRequest (design doc m5.md §11 step 27). Scope "" lets the
// daemon default to "admin".
type CreateTokenRequest struct {
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	SessionID string `json:"session_id"`
}

// TokenCreated is the decoded body of POST /v1/tokens's 201, mirroring
// api.tokenCreatedResponse. Token is the plaintext bearer token — it is
// shown here and nowhere else; ListTokens's TokenInfo has no field for it.
type TokenCreated struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// TokenInfo is one entry in ListTokens's result, mirroring
// api.tokenInfoResponse. It has no field for the plaintext or the hash.
type TokenInfo struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Scope      string `json:"scope"`
	SessionID  string `json:"session_id"`
	CreatedMs  int64  `json:"created_ms"`
	LastUsedMs int64  `json:"last_used_ms"`
	RevokedMs  int64  `json:"revoked_ms"`
}

type listTokensResponse struct {
	Tokens []TokenInfo `json:"tokens"`
}

// CreateToken calls POST /v1/tokens.
func (c *Client) CreateToken(ctx context.Context, req CreateTokenRequest) (TokenCreated, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return TokenCreated{}, fmt.Errorf("client: encoding create-token request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/tokens", b)
	if err != nil {
		return TokenCreated{}, err
	}
	var v TokenCreated
	if err := decode(resp, &v); err != nil {
		return TokenCreated{}, err
	}
	return v, nil
}

// ListTokens calls GET /v1/tokens, unwrapping the {tokens:[]} envelope.
func (c *Client) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/tokens", nil)
	if err != nil {
		return nil, err
	}
	var v listTokensResponse
	if err := decode(resp, &v); err != nil {
		return nil, err
	}
	return v.Tokens, nil
}

// RevokeToken calls DELETE /v1/tokens/{id}.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/tokens/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return decode(resp, nil)
}
