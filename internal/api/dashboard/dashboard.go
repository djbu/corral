// Package dashboard is corral's embedded, unauthenticated browser dashboard
// shell (m5.md §13 step 33): a static single-page app (index.html, app.js,
// app.css) that fetches the authenticated /v1/dashboard snapshot and
// /v1/events/stream once it has a bearer token from the operator. Handler
// carries no session data of its own — only non-secret client code — which
// is what lets api.Server.AuthenticatedHandler place it in front of the
// bearer wall.
package dashboard

import (
	"embed"
	"net/http"
)

// assets embeds exactly three files by explicit name, never a directory
// glob (e.g. "*" or "."): a glob could silently sweep a stray file placed
// in this directory into the unauthenticated surface. Every file this
// package may ever serve must be named here first.
//
//go:embed index.html app.js app.css
var assets embed.FS

// Handler serves the unauthenticated dashboard shell:
//
//	GET /                  -> index.html (text/html; charset=utf-8)
//	GET /dashboard/app.js  -> app.js     (text/javascript, via http.FileServerFS)
//	GET /dashboard/app.css -> app.css    (text/css, via http.FileServerFS)
//
// It is served ONLY on the TCP-facing api.Server.AuthenticatedHandler (via
// its shell parameter), never on the unix-socket Handler() — the unix
// socket has no such parameter to accept it, by construction.
func Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Exact-path guard: without it, http.ServeMux's "/" pattern would
		// also match every unregistered path (e.g. "/favicon.ico"),
		// serving index.html for all of them instead of a 404.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFileFS(w, r, assets, "index.html")
	})

	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServerFS(assets)))

	return mux
}
