package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/danielbecerra/corral/internal/config"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
)

// SessionsDeps is what handlers_sessions.go's routes need. daemon.go
// constructs one of these at startup (once the registry exists) and passes
// it to RegisterSessions.
type SessionsDeps struct {
	Store    *store.Store
	Engine   state.Engine
	Registry *supervisor.Registry
}

// RegisterSessions registers GET/POST /v1/sessions and GET/DELETE
// /v1/sessions/{idOrName} on s (design doc §9.2). The attach upgrade at
// GET /v1/sessions/{idOrName}/attach is step 10's.
func (s *Server) RegisterSessions(deps SessionsDeps) {
	s.Handle("GET /v1/sessions", deps.handleList)
	s.Handle("POST /v1/sessions", deps.handleCreate)
	s.Handle("GET /v1/sessions/{idOrName}", deps.handleGet)
	s.Handle("DELETE /v1/sessions/{idOrName}", deps.handleDelete)
}

// sessionResponse is design doc §9.2's exact <Session> JSON shape.
type sessionResponse struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Mode            string  `json:"mode"`
	Cwd             string  `json:"cwd"`
	Status          string  `json:"status"`
	DesiredState    string  `json:"desired_state"`
	AgentState      string  `json:"agent_state"`
	Attached        bool    `json:"attached"`
	PID             int     `json:"pid"`
	Model           string  `json:"model"`
	ClaudeSessionID string  `json:"claude_session_id"`
	Rows            int     `json:"rows"`
	Cols            int     `json:"cols"`
	ResumeCount     int     `json:"resume_count"`
	CreatedAt       string  `json:"created_at"`
	StartedAt       *string `json:"started_at"`
	LastAttachedAt  *string `json:"last_attached_at"`
	EndedAt         *string `json:"ended_at"`
	ExitCode        *int    `json:"exit_code"`
	ExitSignal      *string `json:"exit_signal"`
}

func msToRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func msPtrToRFC3339Ptr(ms *int64) *string {
	if ms == nil {
		return nil
	}
	s := msToRFC3339(*ms)
	return &s
}

// toSessionResponse converts a store record to the wire shape. attached is
// always false in M1 (step 10 owns the attach protocol that would make it
// meaningfully true).
func toSessionResponse(sess *session.Session) sessionResponse {
	resp := sessionResponse{
		ID:              sess.ID,
		Name:            sess.Name,
		Mode:            string(sess.Mode),
		Cwd:             sess.Cwd,
		Status:          string(sess.Status),
		DesiredState:    string(sess.DesiredState),
		Attached:        false,
		PID:             sess.PID,
		Model:           sess.Model,
		ClaudeSessionID: sess.ClaudeSessionID,
		Rows:            sess.Rows,
		Cols:            sess.Cols,
		ResumeCount:     sess.ResumeCount,
		CreatedAt:       msToRFC3339(sess.CreatedAtMs),
		StartedAt:       msPtrToRFC3339Ptr(sess.StartedAtMs),
		LastAttachedAt:  msPtrToRFC3339Ptr(sess.LastAttachedAtMs),
		EndedAt:         msPtrToRFC3339Ptr(sess.EndedAtMs),
		ExitCode:        sess.ExitCode,
	}
	if sess.ExitSignal != "" {
		resp.ExitSignal = &sess.ExitSignal
	}
	return resp
}

func (d SessionsDeps) writeSession(w http.ResponseWriter, status int, sess *session.Session) {
	resp := toSessionResponse(sess)
	resp.AgentState = string(d.Engine.State(sess.ID))
	writeJSON(w, status, resp)
}

