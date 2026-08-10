package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/djbu/corral/internal/hookrelay"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
)

// maxHookBodyBytes is the hard cap on a single hook payload's body (design
// doc §2.6/Amendment): relay.go's own stdin cap is 1 MiB, so the daemon
// enforces the same limit against a delivery it does not otherwise trust.
const maxHookBodyBytes = 1 << 20

// secretLooker is the subset of *supervisor.Registry handleHookEvent needs
// to authenticate a delivery. Declared as an interface (rather than
// HooksDeps holding a concrete *supervisor.Registry) purely so tests can
// inject a fake live-session/secret map without spawning a real process —
// supervisor.Registry has no other test seam for adding a live session.
type secretLooker interface {
	SecretFor(sessionID string) (secret string, ok bool)
}

// HooksDeps is what handlers_hooks.go's route needs. daemon.go constructs
// one at startup and passes it to RegisterHooks.
type HooksDeps struct {
	Store   *store.Store
	Engine  state.Engine
	Secrets secretLooker
}

// RegisterHooks registers POST /v1/hooks/events on s (design doc §2.6):
// the daemon-side endpoint `corral hook-relay` forwards every Claude Code
// hook invocation to.
func (s *Server) RegisterHooks(deps HooksDeps) {
	s.Handle("POST /v1/hooks/events", deps.handleHookEvent)
}

// handleHookEvent authenticates a hook delivery against the target
// session's in-memory secret, decodes it, and hands it to the Engine —
// never returning anything but 200 (or the auth/body-cap failures below) so
// a hook-ingest problem on corral's side can never perturb the supervised
// agent (design doc §2.6: "corral must never perturb the agent").
func (d HooksDeps) handleHookEvent(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Corral-Session-Id")
	secret := r.Header.Get("Corral-Session-Secret")
	eventName := r.Header.Get("Corral-Hook-Event")

	known, ok := d.Secrets.SecretFor(sessionID)
	// Always run the constant-time compare, even when the session is
	// unknown (known == "" in that case), so the 403's timing cannot be
	// used to distinguish "no such session" from "wrong secret" — the
	// response body below is likewise byte-identical in both cases.
	match := subtle.ConstantTimeCompare([]byte(secret), []byte(known)) == 1
	authed := ok && match
	if !authed {
		d.appendHookEvent(r.Context(), sessionID, session.EventHookUnauthorized, eventName)
		writeError(w, http.StatusForbidden, CodeUnauthorized, "unauthorized", nil)
		return
	}

	limited := http.MaxBytesReader(w, r.Body, maxHookBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeBadRequest, "hook payload exceeds 1 MiB", nil)
			return
		}
		// Any other read error (e.g. the client hung up mid-body): treat
		// like an undecodable payload rather than erroring — there is
		// nothing about this that is corral's own bug.
		body = []byte{}
	}

	payload, derr := hookrelay.Decode(body)
	if derr != nil {
		d.appendHookEvent(r.Context(), sessionID, session.EventHookUndecodable, eventName)
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	if err := d.Engine.OnHookEvent(r.Context(), sessionID, payload); err != nil {
		// Whether it's the documented ErrQueueFull or anything else, ingest
		// failures are recorded as dropped and never surfaced as a non-2xx
		// to the relay/hook process.
		d.appendHookEvent(r.Context(), sessionID, session.EventHookDropped, eventName)
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	d.appendHookEvent(r.Context(), sessionID, session.EventHookReceived, eventName)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// appendHookEvent records kind for sessionID with a minimal
// {"event":"<eventName>"} data payload. Best-effort: an AppendEvent failure
// is ignored, since a bookkeeping write must never turn into a hook-ingest
// failure the relay/agent would observe.
func (d HooksDeps) appendHookEvent(ctx context.Context, sessionID string, kind session.EventKind, eventName string) {
	payload := "{}"
	if b, err := json.Marshal(map[string]string{"event": eventName}); err == nil {
		payload = string(b)
	}
	_, _ = d.Store.AppendEvent(ctx, sessionID, kind, payload)
}
