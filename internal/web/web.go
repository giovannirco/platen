// Package web serves Platen's browser interface. The files are embedded in the
// binary, so there is nothing to install next to it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var files embed.FS

// Handler serves the interface. Unknown paths get the single page, so that a
// reload on any view works.
func Handler() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	server := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(static, path); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// The files are small and change with every release, so browsers always
		// ask again instead of running a stale script against a newer API.
		w.Header().Set("Cache-Control", "no-cache")
		server.ServeHTTP(w, r)
	})
}
