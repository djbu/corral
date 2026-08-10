package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/store"
)

// heartbeatInterval is how often GET /v1/events/stream writes a
// comment-only ": ping" frame, so idle proxies/load balancers between the
// client and this daemon don't time out a quiet-but-healthy connection.
const heartbeatInterval = 20 * time.Second

// EventsDeps is what handlers_events.go's route needs. daemon.go constructs
// one of these at startup — after wiring d.broker = api.NewBroker() and
// d.store.SetEventPublisher(d.broker) — and passes it to RegisterEvents.
type EventsDeps struct {
	Broker *Broker
	Store  *store.Store
	Clock  clock.Clock
	Log    *slog.Logger
}

// RegisterEvents registers GET /v1/events/stream on s. This route is
// exempted from the version-handshake header requirement in
// versionMiddleware (server.go) and, on the network-facing listener, may
// authenticate via ?token=... instead of an Authorization header
// (middleware_auth.go) — both because a browser EventSource cannot set
// custom request headers. See those two files' doc comments for why
// neither exemption widens the security boundary.
func (s *Server) RegisterEvents(deps EventsDeps) {
	s.Handle("/v1/events/stream", deps.handleStream)
}

// frameJSON is the wire shape of a normal (non-resync) frame's "data:"
// line: {"seq":...,"session_id":...,"ts_ms":...,"kind":...,"data":<raw
// event data>}. Data is embedded as raw JSON rather than a re-escaped
// string, since store.Event.DataJSON is already valid JSON by construction
// (AppendEvent defaults it to "{}" and never accepts anything else).
type frameJSON struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"session_id"`
	TsMs      int64           `json:"ts_ms"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
}

// handleStream serves GET /v1/events/stream: an indefinitely long-lived
// text/event-stream response, one frame per broker-delivered event (or a
// resync marker, or a heartbeat comment), until the client disconnects, the
// broker is closed (daemon shutdown), or a write fails.
//
// Ordering: see broker.go's Frame doc comment — this handler makes no
// attempt to reorder anything itself; it writes frames in the order its
// subscriber channel delivers them, and the dashboard is expected to sort
// by seq.
func (d EventsDeps) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeBadRequest, "GET only", nil)
		return
	}

	log := d.Log
	if log == nil {
		log = slog.Default()
	}

	// Step 34 (m5.md §10): compute the caller's subtree once, up front,
	// before any header is set or byte written — a store error here can
	// still be a clean JSON 500, unlike one raised mid-stream after the
	// text/event-stream headers and first Flush have already committed the
	// response to 200. scopedSessions == nil means "not confined" (admin or
	// no token); a non-nil, possibly-empty set means "confined to exactly
	// this."
	var scopedSessions map[string]struct{}
	if row, ok := TokenFromContext(r.Context()); ok && row.Scope == "session" {
		var err error
		scopedSessions, err = sessionSubtree(r.Context(), d.Store, row.SessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disables response buffering in nginx-as-reverse-proxy deployments;
	// harmless (and unread) everywhere else.
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// Deliberately no explicit w.WriteHeader(http.StatusOK) before this: the
	// first successful Flush performs it implicitly, with the headers set
	// above already attached, and that's what makes the failure branch below
	// reachable at all. If we called WriteHeader(200) ourselves first, a
	// subsequent attempt to send a 500 here would be silently dropped by
	// net/http as a "superfluous WriteHeader" — the client would still see
	// a 200 with an empty body instead of a real error.
	if err := rc.Flush(); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming not supported", nil)
		return
	}

	id, ch, done := d.Broker.Subscribe()
	defer d.Broker.Unsubscribe(id)

	ticker := d.Clock.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if frame.Resync {
				if _, err := w.Write([]byte("event: resync\ndata: {}\n\n")); err != nil {
					return
				}
			} else {
				// Step 34 (m5.md §10): a scope='session' subscriber only
				// receives frames for sessions in its subtree — never a
				// foreign session's, and never a daemon-scoped frame
				// (SessionID == "", e.g. token.unauthorized), which belongs
				// to no session at all. Admin/absent-token subscribers see
				// everything, as before. scopedSessions was computed once,
				// before this loop started, not per frame.
				if scopedSessions != nil {
					if _, allowed := scopedSessions[frame.Event.SessionID]; !allowed {
						continue
					}
				}
				payload, err := json.Marshal(frameJSON{
					Seq:       frame.Event.Seq,
					SessionID: frame.Event.SessionID,
					TsMs:      frame.Event.TsMs,
					Kind:      string(frame.Event.Kind),
					Data:      json.RawMessage(frame.Event.DataJSON),
				})
				if err != nil {
					log.Error("handleStream: marshal frame", "err", err)
					continue
				}
				if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
					return
				}
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-ticker.C():
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
