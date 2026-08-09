package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/config"
	corralgit "github.com/danielbecerra/corral/internal/git"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/version"
)

func TestLearningsAPI_ScanListShowReport(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := t.TempDir()
	createLearningSession(t, st, "s1", repo)
	for i := 0; i < 3; i++ {
		appendLearningApproval(t, st, "s1", "npm test")
	}
	fc.Advance(time.Millisecond)
	cfg := config.Learn{Window: 14 * 24 * time.Hour, MinApprovals: 3, TTL: 90 * 24 * time.Hour}
	srv := New()
	srv.RegisterLearnings(LearningsDeps{Store: st, Clock: fc, Config: cfg})

	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/learnings/scan", mustMarshal(t, scanLearningsRequest{Repo: repo}))
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status = %d body=%s", rec.Code, rec.Body.String())
	}
	var scan scanLearningsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatal(err)
	}
	if len(scan.Learnings) != 1 || scan.Learnings[0].Status != string(store.LearningProposed) {
		t.Fatalf("scan = %+v", scan)
	}
	id := scan.Learnings[0].ID

	rec = doVersioned(t, srv.Handler(), http.MethodGet, "/v1/learnings?status=proposed&repo="+repo, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(id)) {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doVersioned(t, srv.Handler(), http.MethodGet, "/v1/learnings/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("show status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail learningResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Evidence) != 12 {
		t.Fatalf("detail = (%+v,%v)", detail, err)
	}
	rec = doVersioned(t, srv.Handler(), http.MethodGet, "/v1/learnings/"+id+"/report", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"measurements":[]`)) {
		t.Fatalf("report status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doVersioned(t, srv.Handler(), http.MethodPost, "/v1/learnings/"+id+"/adopt", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"status":"adopted"`)) {
		t.Fatalf("adopt status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.TransitionLearning(context.Background(), id, store.LearningTransition{
		From: store.LearningAdopted, To: store.LearningStale,
	}); err != nil {
		t.Fatal(err)
	}
	rec = doVersioned(t, srv.Handler(), http.MethodPost, "/v1/learnings/"+id+"/retire", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"status":"retired"`)) {
		t.Fatalf("retire status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLearningsAPI_SessionTokenScope(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repoA, repoB := t.TempDir(), t.TempDir()
	createLearningSession(t, st, "root", repoA)
	canonicalA, _ := corralgit.CanonicalPath(repoA)
	canonicalB, _ := corralgit.CanonicalPath(repoB)
	createAPILearning(t, st, "a", canonicalA, "fp-a")
	createAPILearning(t, st, "b", canonicalB, "fp-b")
	srv := New()
	srv.RegisterLearnings(LearningsDeps{Store: st, Clock: fc, Config: config.Learn{Window: time.Hour, MinApprovals: 3, TTL: time.Hour}})
	tokenCtx := contextWithToken(context.Background(), store.TokenRow{ID: "tok", Scope: "session", SessionID: "root"})

	rec := scopedVersionedRequest(t, srv.Handler(), tokenCtx, http.MethodGet, "/v1/learnings", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"id":"a"`)) || bytes.Contains(rec.Body.Bytes(), []byte(`"id":"b"`)) {
		t.Fatalf("scoped list status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = scopedVersionedRequest(t, srv.Handler(), tokenCtx, http.MethodGet, "/v1/learnings/b", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign show status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = scopedVersionedRequest(t, srv.Handler(), tokenCtx, http.MethodPost, "/v1/learnings/scan", mustMarshal(t, scanLearningsRequest{}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped scan status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = scopedVersionedRequest(t, srv.Handler(), tokenCtx, http.MethodPost, "/v1/learnings/a/reject", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped reject status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLearningsAPI_RejectRedactsOperatorReason(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	createAPILearning(t, st, "reject-me", t.TempDir(), "fp")
	srv := New()
	srv.RegisterLearnings(LearningsDeps{Store: st, Clock: fc, Config: config.Learn{}})
	secret := "sk-ant-abcdefghijklmnopqrstuvwxyz0123456789"
	rec := doVersioned(t, srv.Handler(), http.MethodPost, "/v1/learnings/reject-me/reject",
		mustMarshal(t, rejectLearningRequest{Reason: "contains " + secret}))
	if rec.Code != http.StatusOK {
		t.Fatalf("reject status=%d body=%s", rec.Code, rec.Body.String())
	}
	l, err := st.GetLearning(context.Background(), "reject-me")
	if err != nil || strings.Contains(l.VerificationJSON, secret) || !strings.Contains(l.VerificationJSON, "redacted") {
		t.Fatalf("rejected learning = (%+v,%v)", l, err)
	}
}

func scopedVersionedRequest(t *testing.T, h http.Handler, ctx context.Context, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Corral-Api-Version", fmt.Sprintf("%d", version.APIVersion))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func createLearningSession(t *testing.T, st *store.Store, id, cwd string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID: id, Name: id, Mode: session.ModeInteractive, Cwd: cwd,
		ClaudeBin: "/bin/true", SettingSources: "user,project,local",
		DesiredState: session.DesiredRunning, Status: session.StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func appendLearningApproval(t *testing.T, st *store.Store, sessionID, command string) {
	t.Helper()
	ctx := context.Background()
	req, err := st.AppendEvent(ctx, sessionID, session.EventPermissionRequested,
		`{"tool_name":"Bash","tool_input":{"command":"`+command+`"},"redactions":[],"truncated":false}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		kind session.EventKind
		data string
	}{
		{session.EventPermissionBlocked, fmt.Sprintf(`{"request_seq":%d}`, req.Seq)},
		{session.EventSessionAnswered, fmt.Sprintf(`{"permission_request_seq":%d,"via":"http"}`, req.Seq)},
		{session.EventPermissionResolved, fmt.Sprintf(`{"request_seq":%d,"outcome":"approved"}`, req.Seq)},
	} {
		if _, err := st.AppendEvent(ctx, sessionID, item.kind, item.data); err != nil {
			t.Fatal(err)
		}
	}
}

func createAPILearning(t *testing.T, st *store.Store, id, repo, fingerprint string) {
	t.Helper()
	if _, err := st.UpsertLearningCandidate(context.Background(), store.UpsertLearningParams{
		ID: id, Repo: repo, Kind: store.LearningPermissionRule, Fingerprint: fingerprint,
		ContentJSON:  `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`,
		BaselineJSON: `{"window_start_ms":1,"window_end_ms":2}`, EvidenceCount: 3,
		ExpiresMs: 3,
	}); err != nil {
		t.Fatal(err)
	}
}
