package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/supervisor"
)

// attachDeps is what the attach upgrade route needs. It is deliberately
// separate from SessionsDeps (rather than adding fields to it) — Engine
// isn't needed here — but step 34 (m5.md §10) needs Store too: attach runs
// over TCP as well as the unix socket (step 31, remote attach), so a
// scope='session' token must be checked here just like every other
// session-scoped route.
type attachDeps struct {
	Registry *supervisor.Registry
	Store    *store.Store
	Log      *slog.Logger
}

// RegisterAttach registers GET /v1/sessions/{idOrName}/attach (design doc
// §5.1/§9.2): a request that sets Upgrade: corral-attach/1 is hijacked into
// a raw net.Conn and handed to supervisor.Registry.Attach, which then owns
// the connection for the rest of the attachment's life (§5). Any other
// request to this path — missing/wrong Upgrade header, a session that isn't
// live, or (step 34) a scoped token confined away from this session — is
// answered as an ordinary JSON error response, never hijacked.
func (s *Server) RegisterAttach(registry *supervisor.Registry, st *store.Store, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	d := attachDeps{Registry: registry, Store: st, Log: log}
	s.Handle("GET /v1/sessions/{idOrName}/attach", d.handleAttach)
}

func (d attachDeps) handleAttach(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "corral-attach/1" {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			`attach requires "Upgrade: corral-attach/1"`, nil)
		return
	}

	idOrName := r.PathValue("idOrName")
	ls, ok := d.Registry.Get(idOrName)
	if !ok {
		writeError(w, http.StatusNotFound, CodeSessionNotFound, fmt.Sprintf("no live session %q", idOrName), nil)
		return
	}

	allowed, err := tokenAllowsSession(r.Context(), d.Store, ls.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this session", nil)
		return
	}

	rc := http.NewResponseController(w)
	conn, brw, err := rc.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "hijack: "+err.Error(), nil)
		return
	}
	defer conn.Close()

	// Written by hand, directly to the hijacked conn, rather than
	// w.WriteHeader(101) before hijacking: net/http treats 1xx responses
	// as informational and Hijack() itself documents that no response
	// line/headers are sent on your behalf once you've hijacked — so this
	// is the only way the client actually sees the 101 status line.
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: corral-attach/1\r\nConnection: Upgrade\r\n\r\n")); err != nil {
		d.Log.Warn("api: attach: writing 101 response", "session_id", ls.SessionID, "err", err)
		return
	}

	// brw.Reader may already hold bytes the client sent right after its
	// own request (pipelined ahead of the 101) — reads must go through it,
	// never a fresh bufio.NewReader(conn), or those bytes are lost.
	br := brw.Reader
	if err := d.Registry.Attach(ls, conn, br); err != nil {
		d.Log.Warn("api: attach: session ended", "session_id", ls.SessionID, "err", err)
	}
}
