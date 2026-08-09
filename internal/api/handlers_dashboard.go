package api

import (
	"net/http"

	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// DashboardDeps is what handlers_dashboard.go's route needs. daemon.go
// constructs one at startup, reusing the exact same Store/Engine/Registry
// values already passed to RegisterSessions/RegisterDags, and passes it to
// RegisterDashboard (m5.md §13 step 33).
type DashboardDeps struct {
	Store    *store.Store
	Engine   state.Engine
	Registry *supervisor.Registry
}

// RegisterDashboard registers GET /v1/dashboard on s. It is a plain /v1/
// route like any other: the version-handshake and (on the TCP listener)
// bearer-auth middleware apply to it automatically, with nothing
// dashboard-specific about its wiring. The unauthenticated static shell
// that fetches this endpoint from a browser lives in package
// internal/api/dashboard and is wired separately, in
// Server.AuthenticatedHandler.
func (s *Server) RegisterDashboard(deps DashboardDeps) {
	s.Handle("/v1/dashboard", deps.handleSnapshot)
}

// dashboardResponse is the body of GET /v1/dashboard: everything the
// browser dashboard's initial render needs in one round trip. DAG task
// detail is deliberately NOT included here — the SPA fetches GET
// /v1/dags/{id} on demand only once an operator opens a specific dag, so
// this snapshot stays cheap regardless of how many tasks a dag has.
type dashboardResponse struct {
	Sessions []sessionResponse    `json:"sessions"`
	Dags     []dagSummaryResponse `json:"dags"`
}

// handleSnapshot serves GET /v1/dashboard. Its session list MUST be built
// by the identical enrichment path GET /v1/sessions uses (buildSessionResponses,
// handlers_sessions.go) — the dashboard's blocked-card feature keys
// entirely off agent_state == "blocked", and a second, divergent way of
// computing that field would let the dashboard silently disagree with
// `corral ls` on the one field that matters. Likewise its dag list is
// dagSummaries (handlers_dags.go), the same slice GET /v1/dags produces.
func (d DashboardDeps) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeBadRequest, "GET only", nil)
		return
	}

	ctx := r.Context()

	sessions, err := buildSessionResponses(ctx, d.Store, d.Engine, d.Registry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	dags, err := dagSummaries(ctx, d.Store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, dashboardResponse{Sessions: sessions, Dags: dags})
}
