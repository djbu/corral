package api

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/version"
)

// MetaDeps is what handlers_meta.go's three routes need. daemon.go
// constructs one of these at startup and passes it to RegisterMeta.
type MetaDeps struct {
	Store     *store.Store
	Clock     clock.Clock
	StartedAt time.Time
	// DefaultGrace is used for POST /v1/daemon/shutdown when the request
	// body omits "grace".
	DefaultGrace time.Duration
	// RequestShutdown is called once the handler has validated and
	// accepted a shutdown request; it must not block — signals.go/
	// daemon.go's main select loop is the actual place shutdown happens.
	RequestShutdown func(grace time.Duration)
}

// RegisterMeta registers GET /v1/version, GET /v1/config, and POST
// /v1/daemon/shutdown on s.
func (s *Server) RegisterMeta(deps MetaDeps) {
	s.Handle("/v1/version", deps.handleVersion)
	s.Handle("/v1/config", deps.handleConfig)
	s.Handle("/v1/daemon/shutdown", deps.handleShutdown)
}

type versionResponse struct {
	DaemonVersion string `json:"daemon_version"`
	APIVersion    int    `json:"api_version"`
	SchemaVersion int    `json:"schema_version"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"`
	UptimeMs      int64  `json:"uptime_ms"`
}

func (d MetaDeps) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeBadRequest, "GET only", nil)
		return
	}

	schemaVersion, err := d.Store.SchemaVersion(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	now := d.Clock.Now()
	writeJSON(w, http.StatusOK, versionResponse{
		DaemonVersion: version.Version,
		APIVersion:    version.APIVersion,
		SchemaVersion: schemaVersion,
		PID:           os.Getpid(),
		StartedAt:     d.StartedAt.Format(time.RFC3339),
		UptimeMs:      now.Sub(d.StartedAt).Milliseconds(),
	})
}

type configResponse struct {
	Values  map[string]string   `json:"values"`
	Sources map[string]string   `json:"sources"`
	Ignored []configIgnoredItem `json:"ignored"`
}

type configIgnoredItem struct {
	File string `json:"file"`
	Key  string `json:"key"`
}

func (d MetaDeps) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeBadRequest, "GET only", nil)
		return
	}

	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
			return
		}
	}

	eff, err := config.Effective(cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
		return
	}

	ignored := make([]configIgnoredItem, 0, len(eff.Ignored))
	for _, r := range eff.Ignored {
		ignored = append(ignored, configIgnoredItem{File: r.File, Key: r.Key})
	}

	writeJSON(w, http.StatusOK, configResponse{Values: eff.Values, Sources: eff.Sources, Ignored: ignored})
}

type shutdownRequest struct {
	Grace string `json:"grace"`
}

type shutdownResponse struct {
	Accepted bool `json:"accepted"`
}

func (d MetaDeps) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeBadRequest, "POST only", nil)
		return
	}

	grace := d.DefaultGrace
	if r.Body != nil {
		var body shutdownRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err == nil && body.Grace != "" {
			parsed, err := time.ParseDuration(body.Grace)
			if err != nil {
				writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid grace duration: "+err.Error(), nil)
				return
			}
			grace = parsed
		}
	}

	writeJSON(w, http.StatusAccepted, shutdownResponse{Accepted: true})

	if d.RequestShutdown != nil {
		d.RequestShutdown(grace)
	}
}
