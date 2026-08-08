package hookrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unixServer starts an httptest.Server listening on a fresh unix socket
// (rather than httptest's default tcp listener), returning the socket
// path. It is closed automatically via t.Cleanup. The socket lives under
// os.TempDir() with a short suffix, not t.TempDir(): test-name-qualified
// paths blow past AF_UNIX's ~104-byte sun_path limit on darwin (same
// workaround as internal/daemon's shortTempDir).
func unixServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "corral-hr-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listening on %s: %v", sockPath, err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return sockPath
}

func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// TestDeliver_ForwardsBodyVerbatim asserts the exact bytes given to Deliver
// reach the server unmodified, with every header design doc §2.6 requires.
func TestDeliver_ForwardsBodyVerbatim(t *testing.T) {
	body := []byte(`{"hook_event_name":"Stop","session_id":"abc"}`)
	var gotBody []byte
	var gotHeaders http.Header

	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = readAll(r)
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"decision":null}`))
	})

	decision, err := Deliver(context.Background(), sock, "sess-1", "secret-1", "Stop", "delivery-1", body, time.Second)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(decision) != 0 {
		t.Fatalf("decision = %q, want none", decision)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("server received body %q, want %q", gotBody, body)
	}
	if gotHeaders.Get("Corral-Api-Version") != "1" {
		t.Errorf("Corral-Api-Version = %q, want 1", gotHeaders.Get("Corral-Api-Version"))
	}
	if gotHeaders.Get("Corral-Session-Id") != "sess-1" {
		t.Errorf("Corral-Session-Id = %q, want sess-1", gotHeaders.Get("Corral-Session-Id"))
	}
	if gotHeaders.Get("Corral-Session-Secret") != "secret-1" {
		t.Errorf("Corral-Session-Secret = %q, want secret-1", gotHeaders.Get("Corral-Session-Secret"))
	}
	if gotHeaders.Get("Corral-Hook-Event") != "Stop" {
		t.Errorf("Corral-Hook-Event = %q, want Stop", gotHeaders.Get("Corral-Hook-Event"))
	}
	if gotHeaders.Get("Corral-Delivery-Id") != "delivery-1" {
		t.Errorf("Corral-Delivery-Id = %q, want delivery-1", gotHeaders.Get("Corral-Delivery-Id"))
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotHeaders.Get("Content-Type"))
	}
}

func readAll(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// TestDeliver_NonOKStatus asserts a non-2xx response is a Deliver error
// (the caller — Run — turns this into a single stderr line and still exits
// 0, but that's Run's job, not Deliver's).
func TestDeliver_NonOKStatus(t *testing.T) {
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	})

	_, err := Deliver(context.Background(), sock, "sess-1", "secret-1", "Stop", "delivery-1", []byte(`{}`), time.Second)
	if err == nil {
		t.Fatal("Deliver: want error for 500 response, got nil")
	}
}

// TestDeliver_RefusedSocket asserts dialing a socket nothing is listening
// on is a Deliver error, not a panic.
func TestDeliver_RefusedSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nothing-listening.sock")
	_, err := Deliver(context.Background(), sock, "sess-1", "secret-1", "Stop", "delivery-1", []byte(`{}`), time.Second)
	if err == nil {
		t.Fatal("Deliver: want error for refused connection, got nil")
	}
}

// TestDeliver_Timeout asserts a server that never responds within the
// given timeout is a Deliver error, not a hang.
func TestDeliver_Timeout(t *testing.T) {
	block := make(chan struct{})

	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})

	// Registered AFTER unixServer, so LIFO cleanup closes block BEFORE
	// unixServer's server.Close runs. Otherwise server.Close waits on the
	// still-blocked handler and never returns, wedging the whole package
	// until the test binary's global timeout.
	t.Cleanup(func() { close(block) })

	start := time.Now()
	_, err := Deliver(context.Background(), sock, "sess-1", "secret-1", "Stop", "delivery-1", []byte(`{}`), 100*time.Millisecond)
	if err == nil {
		t.Fatal("Deliver: want error for timeout, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Deliver took %v, want it to respect the ~100ms deadline", elapsed)
	}
}

// TestDeliver_GarbageResponseBody asserts a 2xx response whose body isn't
// valid JSON is a Deliver error rather than a panic or a silently-ignored
// decision.
func TestDeliver_GarbageResponseBody(t *testing.T) {
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json at all {{{`))
	})

	_, err := Deliver(context.Background(), sock, "sess-1", "secret-1", "Stop", "delivery-1", []byte(`{}`), time.Second)
	if err == nil {
		t.Fatal("Deliver: want error for garbage response body, got nil")
	}
}

