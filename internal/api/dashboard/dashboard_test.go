package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ServesIndexAtRoot(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html prefix", ct)
	}
	if !strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatalf("body missing #app root container: %s", rec.Body.String())
	}
}

func TestHandler_ServesAppJS(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/app.js", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("Content-Type = %q, want text/javascript prefix", ct)
	}
}

func TestHandler_ServesAppCSS(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/app.css", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css prefix", ct)
	}
}

// TestHandler_ExactPathGuard proves the "/" registration doesn't fall back
// to serving index.html for arbitrary unregistered paths — only the exact
// path "/" does — and that no path beyond the three embedded assets is
// ever served (the HARD CONSTRAINT in dashboard.go's doc comment: "do not
// add favicon or any other path").
func TestHandler_ExactPathGuard(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/favicon.ico", "/index.html", "/robots.txt"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, rec.Code)
		}
	}
}

// TestHandler_DashboardFileServerCleansTraversal proves
// http.FileServerFS's traversal cleaning applies: a path-traversal attempt
// under /dashboard/ never escapes the embedded three-file asset set.
func TestHandler_DashboardFileServerCleansTraversal(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/../server.go", nil)
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("GET /dashboard/../server.go: status = 200, want traversal blocked")
	}
}
