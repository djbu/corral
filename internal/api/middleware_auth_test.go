package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/apitoken"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/version"
)

// recordedEvent is one call fakeAuthStore.AppendEvent captured.
type recordedEvent struct {
	SessionID string
	Kind      session.EventKind
	DataJSON  string
}

// fakeAuthStore is a tokenAuthStore test double. VerifyToken delegates to
// a real temp *store.Store (so mint/revoke lifecycle is exercised for
// real, not stubbed), unless verifyErr is set, in which case it reports a
// store failure without touching the real store at all. AppendEvent calls
// are recorded rather than persisted, and TouchToken calls are signaled on
// a buffered channel so the fire-and-forget goroutine is assertable
// without a sleep.
type fakeAuthStore struct {
	store     *store.Store
	verifyErr error
	events    []recordedEvent
	touched   chan string
}

func newFakeAuthStore(t *testing.T) *fakeAuthStore {
	t.Helper()
	return &fakeAuthStore{
		store:   openMetaTestStore(t),
		touched: make(chan string, 8),
	}
}

func (f *fakeAuthStore) VerifyToken(ctx context.Context, plaintext string) (store.TokenRow, bool, error) {
	if f.verifyErr != nil {
		return store.TokenRow{}, false, f.verifyErr
	}
	return f.store.VerifyToken(ctx, plaintext)
}

func (f *fakeAuthStore) TouchToken(ctx context.Context, id string) {
	f.touched <- id
}

func (f *fakeAuthStore) AppendEvent(ctx context.Context, sessionID string, kind session.EventKind, dataJSON string) (*session.Event, error) {
	f.events = append(f.events, recordedEvent{SessionID: sessionID, Kind: kind, DataJSON: dataJSON})
	return &session.Event{SessionID: sessionID, Kind: kind, DataJSON: dataJSON}, nil
}

// mintToken mints a real token in f's backing store, returning the
// plaintext.
func mintToken(t *testing.T, f *fakeAuthStore) string {
	t.Helper()
	plaintext, hash, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := f.store.CreateToken(context.Background(), "tok-"+plaintext[len(plaintext)-6:], hash, "test", "admin", nil); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return plaintext
}

// authProbe writes 200 and, if TokenFromContext resolves, the resolved
// TokenRow.ID as the body — so a test can tell not just that a request
// passed auth, but which token it resolved to.
func authProbe(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if row, ok := TokenFromContext(r.Context()); ok {
		_, _ = w.Write([]byte(row.ID))
	}
}

