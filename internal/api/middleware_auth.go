package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielbecerra/corral/internal/apitoken"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

// tokenAuthStore is the subset of *store.Store bearerAuth needs to
// authenticate a bearer token. Declared as an interface (rather than
// bearerAuth taking a *store.Store directly) purely so tests can inject a
// fake that records AppendEvent calls and signals TouchToken on a channel
// without racing a real background write — the same test-seam idiom as
// secretLooker in handlers_hooks.go.
type tokenAuthStore interface {
	VerifyToken(ctx context.Context, plaintext string) (store.TokenRow, bool, error)
	TouchToken(ctx context.Context, id string)
	AppendEvent(ctx context.Context, sessionID string, kind session.EventKind, dataJSON string) (*session.Event, error)
}

// dummyTokenHash is a fixed, never-matching SHA-256 hex digest (of the
// literal string "corral-bearer-auth-dummy-hash", hashed once and pasted
// as a constant so it never changes across runs). Every 401 path that
// didn't reach a real VerifyToken call (missing/malformed header) still
// runs apitoken.Verify against this so it does comparable constant-time
// crypto work to a real miss. This is defense-in-depth matching the house
// style set by handlers_hooks.go's always-compare idiom, NOT the primary
// control — the indexed token_hash lookup inside VerifyToken already
// dominates real timing, and a network round-trip's jitter swamps any
// nanosecond-scale difference a missing compare might leak.
const dummyTokenHash = "4d9de0975063e61d1505c29711a05180c096c9ae8bcca8329fb447145201c5bd"

// ctxKey is middleware_auth.go's own unexported context-key type, so its
// key can never collide with a key defined in another package (the
// standard Go context-key idiom).
type ctxKey int

const tokenCtxKey ctxKey = iota

// contextWithToken returns a copy of ctx carrying row, retrievable via
// TokenFromContext.
func contextWithToken(ctx context.Context, row store.TokenRow) context.Context {
	return context.WithValue(ctx, tokenCtxKey, row)
}

// TokenFromContext returns the store.TokenRow bearerAuth resolved for the
// current request, if the request went through bearerAuth and
// authenticated successfully. Exported so a later handler (step 34's
// per-session scope check) can read the caller's identity.
func TokenFromContext(ctx context.Context) (store.TokenRow, bool) {
	row, ok := ctx.Value(tokenCtxKey).(store.TokenRow)
	return row, ok
}

// bearerAuth wraps next in bearer-token authentication (design doc §4). It
// reuses handlers_hooks.go's auth idiom verbatim: the failure path always
// does comparable constant-time crypto work regardless of why it failed,
// the response body is byte-identical across every failure mode, and a
// durable unauthorized event is appended on every failure — never
// carrying token material, only the fact of the attempt.
func bearerAuth(next http.Handler, auth tokenAuthStore, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, haveToken := bearerToken(r.Header.Get("Authorization"))

		// Step 32: GET /v1/events/stream is consumed by browser EventSource,
		// which cannot set custom headers, so it alone may also present its
		// token as ?token=... . The Authorization header always wins when
		// present — the query-string fallback only applies when the header
		// yielded nothing at all — and this path is the only one exempted;
		// every other route still requires the header. Never logged: the
		// token only ever flows into VerifyToken below.
		if !haveToken && r.URL.Path == "/v1/events/stream" {
			if q := r.URL.Query().Get("token"); q != "" {
				presented, haveToken = q, true
			}
		}

		var (
			row store.TokenRow
			ok  bool
			err error
		)
		if haveToken {
			row, ok, err = auth.VerifyToken(r.Context(), presented)
		}
		if err != nil {
			// A real store failure (e.g. the DB is unreachable) is
			// distinct from an auth failure — it is corral's own bug,
			// not evidence the caller lacks a valid token — so it gets
			// its own status and no unauthorized event.
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error", nil)
			return
		}
		if !haveToken || !ok {
			// Always run the constant-time compare, even when
			// VerifyToken was never called (missing/malformed header),
			// so the 401's timing cannot be used to distinguish "no
			// header" from "unknown token" from "revoked token" — the
			// response body below is likewise byte-identical in every
			// case.
			_ = apitoken.Verify(presented, dummyTokenHash)
			appendUnauthorizedEvent(r.Context(), auth, r, log)
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized", nil)
			return
		}

		go auth.TouchToken(context.WithoutCancel(r.Context()), row.ID)
		next.ServeHTTP(w, r.WithContext(contextWithToken(r.Context(), row)))
	})
}

// bearerToken extracts the token from an Authorization header value.
// Only scheme "Bearer" (case-insensitive per RFC 7235) with a non-empty
// token is accepted; anything else (missing header, wrong scheme, empty
// token) reports ok == false, which bearerAuth treats identically to an
// unknown token.
func bearerToken(header string) (token string, ok bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

// unauthorizedEventData is the data JSON for EventTokenUnauthorized.
// Deliberately the only field: never the token, its prefix, or its hash —
// a failed-auth log that echoes the attempted secret would itself be a
// leak (design doc §4).
type unauthorizedEventData struct {
	RemoteAddr string `json:"remote_addr"`
}

// appendUnauthorizedEvent records a daemon-scoped (sessionID "")
// EventTokenUnauthorized. Best-effort: an AppendEvent failure is logged
// and swallowed, since a bookkeeping write must never change the auth
// outcome the caller already computed.
func appendUnauthorizedEvent(ctx context.Context, auth tokenAuthStore, r *http.Request, log *slog.Logger) {
	data, err := json.Marshal(unauthorizedEventData{RemoteAddr: r.RemoteAddr})
	if err != nil {
		data = []byte("{}")
	}
	if _, err := auth.AppendEvent(ctx, "", session.EventTokenUnauthorized, string(data)); err != nil {
		log.Warn("bearerAuth: failed to append token.unauthorized event", "error", err)
	}
}
