package api

import (
	"fmt"
	"net/http"
	"strconv"

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
