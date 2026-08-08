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
	s.Handle("GET /v1/sessions/{idOrName}/events", deps.handleEvents)
	s.Handle("DELETE /v1/sessions/{idOrName}", deps.handleDelete)
	s.Handle("POST /v1/sessions/{idOrName}/answer", deps.handleAnswer)
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

	// M2 state fields (§4.5). Stale is a render-time hint that agent_state
	// is "working" but the last hook is older than stale_after — clients
	// render "working?". BlockedReason is the raw stored blocked_reason JSON
	// (§4.3) when agent_state is "blocked", embedded verbatim. PermissionMode
	// is the last permission mode observed from hook payloads (A.8).
	Stale          bool            `json:"stale,omitempty"`
	BlockedReason  json.RawMessage `json:"blocked_reason,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	LastHookAt     *string         `json:"last_hook_at,omitempty"`
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

// toSessionResponse converts a store record to the wire shape. attached
// reflects whether the registry currently has a live Attachment for this
// session (step 10) — callers pass d.Registry.Attached(sess.ID) rather
// than this function reaching into the registry itself, so it stays a
// pure function of its inputs for the tests that call it directly.
func toSessionResponse(sess *session.Session, attached bool) sessionResponse {
	resp := sessionResponse{
		ID:              sess.ID,
		Name:            sess.Name,
		Mode:            string(sess.Mode),
		Cwd:             sess.Cwd,
		Status:          string(sess.Status),
		DesiredState:    string(sess.DesiredState),
		Attached:        attached,
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
	if sess.BlockedReasonJSON != "" {
		resp.BlockedReason = json.RawMessage(sess.BlockedReasonJSON)
	}
	resp.PermissionMode = sess.PermissionMode
	resp.LastHookAt = msPtrToRFC3339Ptr(sess.LastHookAtMs)
	return resp
}

// staleFor reports render-time staleness (§4.5) via the optional Stale
// interface — implemented by the real engine, absent on NoopEngine. The
// type assertion keeps staleness out of the core Engine contract.
func (d SessionsDeps) staleFor(id string) bool {
	if s, ok := d.Engine.(interface{ Stale(string) bool }); ok {
		return s.Stale(id)
	}
	return false
}

func (d SessionsDeps) writeSession(w http.ResponseWriter, status int, sess *session.Session) {
	resp := toSessionResponse(sess, d.Registry.Attached(sess.ID))
	resp.AgentState = string(d.Engine.State(sess.ID))
	resp.Stale = d.staleFor(sess.ID)
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
		resp := toSessionResponse(sess, d.Registry.Attached(sess.ID))
		resp.AgentState = string(d.Engine.State(sess.ID))
		resp.Stale = d.staleFor(sess.ID)
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

// eventResponse is one row of GET /v1/sessions/{idOrName}/events. Data is
// embedded verbatim (never re-encoded) so the wire bytes match the stored
// audit payload exactly — the contract suite (A.7) diffs these.
type eventResponse struct {
	Seq  int64           `json:"seq"`
	Ts   string          `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type listEventsResponse struct {
	Events []eventResponse `json:"events"`
}

// handleEvents serves one session's append-only event stream in
// seq-ascending order (design doc §9.2 / step 6c). It is the audit/replay
// source of truth the fakeclaude-vs-real-claude contract suite reads to
// compare hook traces, and what blocked-reason debugging needs. Cheap: a
// single indexed ListEvents, no derivation.
func (d SessionsDeps) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess, err := d.resolveSession(r, r.PathValue("idOrName"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeSessionNotFound, fmt.Sprintf("no session %q", r.PathValue("idOrName")), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	evs, err := d.Store.ListEvents(r.Context(), sess.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out := make([]eventResponse, 0, len(evs))
	for _, e := range evs {
		data := json.RawMessage(e.DataJSON)
		if len(data) == 0 {
			data = json.RawMessage("{}")
		}
		out = append(out, eventResponse{
			Seq:  e.Seq,
			Ts:   msToRFC3339(e.TsMs),
			Kind: string(e.Kind),
			Data: data,
		})
	}
	writeJSON(w, http.StatusOK, listEventsResponse{Events: out})
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

// maxAnswerBytes caps answerRequest.Text so a runaway client can't wedge a
// session's PTY (or the daemon's memory) with an unbounded write.
const maxAnswerBytes = 64 << 10 // 65536

// answerRequest is POST /v1/sessions/{idOrName}/answer's body (design doc
// §7). Newline is a *bool, not bool, so an omitted field defaults to true
// (append "\r") rather than false — an explicit `"newline": false` is the
// only way to suppress it.
type answerRequest struct {
	Text    string `json:"text"`
	Key     string `json:"key"`
	Newline *bool  `json:"newline"`
}

// handleAnswer writes text or a named key to a live session's PTY master, as
// if a human had typed it (design doc §7). It never touches agent_state: a
// session leaves "blocked" only when a later hook proves it, so the only
// state-machine-visible trace of an answer is the session.answered event
// appended here, best-effort, after the write succeeds.
func (d SessionsDeps) handleAnswer(w http.ResponseWriter, r *http.Request) {
	idOrName := r.PathValue("idOrName")

	var body answerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
		return
	}

	if (body.Text == "") == (body.Key == "") {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "exactly one of text or key must be set", nil)
		return
	}
	if len(body.Text) > maxAnswerBytes {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "answer text too large", nil)
		return
	}
	// newline defaults to true when omitted; it is ignored entirely when Key
	// is set (encodeAnswer never consults it in that branch), so a client
	// that also passes --no-newline alongside --key is not an error.
	newline := true
	if body.Newline != nil {
		newline = *body.Newline
	}

	sess, err := d.resolveSession(r, idOrName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeSessionNotFound, fmt.Sprintf("no session %q", idOrName), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	payload, err := encodeAnswer(body.Text, body.Key, newline)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
		return
	}

	ctx := r.Context()
	if err := d.Registry.WriteInput(ctx, sess.ID, payload); err != nil {
		if errors.Is(err, supervisor.ErrNotLive) {
			writeError(w, http.StatusConflict, CodeSessionNotLive, "session is not live", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}

	// Best-effort audit trail: the write already happened, so a failure here
	// must not turn into an error response (matches the session.created
	// pattern in handleCreate above).
	var data []byte
	var marshalErr error
	if body.Key != "" {
		data, marshalErr = json.Marshal(map[string]string{"key": body.Key})
	} else {
		// Raw byte length of the text, deliberately not len(redact(text)):
		// redacting first would leak whether the text looked secret-shaped
		// through the length alone.
		data, marshalErr = json.Marshal(map[string]int{"len": len(body.Text)})
	}
	if marshalErr == nil {
		if _, err := d.Store.AppendEvent(ctx, sess.ID, session.EventSessionAnswered, string(data)); err != nil {
			_ = err
		}
	}

	d.writeSession(w, http.StatusOK, sess)
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