// doAuthed builds a version-header'd request (mirroring doVersioned) with
// an optional Authorization header, against h.
func doAuthed(t *testing.T, h http.Handler, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newAuthTestHandler(auth tokenAuthStore) http.Handler {
	s := New()
	s.Handle("GET /v1/version", authProbe)
	return s.AuthenticatedHandler(auth, nil, nil)
}

// --- 1. valid token -------------------------------------------------------

func TestBearerAuth_ValidToken(t *testing.T) {
	f := newFakeAuthStore(t)
	plaintext := mintToken(t, f)
	row, ok, err := f.store.VerifyToken(context.Background(), plaintext)
	if err != nil || !ok {
		t.Fatalf("sanity VerifyToken: row=%v ok=%v err=%v", row, ok, err)
	}

	h := newAuthTestHandler(f)
	rec := doAuthed(t, h, "Bearer "+plaintext)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != row.ID {
		t.Fatalf("body = %q, want resolved TokenRow.ID %q", rec.Body.String(), row.ID)
	}

	select {
	case id := <-f.touched:
		if id != row.ID {
			t.Fatalf("TouchToken id = %q, want %q", id, row.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TouchToken was not called within timeout")
	}
}

// --- 1b. scheme is case-insensitive per RFC 7235 ---------------------------

func TestBearerAuth_SchemeIsCaseInsensitive(t *testing.T) {
	f := newFakeAuthStore(t)
	plaintext := mintToken(t, f)

	h := newAuthTestHandler(f)
	rec := doAuthed(t, h, "bearer "+plaintext)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for lowercase \"bearer\" scheme, body=%s", rec.Code, rec.Body.String())
	}
}

// --- 2-6. every failure mode is a 401 -------------------------------------

func TestBearerAuth_FailureModes(t *testing.T) {
	cases := []struct {
		name string
		auth string
	}{
		{"missing header", ""},
		{"malformed scheme", "Basic xyz"},
		{"malformed empty token", "Bearer"},
		{"unknown token", "Bearer crl_doesnotexist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAuthStore(t)
			h := newAuthTestHandler(f)
			rec := doAuthed(t, h, tc.auth)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestBearerAuth_RevokedToken(t *testing.T) {
	f := newFakeAuthStore(t)
	plaintext := mintToken(t, f)
	row, ok, err := f.store.VerifyToken(context.Background(), plaintext)
	if err != nil || !ok {
		t.Fatalf("sanity VerifyToken before revoke: row=%v ok=%v err=%v", row, ok, err)
	}
	if err := f.store.RevokeToken(context.Background(), row.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	h := newAuthTestHandler(f)
	rec := doAuthed(t, h, "Bearer "+plaintext)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

// --- 7. byte-identical bodies across every failure mode --------------------

func TestBearerAuth_FailureBodiesAreByteIdentical(t *testing.T) {
	// authFor builds the Authorization header value for each failure mode
	// against a fresh fakeAuthStore f, so "revoked" can mint+revoke a real
	// token in f's own backing store.
	authFor := map[string]func(f *fakeAuthStore) string{
		"missing":   func(f *fakeAuthStore) string { return "" },
		"malformed": func(f *fakeAuthStore) string { return "Basic xyz" },
		"unknown":   func(f *fakeAuthStore) string { return "Bearer crl_doesnotexist" },
		"revoked": func(f *fakeAuthStore) string {
			pt := mintToken(t, f)
			row, ok, err := f.store.VerifyToken(context.Background(), pt)
			if err != nil || !ok {
				t.Fatalf("sanity VerifyToken: row=%v ok=%v err=%v", row, ok, err)
			}
			if err := f.store.RevokeToken(context.Background(), row.ID); err != nil {
				t.Fatalf("RevokeToken: %v", err)
			}
			return "Bearer " + pt
		},
	}

	bodies := map[string][]byte{}
	for name, buildAuth := range authFor {
		f := newFakeAuthStore(t)
		auth := buildAuth(f)
		h := newAuthTestHandler(f)
		rec := doAuthed(t, h, auth)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401, body=%s", name, rec.Code, rec.Body.String())
		}
		bodies[name] = rec.Body.Bytes()
	}

	var first []byte
	var firstName string
	for name, b := range bodies {
		if first == nil {
			first, firstName = b, name
			continue
		}
		if string(b) != string(first) {
			t.Fatalf("body for %q = %q, differs from %q's body %q", name, b, firstName, first)
		}
	}

	// All bodies being equal is only meaningful if the shared body is
	// actually the expected unauthorized envelope — otherwise four empty
	// (or four wrong) bodies would trivially satisfy the loop above.
	var env errorEnvelope
	if err := json.Unmarshal(first, &env); err != nil {
		t.Fatalf("Unmarshal shared body: %v, body=%s", err, first)
	}
	if env.Error.Code != CodeUnauthorized {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeUnauthorized)
	}
	if env.Error.Message != "unauthorized" {
		t.Fatalf("error.message = %q, want %q", env.Error.Message, "unauthorized")
	}
}

// --- 8. no token material in the unauthorized event -----------------------

func TestBearerAuth_UnauthorizedEventCarriesNoTokenMaterial(t *testing.T) {
	f := newFakeAuthStore(t)
	h := newAuthTestHandler(f)

	rec := doAuthed(t, h, "Bearer crl_doesnotexist")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}

	if len(f.events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(f.events))
	}
	ev := f.events[0]
	if ev.Kind != session.EventTokenUnauthorized {
		t.Fatalf("kind = %q, want %q", ev.Kind, session.EventTokenUnauthorized)
	}
	if ev.SessionID != "" {
		t.Fatalf("sessionID = %q, want \"\" (daemon-scoped)", ev.SessionID)
	}
	if strings.Contains(ev.DataJSON, "crl_doesnotexist") {
		t.Fatalf("event data JSON contains the attempted token: %s", ev.DataJSON)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(ev.DataJSON), &data); err != nil {
		t.Fatalf("event data JSON does not parse: %v (%s)", err, ev.DataJSON)
	}
	if _, ok := data["remote_addr"]; !ok {
		t.Fatalf("event data JSON missing remote_addr: %s", ev.DataJSON)
	}
}

// --- 9. store error path is 500, not 401 -----------------------------------

func TestBearerAuth_StoreErrorIs500NotUnauthorized(t *testing.T) {
	f := newFakeAuthStore(t)
	f.verifyErr = errors.New("boom: db is unreachable")

	h := newAuthTestHandler(f)
	rec := doAuthed(t, h, "Bearer crl_whatever")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("Unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if env.Error.Code != CodeInternal {
		t.Fatalf("error.code = %q, want %q", env.Error.Code, CodeInternal)
	}
	if len(f.events) != 0 {
		t.Fatalf("len(events) = %d, want 0 (a store error is not an auth failure)", len(f.events))
	}
}

// --- 10. the unix Handler() has no auth applied at all ---------------------

func TestHandler_UnixPathHasNoAuth(t *testing.T) {
	s := New()
	s.Handle("GET /v1/version", authProbe)

	rec := doVersioned(t, s.Handler(), http.MethodGet, "/v1/version", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unix Handler() must not require a token), body=%s", rec.Code, rec.Body.String())
	}
}
