package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/djbu/corral/internal/apitoken"
)

// newTokensTestServer mirrors newDagsTestServer for the tokens routes.
func newTokensTestServer(t *testing.T, deps TokensDeps) *Server {
	t.Helper()
	s := New()
	s.RegisterTokens(deps)
	return s
}

func newTokensTestDeps(t *testing.T) TokensDeps {
	t.Helper()
	return TokensDeps{Store: openMetaTestStore(t)}
}

// --- 1. POST /v1/tokens happy path (admin, default scope) ----------------

func TestHandleCreateToken_HappyPath_AdminDefaultScope(t *testing.T) {
	deps := newTokensTestDeps(t)
	srv := newTokensTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tokens", mustMarshal(t, createTokenRequest{Label: "laptop"}))
	assertAPIVersionHeader(t, rec)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	var got tokenCreatedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if got.ID == "" {
		t.Fatal("id is empty, want non-empty")
	}
	if got.Label != "laptop" {
		t.Fatalf("label = %q, want %q", got.Label, "laptop")
	}
	if got.Scope != "admin" {
		t.Fatalf("scope = %q, want %q (default)", got.Scope, "admin")
	}
	if got.SessionID != "" {
		t.Fatalf("session_id = %q, want empty for admin scope", got.SessionID)
	}
	if !strings.HasPrefix(got.Token, "crl_") {
		t.Fatalf("token = %q, want crl_-prefixed", got.Token)
	}
}

// --- 2. POST /v1/tokens happy path (session scope) ------------------------

func TestHandleCreateToken_HappyPath_SessionScope(t *testing.T) {
	deps := newTokensTestDeps(t)
	srv := newTokensTestServer(t, deps)

	reqBody := createTokenRequest{Label: "phone", Scope: "session", SessionID: "sess-1"}
	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tokens", mustMarshal(t, reqBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var got tokenCreatedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if got.Scope != "session" {
		t.Fatalf("scope = %q, want %q", got.Scope, "session")
	}
	if got.SessionID != "sess-1" {
		t.Fatalf("session_id = %q, want %q", got.SessionID, "sess-1")
	}
}

// --- 3. Validation table ---------------------------------------------------

func TestHandleCreateToken_Validation(t *testing.T) {
	cases := []struct {
		name string
		req  createTokenRequest
	}{
		{name: "empty label", req: createTokenRequest{Label: ""}},
		{name: "bad scope", req: createTokenRequest{Label: "x", Scope: "superadmin"}},
		{name: "session scope without session_id", req: createTokenRequest{Label: "x", Scope: "session"}},
		{name: "admin scope with session_id", req: createTokenRequest{Label: "x", Scope: "admin", SessionID: "sess-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTokensTestDeps(t)
			srv := newTokensTestServer(t, deps)

			rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tokens", mustMarshal(t, tc.req))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			env := decodeErrorEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != CodeBadRequest {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
			}
		})
	}
}

// --- 4. POST /v1/tokens with malformed JSON --------------------------------

func TestHandleCreateToken_BadJSON(t *testing.T) {
	deps := newTokensTestDeps(t)
	srv := newTokensTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tokens", []byte("{not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != CodeBadRequest {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeBadRequest)
	}
}

// --- 5. GET /v1/tokens never leaks plaintext or hash -----------------------

// hex64 matches a bare 64-hex-character run — the shape of a raw SHA-256
// hex digest (apitoken.hashToken's output). GET's response body must never
// contain one: TokenRow has no hash field to begin with (structural), but
// this pins the wire contract too, in case a future field addition
// resurrects the leak at the JSON layer instead of the Go layer.
var hex64 = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

func TestHandleListTokens_NeverLeaksPlaintextOrHash(t *testing.T) {
	deps := newTokensTestDeps(t)
	srv := newTokensTestServer(t, deps)

	createRec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tokens", mustMarshal(t, createTokenRequest{Label: "laptop"}))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body=%s", createRec.Code, createRec.Body.String())
	}
	var created tokenCreatedResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	plaintext := created.Token
	if plaintext == "" {
		t.Fatal("fixture bug: minted token has empty plaintext")
	}
	hash := apitoken.HashForLookup(plaintext)

	listRec := doVersioned(t, srv.Handler(), http.MethodGet, "/v1/tokens", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200, body=%s", listRec.Code, listRec.Body.String())
	}
	raw := listRec.Body.Bytes()

	if strings.Contains(string(raw), plaintext) {
		t.Fatalf("GET /v1/tokens body contains the minted plaintext: %s", raw)
	}
	if strings.Contains(string(raw), hash) {
		t.Fatalf("GET /v1/tokens body contains the token's hash: %s", raw)
	}
	if hex64.Match(raw) {
		t.Fatalf("GET /v1/tokens body contains a bare 64-hex-char run (looks like a hash): %s", raw)
	}

	var got listTokensResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, raw)
	}
	if len(got.Tokens) != 1 {
		t.Fatalf("len(tokens) = %d, want 1", len(got.Tokens))
	}
	if got.Tokens[0].ID != created.ID {
		t.Fatalf("tokens[0].id = %q, want %q", got.Tokens[0].ID, created.ID)
	}
	if got.Tokens[0].Label != "laptop" {
		t.Fatalf("tokens[0].label = %q, want %q", got.Tokens[0].Label, "laptop")
	}
	if got.Tokens[0].Scope != "admin" {
		t.Fatalf("tokens[0].scope = %q, want %q", got.Tokens[0].Scope, "admin")
	}
	if got.Tokens[0].RevokedMs != 0 {
		t.Fatalf("tokens[0].revoked_ms = %d, want 0 (active)", got.Tokens[0].RevokedMs)
	}
}

// --- 6. DELETE /v1/tokens/{id} revokes and verify fails --------------------

func TestHandleRevokeToken_RevokesThenVerifyFails(t *testing.T) {
	deps := newTokensTestDeps(t)
	srv := newTokensTestServer(t, deps)

	createRec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/tokens", mustMarshal(t, createTokenRequest{Label: "laptop"}))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body=%s", createRec.Code, createRec.Body.String())
	}
	var created tokenCreatedResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	delRec := doVersioned(t, srv.Handler(), http.MethodDelete, "/v1/tokens/"+created.ID, nil)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200, body=%s", delRec.Code, delRec.Body.String())
	}

	_, ok, err := deps.Store.VerifyToken(context.Background(), created.Token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if ok {
		t.Fatal("VerifyToken of a revoked token = true, want false")
	}
}

// --- 7. DELETE /v1/tokens/{unknown} is not an error ------------------------

func TestHandleRevokeToken_UnknownIDIsNotAnError(t *testing.T) {
	deps := newTokensTestDeps(t)
	srv := newTokensTestServer(t, deps)

	rec := doVersioned(t, srv.Handler(), http.MethodDelete, "/v1/tokens/no-such-id", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}
