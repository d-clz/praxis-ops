// Package dashboard serves the operator dashboard's static frontend
// (login, session list, terminal panel) via go:embed. Purely static-asset
// serving -- no auth, no business logic here. The dashboard's actual data
// (GET /dashboard/summary, GET /sessions, the shell WebSocket) lives in
// internal/api, authenticated the same way every other route is; this
// package only needs to exist because the HTML/JS/CSS themselves have to
// be reachable before any token is known.
//
// Mounted at /ui/, not /dashboard/, deliberately: /dashboard/summary is
// an API route owned by internal/api. Serving the static frontend from
// /dashboard/ (a ServeMux subtree pattern) on the outer composing mux in
// cmd/orchestrator/main.go would swallow that path before the request
// ever reached the API's own mux -- ServeMux picks the most specific
// registered pattern at whichever mux layer first sees the request, and
// two different mux instances chained via Handle() don't get a second
// chance at a path the outer one already claimed.
package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler serves the embedded frontend under the given prefix (expected:
// "/ui/"). http.StripPrefix + http.FileServer, the standard library's own
// static-file pattern -- nothing dashboard-specific to hand-roll here.
func Handler(prefix string) (http.Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	return http.StripPrefix(prefix, http.FileServer(http.FS(sub))), nil
}
