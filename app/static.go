package app

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"guludrive/core"
)

func httpError(w http.ResponseWriter, r *http.Request, status int) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, status, map[string]string{"error": http.StatusText(status)})
		return
	}
	http.Error(w, http.StatusText(status), status)
}

// staticHandler serves only the embedded public build, never a runtime directory.
func staticHandler(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			httpError(w, r, http.StatusMethodNotAllowed)
			return
		}
		p, err := staticPath(r.URL.Path)
		if err != nil {
			httpError(w, r, http.StatusNotFound)
			return
		}
		info, err := fs.Stat(assets, p)
		if err == nil && info.IsDir() {
			info, err = fs.Stat(assets, path.Join(p, "index.html"))
		}
		if err != nil || info.IsDir() {
			// Missing routes and asset directories never list files or fall back to HTML.
			httpError(w, r, http.StatusNotFound)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func staticPath(p string) (string, error) {
	p, err := core.NormalizePath(p)
	if err != nil {
		return "", err
	}
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "index.html"
	}
	return p, nil
}
