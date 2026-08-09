package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/danielbecerra/corral/internal/apitoken"
	"github.com/danielbecerra/corral/internal/store"
)

// TokensDeps is everything handlers_tokens.go's routes need. daemon.go
// constructs one at startup and passes it to RegisterTokens.
type TokensDeps struct {
	Store *store.Store
}

// RegisterTokens registers POST /v1/tokens, GET /v1/tokens, and DELETE
// /v1/tokens/{id} on s (m5.md §11 step 27). These endpoints are reachable
// on the unix socket today with no auth at all — bearer-auth middleware
// lands in step 28 and TCP reachability in step 30; minting a token over
// the unix socket is itself the trust boundary until then (m5.md §11: "the
// operator is on the box").
func (s *Server) RegisterTokens(deps TokensDeps) {
	s.Handle("POST /v1/tokens", deps.handleCreate)
	s.Handle("GET /v1/tokens", deps.handleList)
	s.Handle("DELETE /v1/tokens/{id}", deps.handleRevoke)
}

// createTokenRequest is POST /v1/tokens's body.
type createTokenRequest struct {
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	SessionID string `json:"session_id"`
}

// tokenCreatedResponse is the body of POST /v1/tokens's 201. Token is the
// plaintext, minted fresh by apitoken.Mint — this is the ONLY response
// shape in this file (or anywhere else in corral) that ever carries it;
// tokenInfoResponse (list) has no field for it, structurally, matching
// store.TokenRow's own no-hash-field discipline.
type tokenCreatedResponse struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	SessionID string `json:"session_id,omitempty"`
	Token     string `json:"token"`
}

// tokenInfoResponse is one entry in listTokensResponse, mirroring
// store.TokenRow. It has no field for the hash or the plaintext — see
// TokenRow's own doc comment for why that's structural, not disciplinary.
type tokenInfoResponse struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Scope      string `json:"scope"`
	SessionID  string `json:"session_id,omitempty"`
	CreatedMs  int64  `json:"created_ms"`
	LastUsedMs int64  `json:"last_used_ms,omitempty"`
	RevokedMs  int64  `json:"revoked_ms,omitempty"`
}

type listTokensResponse struct {
	Tokens []tokenInfoResponse `json:"tokens"`
}

// handleCreate validates and mints a new api token (m5.md §11 step 27).
// scope defaults to "admin" when omitted; the session_id/scope pairing is
// validated here (the shape only — step 34 enforces that a session-scoped
// session_id actually names a live session).
func (d TokensDeps) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
		return
	}

	if req.Label == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "label must not be empty", nil)
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = "admin"
	}
	if scope != "admin" && scope != "session" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "scope must be \"admin\" or \"session\"", nil)
		return
	}

	// The silent-NULL trap flagged in step 26: a session-scoped token with
	// no session_id would mint fine (the store column is nullable) and
	// then be un-confinable at auth time; an admin token WITH a
	// session_id is nonsensical the other way. Both are rejected at mint
	// time rather than left for step 34's enforcement to discover.
	if scope == "session" && req.SessionID == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "session_id is required when scope is \"session\"", nil)
		return
	}
	if scope == "admin" && req.SessionID != "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "session_id must be empty when scope is \"admin\"", nil)
		return
	}

	plaintext, hash, err := apitoken.Mint()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	id := uuid.New().String()

	var sessionID *string
	if scope == "session" {
		sessionID = &req.SessionID
	}

	if err := d.Store.CreateToken(r.Context(), id, hash, req.Label, scope, sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusCreated, tokenCreatedResponse{
		ID:        id,
		Label:     req.Label,
		Scope:     scope,
		SessionID: req.SessionID,
		Token:     plaintext,
	})
}

// handleList serves GET /v1/tokens: every token's metadata, never the
// plaintext or the hash.
func (d TokensDeps) handleList(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListTokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out := make([]tokenInfoResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, tokenInfoResponse{
			ID:         row.ID,
			Label:      row.Label,
			Scope:      row.Scope,
			SessionID:  row.SessionID,
			CreatedMs:  row.CreatedMs,
			LastUsedMs: row.LastUsedMs,
			RevokedMs:  row.RevokedMs,
		})
	}
	writeJSON(w, http.StatusOK, listTokensResponse{Tokens: out})
}

// handleRevoke serves DELETE /v1/tokens/{id}. Revoking an unknown or
// already-revoked id is not an error — store.RevokeToken is already
// idempotent — so this always answers 200 once the store call succeeds.
func (d TokensDeps) handleRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := d.Store.RevokeToken(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}