// resolveSession looks up idOrName first as an ID, then as a name.
func (d SessionsDeps) resolveSession(r *http.Request, idOrName string) (*session.Session, error) {
	sess, err := d.Store.GetSession(r.Context(), idOrName)
	if err == nil {
		return sess, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return d.Store.GetSessionByName(r.Context(), idOrName)
}

type listSessionsResponse struct {
	Sessions []sessionResponse `json:"sessions"`
}

func (d SessionsDeps) handleList(w http.ResponseWriter, r *http.Request) {
	sessions, err := d.Store.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out := make([]sessionResponse, 0, len(sessions))
	for _, sess := range sessions {
		resp := toSessionResponse(sess)
		resp.AgentState = string(d.Engine.State(sess.ID))
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, listSessionsResponse{Sessions: out})
}

func (d SessionsDeps) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, err := d.resolveSession(r, r.PathValue("idOrName"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeSessionNotFound, fmt.Sprintf("no session %q", r.PathValue("idOrName")), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	d.writeSession(w, http.StatusOK, sess)
}

type createSessionRequest struct {
	Name  string `json:"name"`
	Cwd   string `json:"cwd"`
	Mode  string `json:"mode"`
	Model string `json:"model"`
	Rows  uint16 `json:"rows"`
	Cols  uint16 `json:"cols"`
}

func (d SessionsDeps) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
		return
	}

	if body.Cwd == "" || !filepath.IsAbs(body.Cwd) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("cwd %q must be an absolute path", body.Cwd), nil)
		return
	}
	if info, err := statDir(body.Cwd); err != nil || !info {
		writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("cwd %q does not exist or is not a directory", body.Cwd), nil)
		return
	}

	mode := session.ModeInteractive
	if body.Mode != "" && body.Mode != string(session.ModeInteractive) {
		writeError(w, http.StatusBadRequest, CodeUnsupportedMode, fmt.Sprintf("mode %q is not supported in M1 (only %q)", body.Mode, session.ModeInteractive), nil)
		return
	}

	ctx := r.Context()

	name := body.Name
	if name == "" {
		var err error
		name, err = defaultSessionName(ctx, d.Store, body.Cwd)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
			return
		}
	} else if _, err := d.Store.GetSessionByName(ctx, name); err == nil {
		writeError(w, http.StatusConflict, CodeSessionNameTaken, fmt.Sprintf("session name %q is already in use", name), nil)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	sessCfg, _, _, _, err := config.LoadSession(body.Cwd, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	claudeBin := sessCfg.ClaudeBin
	if !filepath.IsAbs(claudeBin) {
		resolved, err := exec.LookPath(claudeBin)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, fmt.Sprintf("resolving claude_bin %q: %v", claudeBin, err), nil)
			return
		}
		claudeBin = resolved
	}
	model := body.Model
	if model == "" {
		model = sessCfg.Model
	}

	id := newSessionID()
	_, err = d.Store.CreateSession(ctx, store.CreateSessionParams{
		ID:             id,
		Name:           name,
		Mode:           mode,
		Cwd:            body.Cwd,
		ClaudeBin:      claudeBin,
		Model:          model,
		SettingsPath:   "", // filled in by Registry.Spawn once settings.Pin runs
		SettingSources: sessCfg.SettingSources,
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusStarting,
		PID:            0,
		PGID:           0,
		ProcStartNs:    0,
		Rows:           int(body.Rows),
		Cols:           int(body.Cols),
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicateName) {
			writeError(w, http.StatusConflict, CodeSessionNameTaken, err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	if _, err := d.Store.AppendEvent(ctx, id, session.EventSessionCreated, "{}"); err != nil {
		// Non-fatal: the session row exists and spawning still proceeds.
		_ = err
	}

	spec := session.Spec{
		ID:             id,
		Name:           name,
		Mode:           mode,
		Cwd:            body.Cwd,
		ClaudeBin:      claudeBin,
		Model:          model,
		SettingSources: sessCfg.SettingSources,
		Rows:           body.Rows,
		Cols:           body.Cols,
	}
	updated, err := d.Registry.Spawn(ctx, spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	d.writeSession(w, http.StatusCreated, updated)
}

func (d SessionsDeps) handleDelete(w http.ResponseWriter, r *http.Request) {
	idOrName := r.PathValue("idOrName")
	sess, err := d.resolveSession(r, idOrName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeSessionNotFound, fmt.Sprintf("no session %q", idOrName), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	ctx := r.Context()
	var grace *time.Duration
	if g := r.URL.Query().Get("grace"); g != "" {
		parsed, err := time.ParseDuration(g)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid grace duration: "+err.Error(), nil)
			return
		}
		grace = &parsed
	}

	updated, err := d.Registry.Kill(ctx, sess.ID, grace)
	if errors.Is(err, supervisor.ErrNotLive) {
		// Already not running (e.g. exited on its own, or a previous kill):
		// killing is then just recording intent, since there is nothing
		// left to signal.
		updated, err = d.Store.UpdateSession(ctx, sess.ID, func(s *session.Session) {
			s.DesiredState = session.DesiredStopped
		})
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	d.writeSession(w, http.StatusOK, updated)
}

// defaultSessionName implements design doc §9.2's "<basename(cwd)>-<n>"
// default, picking the smallest n >= 1 not already in use.
func defaultSessionName(ctx context.Context, st *store.Store, cwd string) (string, error) {
	base := filepath.Base(cwd)
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s-%d", base, n)
		_, err := st.GetSessionByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// newSessionID returns a fresh uuidv4 string — session.Spec.ID's doc
// comment ("uuidv4, generated by corral") and design doc §7.1.
func newSessionID() string {
	return uuid.New().String()
}

// statDir reports whether path exists and is a directory.
func statDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}
