package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"guludrive/core"
	"guludrive/internal/gdirect"
)

const (
	downloadSigningKey = "download.signing_key"
	downloadModeKey    = "download.mode"
	workerURLKey       = "gdirect.worker_url"
	workerSecretKey    = "gdirect.worker_secret"
	publicOriginKey    = "download.public_origin"
	downloadLinkTTL    = 15 * time.Minute
	maxDownloadLinkTTL = time.Hour
	maxDownloadPayload = 24 << 10
	maxWorkerInteger   = 1<<53 - 1
)

type downloadSettingsView struct {
	Mode              string `json:"mode"`
	WorkerURL         string `json:"workerURL"`
	WorkerConfigured  bool   `json:"workerConfigured"`
	PublicOrigin      string `json:"publicOrigin"`
	PublicOriginReady bool   `json:"publicOriginReady"`
}

type downloadSettings struct {
	mode                    string
	workerURL, workerSecret string
	publicOrigin            string
}

func (d downloadSettings) view() downloadSettingsView {
	return downloadSettingsView{Mode: d.mode, WorkerURL: d.workerURL, WorkerConfigured: d.workerURL != "" && validProxyPassword(d.workerSecret), PublicOrigin: d.publicOrigin}
}

func (s *Server) downloadSettingsView(settings downloadSettings) downloadSettingsView {
	view := settings.view()
	view.PublicOrigin = s.security.publicOrigin()
	view.PublicOriginReady = publicManifestOrigin(view.PublicOrigin) != ""
	return view
}

// Signing keys and Worker secrets are private database state. Only view() may
// cross the settings API boundary; a blank secret input retains the saved value.
type downloads struct {
	mu       sync.RWMutex
	settings downloadSettings
	key      []byte
}

func validWorkerURL(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 2048 || !utf8.ValidString(value) || strings.ContainsAny(value, "#\\") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Host == "" {
		return false
	}
	return publicManifestOrigin(u.Scheme+"://"+u.Host) != ""
}

func validDownloadSettings(value downloadSettings) bool {
	if (value.mode != "local" && value.mode != "gdirect" && value.mode != "proxy") || !validWorkerURL(value.workerURL) {
		return false
	}
	if (value.workerSecret != "" && !validProxyPassword(value.workerSecret)) || (value.publicOrigin != "" && publicManifestOrigin(value.publicOrigin) != value.publicOrigin) {
		return false
	}
	return value.mode != "proxy" || value.view().WorkerConfigured
}

func loadDownloads(ctx context.Context, db *sql.DB) (*downloads, error) {
	if db == nil {
		return nil, errors.New("app: download settings require the service database")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("app: cannot open download settings")
	}
	defer tx.Rollback()
	values := make(map[string]string, 5)
	for _, key := range []string{downloadSigningKey, downloadModeKey, workerURLKey, workerSecretKey, publicOriginKey} {
		var value string
		if err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("app: cannot read download settings")
		}
		values[key] = value
	}
	mode := values[downloadModeKey]
	if mode == "" {
		mode = "local"
	}
	settings := downloadSettings{mode: mode, workerURL: values[workerURLKey], workerSecret: values[workerSecretKey], publicOrigin: values[publicOriginKey]}
	if !validDownloadSettings(settings) {
		return nil, errors.New("app: invalid persisted download settings")
	}
	encoded := values[downloadSigningKey]
	if encoded == "" {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, errors.New("app: cannot create download signing key")
		}
		encoded = base64.RawURLEncoding.EncodeToString(key)
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)`, downloadSigningKey, encoded); err != nil {
			return nil, errors.New("app: cannot save download signing key")
		}
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("app: invalid persisted download signing key")
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.New("app: cannot commit download settings")
	}
	return &downloads{settings: settings, key: key}, nil
}

func (s *Server) downloadSettingsAPI(w http.ResponseWriter, r *http.Request) {
	d := s.downloads
	if r.Method == http.MethodGet {
		d.mu.RLock()
		view := s.downloadSettingsView(d.settings)
		d.mu.RUnlock()
		writeJSON(w, http.StatusOK, view)
		return
	}
	var in struct {
		Mode         string  `json:"mode"`
		WorkerURL    *string `json:"workerURL"`
		WorkerSecret string  `json:"workerSecret"`
		PublicOrigin *string `json:"publicOrigin"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.WorkerURL == nil || in.PublicOrigin == nil {
		apiFailure(w, core.ErrInvalid)
		return
	}
	settings, selectIP, err := s.commitDownloadSettings(r.Context(), in.Mode, *in.WorkerURL, in.WorkerSecret, *in.PublicOrigin)
	if err != nil {
		apiFailure(w, err)
		return
	}
	if selectIP {
		// The choice is committed even if no candidate is reachable. Never hold
		// SQL or the settings lock while probing; status remains observable.
		_, _ = s.gdirect.Refresh(r.Context())
	}
	writeJSON(w, http.StatusOK, s.downloadSettingsView(settings))
}

