package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"guludrive/core"
	"guludrive/worker"
)

const proxyConfigMarker = "/* GULUDRIVE_PROXY_CONFIG */ null"

func validProxyPassword(password string) bool {
	return password != "" && len(password) <= 4096 && utf8.ValidString(password) && !strings.ContainsAny(password, "\x00\r\n")
}

func generateProxyScript(origin, password string) (string, error) {
	origin = publicManifestOrigin(origin)
	if origin == "" || !validProxyPassword(password) {
		return "", core.ErrInvalid
	}
	config := struct {
		PublicOrigin string `json:"publicOrigin"`
		Password     string `json:"password"`
	}{origin, password}
	raw, err := json.Marshal(config)
	if err != nil || strings.Count(worker.Template, proxyConfigMarker) != 1 {
		return "", core.ErrInvalid
	}
	return strings.Replace(worker.Template, proxyConfigMarker, string(raw), 1), nil
}

// proxyScript exports only the supplied draft. It neither reads a saved password
// nor changes settings; the script itself is intentionally a sensitive export.
func (s *Server) proxyScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		PublicOrigin string `json:"publicOrigin"`
		Password     string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	script, err := generateProxyScript(in.PublicOrigin, in.Password)
	if err != nil {
		apiFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Script string `json:"script"`
	}{script})
}
