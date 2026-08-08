package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/danielbecerra/corral/internal/version"
)

// TestVersionMiddleware_HeaderMatrix exercises the handshake in design doc
// §9.1: missing header, non-integer header, and mismatched-version header
// must all be rejected before the wrapped handler ever runs; only the
// exact current version is let through. Every response, including
// rejections, must carry both version headers.
func TestVersionMiddleware_HeaderMatrix(t *testing.T) {
	tests := []struct {
		name       string
		header     string // "" means omit the header entirely
		wantStatus int
		wantCode   Code
		wantReach  bool
	}{
		{name: "missing header", header: "", wantStatus: http.StatusBadRequest, wantCode: CodeVersionMismatch, wantReach: false},
		{name: "non-integer header", header: "banana", wantStatus: http.StatusBadRequest, wantCode: CodeVersionMismatch, wantReach: false},
		{name: "wrong version", header: strconv.Itoa(version.APIVersion + 1), wantStatus: http.StatusBadRequest, wantCode: CodeVersionMismatch, wantReach: false},
		{name: "correct version", header: strconv.Itoa(version.APIVersion), wantStatus: http.StatusOK, wantReach: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
			if tt.header != "" {
				req.Header.Set("Corral-Api-Version", tt.header)
			}
			rec := httptest.NewRecorder()

			versionMiddleware(inner).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if reached != tt.wantReach {
				t.Fatalf("inner handler reached = %v, want %v", reached, tt.wantReach)
			}
			if got := rec.Header().Get("Corral-Api-Version"); got != strconv.Itoa(version.APIVersion) {
				t.Fatalf("response Corral-Api-Version = %q, want %q", got, strconv.Itoa(version.APIVersion))
			}
			if got := rec.Header().Get("Corral-Daemon-Version"); got != version.Version {
				t.Fatalf("response Corral-Daemon-Version = %q, want %q", got, version.Version)
			}
			if !tt.wantReach {
				var env errorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decoding error envelope: %v", err)
				}
				if env.Error.Code != tt.wantCode {
					t.Fatalf("error.code = %q, want %q", env.Error.Code, tt.wantCode)
				}
				if env.Error.Message == "" {
					t.Fatalf("error.message = \"\", want non-empty")
				}
				if env.Error.Details == nil {
					t.Fatalf("error.details = nil, want non-nil (possibly empty) object")
				}
			}
		})
	}
}

// TestWriteError_EnvelopeShape locks down the exact JSON shape design doc
// §9 promises for every non-2xx response, including the nil-details case
// (must still marshal as {} rather than null, so clients need no special
// case for "no details").
func TestWriteError_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusNotFound, CodeSessionNotFound, "no such session", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("Unmarshal top level: %v", err)
	}
	errRaw, ok := raw["error"]
	if !ok {
		t.Fatalf("top-level envelope missing \"error\" key: %s", rec.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(errRaw, &body); err != nil {
		t.Fatalf("Unmarshal error body: %v", err)
	}
	for _, key := range []string{"code", "message", "details"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("error body missing %q key: %s", key, errRaw)
		}
	}
	if string(body["details"]) != "{}" {
		t.Fatalf("error.details = %s, want {} for a nil details map", body["details"])
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("Unmarshal into errorEnvelope: %v", err)
	}
	if env.Error.Code != CodeSessionNotFound || env.Error.Message != "no such session" {
		t.Fatalf("decoded envelope = %+v, want code=%q message=%q", env, CodeSessionNotFound, "no such session")
	}
}