// commitDownloadSettings publishes persistence and runtime mode together. It
// never selects an IP; the caller probes only after SQL and mutexes are released.
func (s *Server) commitDownloadSettings(ctx context.Context, mode, workerURL, workerSecret, publicOrigin string) (downloadSettings, bool, error) {
	d := s.downloads
	d.mu.Lock()
	defer d.mu.Unlock()
	if publicOrigin != "" {
		publicOrigin = publicManifestOrigin(publicOrigin)
		if publicOrigin == "" {
			return downloadSettings{}, false, core.ErrInvalid
		}
	}
	settings := downloadSettings{mode: mode, workerURL: workerURL, workerSecret: d.settings.workerSecret, publicOrigin: publicOrigin}
	if workerSecret != "" {
		settings.workerSecret = workerSecret
	}
	if !validDownloadSettings(settings) {
		return downloadSettings{}, false, core.ErrInvalid
	}
	// Failure leaves both the public settings and router state untouched.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return downloadSettings{}, false, err
	}
	defer tx.Rollback()
	for _, item := range []struct{ key, value string }{{downloadModeKey, settings.mode}, {workerURLKey, settings.workerURL}, {workerSecretKey, settings.workerSecret}, {publicOriginKey, settings.publicOrigin}} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, item.key, item.value); err != nil {
			return downloadSettings{}, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key='gdirect.enabled'`); err != nil {
		return downloadSettings{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return downloadSettings{}, false, err
	}
	selectIP := settings.mode == "gdirect" && d.settings.mode != "gdirect"
	d.settings = settings
	s.security.setPublicOrigin(settings.publicOrigin)
	s.gdirect.SetEnabled(settings.mode == "gdirect")
	return settings, selectIP, nil
}

func (s *Server) refreshGDirect(w http.ResponseWriter, r *http.Request) {
	s.downloads.mu.RLock()
	mode := s.downloads.settings.mode
	s.downloads.mu.RUnlock()
	if mode != "gdirect" {
		httpError(w, r, http.StatusConflict)
		return
	}
	status, err := s.gdirect.Refresh(r.Context())
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			httpError(w, r, http.StatusRequestTimeout)
		case !status.Enabled || status.Selecting:
			httpError(w, r, http.StatusConflict)
		case errors.Is(err, core.ErrBusy) && status.IP == "":
			// A completed scan with no reachable candidate is a status result,
			// not a failed settings change or administrator authentication.
			writeJSON(w, http.StatusOK, status)
		default:
			httpError(w, r, http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) gdirectReady(w http.ResponseWriter) bool {
	status := s.gdirect.Status()
	if status.Enabled && status.IP != "" && !status.Selecting {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Service Unavailable", "code": "gdirect_not_ready"})
	return false
}

// Worker fields are frozen with the independently deployed Worker. Sign the
// exact base64url JSON parameter, not decoded/re-serialized JSON or URL text.
type workerDownload struct {
	V        int    `json:"v"`
	Mode     string `json:"mode"`
	Manifest string `json:"manifest"`
	Exp      int64  `json:"exp"`
}

type serverDownload struct {
	V       int    `json:"v"`
	Mode    string `json:"mode"`
	Route   string `json:"route,omitempty"`
	Path    string `json:"path"`
	ID      int64  `json:"id"`
	Version int64  `json:"version"`
	Exp     int64  `json:"exp"`
}

func downloadParameters(value any, key []byte) (url.Values, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxDownloadPayload {
		return nil, core.ErrInvalid
	}
	p := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(p))
	return url.Values{"p": {p}, "auth": {base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}}, nil
}

func (s *Server) createDownloadLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	p, err := core.NormalizePath(in.Path)
	if err != nil || in.Path == "" {
		apiFailure(w, core.ErrInvalid)
		return
	}
	s.downloads.mu.RLock()
	settings, key := s.downloads.settings, s.downloads.key
	s.downloads.mu.RUnlock()
	expires := time.Now().UTC().Add(downloadLinkTTL).Truncate(time.Second)
	mode, target := settings.mode, ""
	if mode == "gdirect" && !s.gdirectReady(w) {
		return
	}
	var payload any
	if mode == "proxy" {
		if !settings.view().WorkerConfigured {
			apiFailure(w, core.ErrInvalid)
			return
		}
		origin := publicManifestOrigin(s.security.publicOrigin())
		if origin == "" {
			proxyFailure(w, http.StatusUnprocessableEntity, "proxy_origin_required")
			return
		}
		// Issuance is metadata-only. The public scoped manifest resolves Google
		// URLs only when the Worker actually needs the confirmed segment group.
		entry, err := s.fs.Stat(r.Context(), p)
		if err != nil {
			apiFailure(w, err)
			return
		}
		if _, err = manifestMetadata(entry); err != nil {
			manifestFailure(w, err)
			return
		}
		if entry.Path != p {
			proxyFailure(w, http.StatusBadGateway, "proxy_manifest_unavailable")
			return
		}
		parameters, err := downloadParameters(serverDownload{V: 1, Mode: "manifest", Path: p, ID: entry.ID, Version: entry.Version, Exp: expires.Unix()}, key)
		if err != nil {
			apiFailure(w, err)
			return
		}
		target, key = settings.workerURL, []byte(settings.workerSecret)
		payload = workerDownload{V: 2, Mode: "worker", Manifest: origin + "/download-manifest?" + parameters.Encode(), Exp: expires.Unix()}
	} else {
		entry, err := s.fs.Stat(r.Context(), p)
		if err != nil {
			apiFailure(w, err)
			return
		}
		if entry.IsDir {
			apiFailure(w, core.ErrIsDir)
			return
		}
		if entry.ID <= 0 || entry.Version <= 0 {
			apiFailure(w, core.ErrInvalid)
			return
		}
		origin, err := s.security.requestOrigin(r)
		if err != nil {
			apiFailure(w, core.ErrInvalid)
			return
		}
		target = origin + "/download"
		payload = serverDownload{V: 1, Mode: "server", Route: mode, Path: p, ID: entry.ID, Version: entry.Version, Exp: expires.Unix()}
	}
	parameters, err := downloadParameters(payload, key)
	if err != nil {
		apiFailure(w, err)
		return
	}
	link := target + "?" + parameters.Encode()
	if mode == "proxy" && (len(parameters.Get("p")) > 16<<10 || len(link) > (16<<10)+2048) {
		apiFailure(w, core.ErrInvalid)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expiresAt"`
		Mode      string    `json:"mode"`
	}{link, expires, mode})
}

