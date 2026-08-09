package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielbecerra/corral/internal/version"
)

// Server is corral's HTTP API surface: a ServeMux under /v1/, wrapped in
// the version-handshake middleware (design doc §9.1). It is deliberately
// small in step 8 — /v1/version, /v1/config, /v1/daemon/shutdown — since
// /v1/sessions and the attach upgrade land in steps 9/10.
type Server struct {
	mux *http.ServeMux
}

// New builds a Server with no routes registered; callers (daemon.go) call
// the handler-registration methods (RegisterMeta, and later
// RegisterSessions/RegisterAttach) before calling Handler.
func New() *Server {
	return &Server{mux: http.NewServeMux()}
}

// Handle registers a handler for pattern on the underlying mux. Handler
// files (handlers_meta.go, and later handlers_sessions.go/attach.go) call
// this rather than reaching into Server's internals directly.
func (s *Server) Handle(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
}

// Handler returns the complete http.Handler to serve, with the
// version-handshake middleware applied around every registered route.
func (s *Server) Handler() http.Handler {
	return versionMiddleware(s.mux)
}

// AuthenticatedHandler returns the complete http.Handler for a
// network-facing (TCP) listener (design doc §4/§6, step 30): bearer-auth
// wraps versionMiddleware, not the other way around, so an unauthenticated
// remote caller learns nothing — not even whether its API version
// matches — before proving a token. The unix-socket Handler() above is
// unchanged and stays token-free: the socket's 0600 permission is that
// transport's auth boundary (design doc §4).
//
// shell (step 33, m5.md §13) is the dashboard's static, unauthenticated
// asset shell — non-secret client code only, never session data — and is
// deliberately taken as a parameter rather than stored on Server: that
// keeps it structurally impossible to ever attach to the unix Handler()
// above, which has no such parameter to accept it. shell may be nil, in
// which case AuthenticatedHandler behaves exactly as before (no
// unauthenticated surface at all).
//
// When shell is non-nil, an explicit allowlist of exactly two path shapes
// — "/" and anything under "/dashboard/" (note the trailing slash: bare
// "/dashboard" does NOT match) — is routed to shell before auth ever runs;
// every other path, including unknown ones, falls through to the bearer
// wall. This is deliberately phrased as an allowlist with auth as the
// default, never as "authenticate /v1/, else serve files": that inverted
// phrasing would make any future new route unauthenticated by default.
// "/v1/dashboard" (the snapshot endpoint, handlers_dashboard.go) does not
// start with "/dashboard/", so it never collides with this allowlist.
func (s *Server) AuthenticatedHandler(auth tokenAuthStore, log *slog.Logger, shell http.Handler) http.Handler {
	authed := bearerAuth(versionMiddleware(s.mux), auth, log)
	if shell == nil {
		return authed
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/dashboard/") {
			shell.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r) // EVERYTHING else — including unknown paths — hits the wall
	})
}

// versionMiddleware enforces the handshake in design doc §9.1: every
// request must send Corral-Api-Version as an integer, and that integer
// must equal version.APIVersion, or the request is rejected with 400
// version_mismatch before reaching any handler. Every response — including
// the rejection itself — carries Corral-Api-Version and
// Corral-Daemon-Version, so a client can always learn what the daemon
// speaks even from an error.
func versionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
		w.Header().Set("Corral-Daemon-Version", version.Version)

		// Step 32: browser EventSource cannot set custom request headers, so
		// GET /v1/events/stream alone is exempt from the handshake — its
		// version is instead settled by these two response headers, already
		// set above. This is auth-safe, not an auth bypass: on the
		// network-facing (TCP) listener this middleware only runs inside
		// bearerAuth (Server.AuthenticatedHandler wraps versionMiddleware,
		// not the reverse), so an unauthenticated caller still never reaches
		// this handler at all.
		if r.URL.Path == "/v1/events/stream" {
			next.ServeHTTP(w, r)
			return
		}

		raw := r.Header.Get("Corral-Api-Version")
		if raw == "" {
			writeError(w, http.StatusBadRequest, CodeVersionMismatch,
				"missing Corral-Api-Version header; this daemon speaks API version "+strconv.Itoa(version.APIVersion), nil)
			return
		}
		clientVersion, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeVersionMismatch,
				fmt.Sprintf("Corral-Api-Version %q is not an integer; this daemon speaks API version %d", raw, version.APIVersion), nil)
			return
		}
		if clientVersion != version.APIVersion {
			writeError(w, http.StatusBadRequest, CodeVersionMismatch,
				fmt.Sprintf("client speaks API version %d, this daemon speaks %d", clientVersion, version.APIVersion), nil)
			return
		}

		next.ServeHTTP(w, r)
	})
}
