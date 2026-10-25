package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFS holds the embedded web console assets (see internal/api/ui/).
//
//go:embed ui
var uiFS embed.FS

// registerUI serves the web console at /ui/ and redirects / to it.
// The assets are static and expose nothing sensitive; the console reads live
// data from the API using a token the operator supplies in the browser.
func (h *Handler) registerUI(mux *http.ServeMux) {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic("api: embed ui: " + err.Error()) // build-time guarantee; never happens at runtime
	}
	files := http.StripPrefix("/ui/", uiSecurityHeaders(http.FileServer(http.FS(sub))))

	mux.Handle("GET /ui/", files)
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
}

// uiSecurityHeaders wraps the console file server with a strict, self-contained
// CSP. The page loads only its own CSS/JS and talks only to this origin's API,
// so nothing external is permitted.
func uiSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; "+
				"connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