func (s *Server) serverDownload(r *http.Request) (serverDownload, bool) {
	payload, _, valid := verifyDownload(r.URL.RawQuery, s.downloads.key, "server", time.Now().Unix())
	return payload, valid
}

// verifyDownload is pure: both public routes share the same strict signature,
// parser, canonical path and expiry checks, but never share capability modes.
func verifyDownload(rawQuery string, key []byte, expectedMode string, now int64) (serverDownload, bool, bool) {
	var payload serverDownload
	query, err := url.ParseQuery(rawQuery)
	meta := false
	if values, found := query["meta"]; found {
		if expectedMode != "manifest" || len(values) != 1 || values[0] != "1" {
			return payload, false, false
		}
		meta = true
	}
	for name := range query {
		if name != "p" && name != "auth" && !(name == "meta" && meta) {
			return payload, false, false
		}
	}
	if err != nil || len(query["p"]) != 1 || len(query["auth"]) != 1 || len(query.Get("auth")) != base64.RawURLEncoding.EncodedLen(sha256.Size) || len(query.Get("p")) > base64.RawURLEncoding.EncodedLen(maxDownloadPayload) {
		return payload, false, false
	}
	p := query.Get("p")
	signature, err := base64.RawURLEncoding.Strict().DecodeString(query.Get("auth"))
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != query.Get("auth") {
		return payload, false, false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(p))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return payload, false, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(p)
	if err != nil || !utf8.Valid(raw) || base64.RawURLEncoding.EncodeToString(raw) != p {
		return payload, false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return payload, false, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return payload, false, false
	}
	path, err := core.NormalizePath(payload.Path)
	routeOK := (expectedMode == "server" && (payload.Route == "local" || payload.Route == "gdirect")) || (expectedMode == "manifest" && payload.Route == "")
	return payload, meta, err == nil && path == payload.Path && payload.V == 1 && payload.Mode == expectedMode && routeOK && payload.ID > 0 && payload.Version > 0 && payload.Exp > now && payload.Exp-now <= int64(maxDownloadLinkTTL/time.Second)
}

// This boundary accepts only a scoped download capability, never an admin
// password/session or a Google token. Opening a GET pins an immutable entry;
// identity is checked on that exact handle before ServeContent can read it.
func (s *Server) signedDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", dataCSP)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpError(w, r, http.StatusMethodNotAllowed)
		return
	}
	payload, valid := s.serverDownload(r)
	if !valid {
		httpError(w, r, http.StatusForbidden)
		return
	}
	if payload.Route == "gdirect" && !s.gdirectReady(w) {
		return
	}
	r = r.WithContext(gdirect.WithDirect(r.Context(), payload.Route == "gdirect"))
	var reader io.ReadSeekCloser
	var entry core.Entry
	var err error
	if r.Method == http.MethodHead {
		entry, err = s.fs.Stat(r.Context(), payload.Path)
	} else {
		reader, entry, err = s.fs.Open(r.Context(), payload.Path)
		if reader != nil {
			defer reader.Close()
		}
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	if entry.IsDir || entry.ID != payload.ID || entry.Version != payload.Version {
		httpError(w, r, http.StatusForbidden)
		return
	}
	fileHeaders(w, entry)
	w, r = mp4FileRequest(w, r, entry)
	if r.Method == http.MethodHead {
		http.ServeContent(w, r, entry.Name, entry.ModTime, io.NewSectionReader(bytes.NewReader(nil), 0, entry.Size))
	} else {
		response := &fileResponse{ResponseWriter: w, ReadSeeker: reader}
		http.ServeContent(response, r, entry.Name, entry.ModTime, response)
		response.finish(r)
	}
}