// TestRelayForwardsDecisionToStdout is the dedicated response-seam test
// (design doc §2.5): a canned non-null decision from the daemon must be
// printed to stdout verbatim by Run. M2's real daemon never sends one, but
// the seam must already work end to end so a future milestone's
// auto-answer engine is a daemon-only change.
func TestRelayForwardsDecisionToStdout(t *testing.T) {
	canned := `{"systemMessage":"approved by policy"}`
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"decision":%s}`, canned)
	})

	var stdout, stderr bytes.Buffer
	env := envMap(map[string]string{
		"CORRAL_SOCK":           sock,
		"CORRAL_SESSION_ID":     "sess-1",
		"CORRAL_SESSION_SECRET": "secret-1",
	})
	Run(context.Background(), "Stop", strings.NewReader(`{"hook_event_name":"Stop"}`), &stdout, &stderr, env)

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var got, want json.RawMessage = json.RawMessage(stdout.Bytes()), json.RawMessage(canned)
	if !jsonEqual(t, got, want) {
		t.Fatalf("stdout = %q, want decision %q", stdout.String(), canned)
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshaling %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshaling %q: %v", b, err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

// TestRun_MissingEnvIsSilent asserts that any of the three required env
// vars being absent produces zero output on both streams and never
// attempts a delivery at all (design doc §2.4 step 2).
func TestRun_MissingEnvIsSilent(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"all missing", map[string]string{}},
		{"missing sock", map[string]string{"CORRAL_SESSION_ID": "s", "CORRAL_SESSION_SECRET": "x"}},
		{"missing session id", map[string]string{"CORRAL_SOCK": "/tmp/nope.sock", "CORRAL_SESSION_SECRET": "x"}},
		{"missing secret", map[string]string{"CORRAL_SOCK": "/tmp/nope.sock", "CORRAL_SESSION_ID": "s"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			Run(context.Background(), "Stop", strings.NewReader(`{}`), &stdout, &stderr, envMap(tt.env))
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

// TestRun_RefusedSocketWritesOneStderrLine asserts Run degrades to exactly
// one stderr line (and empty stdout) when delivery fails outright — the
// CLI layer still exits 0 regardless (that policy lives in
// cmd_hook_relay.go, not here).
func TestRun_RefusedSocketWritesOneStderrLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := envMap(map[string]string{
		"CORRAL_SOCK":           filepath.Join(t.TempDir(), "nothing.sock"),
		"CORRAL_SESSION_ID":     "sess-1",
		"CORRAL_SESSION_SECRET": "secret-1",
	})
	Run(context.Background(), "Stop", strings.NewReader(`{}`), &stdout, &stderr, env)

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	lines := strings.Count(strings.TrimRight(stderr.String(), "\n"), "\n") + 1
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want one line naming the failure")
	}
	if lines != 1 {
		t.Errorf("stderr has %d lines, want exactly 1: %q", lines, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "corral hook-relay: ") {
		t.Errorf("stderr = %q, want prefix %q", stderr.String(), "corral hook-relay: ")
	}
}

// TestRun_500WritesOneStderrLine covers the non-2xx-status branch of the
// same "any failure -> one stderr line" contract.
func TestRun_500WritesOneStderrLine(t *testing.T) {
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	var stdout, stderr bytes.Buffer
	env := envMap(map[string]string{
		"CORRAL_SOCK":           sock,
		"CORRAL_SESSION_ID":     "sess-1",
		"CORRAL_SESSION_SECRET": "secret-1",
	})
	Run(context.Background(), "Stop", strings.NewReader(`{}`), &stdout, &stderr, env)

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want one line naming the failure")
	}
}

// TestRun_TimeoutWritesOneStderrLine covers the deadline-exceeded branch,
// using CORRAL_HOOK_TIMEOUT to keep the test fast.
func TestRun_TimeoutWritesOneStderrLine(t *testing.T) {
	block := make(chan struct{})
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	// Registered AFTER unixServer, so LIFO cleanup closes block BEFORE
	// unixServer's server.Close runs. Otherwise server.Close waits on the
	// still-blocked handler and never returns, wedging the whole package
	// until the test binary's global timeout.
	t.Cleanup(func() { close(block) })
	var stdout, stderr bytes.Buffer
	env := envMap(map[string]string{
		"CORRAL_SOCK":           sock,
		"CORRAL_SESSION_ID":     "sess-1",
		"CORRAL_SESSION_SECRET": "secret-1",
		"CORRAL_HOOK_TIMEOUT":   "100ms",
	})
	start := time.Now()
	Run(context.Background(), "Stop", strings.NewReader(`{}`), &stdout, &stderr, env)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %v, want it to respect CORRAL_HOOK_TIMEOUT", elapsed)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want one line naming the timeout")
	}
}

// TestRun_GarbageResponseWritesOneStderrLine covers the undecodable-body
// branch.
func TestRun_GarbageResponseWritesOneStderrLine(t *testing.T) {
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{{{not json`))
	})
	var stdout, stderr bytes.Buffer
	env := envMap(map[string]string{
		"CORRAL_SOCK":           sock,
		"CORRAL_SESSION_ID":     "sess-1",
		"CORRAL_SESSION_SECRET": "secret-1",
	})
	Run(context.Background(), "Stop", strings.NewReader(`{}`), &stdout, &stderr, env)

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want one line naming the failure")
	}
}

// TestRun_NullDecisionPrintsNothing exercises the M2-real-world path: the
// daemon responds {"decision": null}, and Run must print nothing at all.
func TestRun_NullDecisionPrintsNothing(t *testing.T) {
	sock := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"decision":null}`))
	})
	var stdout, stderr bytes.Buffer
	env := envMap(map[string]string{
		"CORRAL_SOCK":           sock,
		"CORRAL_SESSION_ID":     "sess-1",
		"CORRAL_SESSION_SECRET": "secret-1",
	})
	Run(context.Background(), "Stop", strings.NewReader(`{}`), &stdout, &stderr, env)

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestReadStdin_Truncates asserts input longer than MaxStdinBytes is
// truncated, not rejected.
func TestReadStdin_Truncates(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxStdinBytes+100)
	data, truncated, err := ReadStdin(bytes.NewReader(big))
	if err != nil {
		t.Fatalf("ReadStdin: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
	if len(data) != MaxStdinBytes {
		t.Errorf("len(data) = %d, want %d", len(data), MaxStdinBytes)
	}
}

// TestReadStdin_UnderCap asserts input under the cap round-trips exactly
// and is not flagged truncated.
func TestReadStdin_UnderCap(t *testing.T) {
	small := []byte(`{"hook_event_name":"Stop"}`)
	data, truncated, err := ReadStdin(bytes.NewReader(small))
	if err != nil {
		t.Fatalf("ReadStdin: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
	if !bytes.Equal(data, small) {
		t.Errorf("data = %q, want %q", data, small)
	}
}
