package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/crypto/bcrypt"

	"guludrive/core"
)

// API sessions are cookie-only and in-memory. Cookie names isolate coexisting
// localhost data directories; the public namespace hash is not a secret.
type security struct {
	credentialMu  sync.RWMutex
	username      [32]byte
	passwordHash  []byte
	displayName   string
	epoch         int
	originMu      sync.RWMutex
	origin        string
	sessions, tls *scs.SessionManager
	mu            sync.Mutex
	window        time.Time
	attempts      int
}

func newSecurity(cfg Config, admin administrator) *security {
	// SCS memory-store cleanup lives for the process; StopCleanup races its initialization.
	manager := scs.New()
	manager.Lifetime = 12 * time.Hour
	manager.HashTokenInStore = true
	digest := sha256.Sum256([]byte(cfg.DataDir))
	manager.Cookie.Name = "guludrive_session_" + hex.EncodeToString(digest[:12])
	manager.Cookie.Path = "/api"
	manager.Cookie.HttpOnly = true
	manager.Cookie.SameSite = http.SameSiteStrictMode
	manager.Cookie.Secure = false
	manager.ErrorFunc = func(w http.ResponseWriter, r *http.Request, _ error) {
		httpError(w, r, http.StatusInternalServerError)
	}
	// Both immutable configurations share SCS's store and session context key.
	// Direct TLS or a host-matched HTTPS public origin uses Secure cookies.
	tls := *manager
	tls.Cookie.Secure = true
	return &security{username: sha256.Sum256([]byte(admin.username)), passwordHash: bytes.Clone(admin.passwordHash), displayName: admin.username, epoch: 1, origin: cfg.PublicOrigin, sessions: manager, tls: &tls}
}

type sessionManagerKey struct{}

func (s *security) manager(r *http.Request) *scs.SessionManager {
	if manager, ok := r.Context().Value(sessionManagerKey{}).(*scs.SessionManager); ok {
		return manager
	}
	origin, _ := s.requestOrigin(r)
	if r.TLS != nil || strings.HasPrefix(origin, "https://") {
		return s.tls
	}
	return s.sessions
}

func (s *security) initialized() bool {
	s.credentialMu.RLock()
	defer s.credentialMu.RUnlock()
	return len(s.passwordHash) != 0
}

func (s *security) publicOrigin() string {
	s.originMu.RLock()
	defer s.originMu.RUnlock()
	return s.origin
}

// The caller supplies a validated origin only after its SQL commit succeeds.
func (s *security) setPublicOrigin(origin string) {
	s.originMu.Lock()
	defer s.originMu.Unlock()
	s.origin = origin
}

func (s *security) administrator() administrator {
	s.credentialMu.RLock()
	defer s.credentialMu.RUnlock()
	return administrator{username: s.displayName, passwordHash: bytes.Clone(s.passwordHash)}
}

// The caller holds credentialMu exclusively after a successful SQL commit.
func (s *security) replaceAdministrator(admin administrator) {
	s.username = sha256.Sum256([]byte(admin.username))
	s.passwordHash = bytes.Clone(admin.passwordHash)
	s.displayName = admin.username
	s.epoch++
}

func (s *security) authenticate(username, password string) (int, bool) {
	// Capture the verifier and its epoch together. bcrypt can run outside the
	// lock, but a concurrent change must leave this login at the old epoch.
	s.credentialMu.RLock()
	wantUsername, hash, epoch := s.username, bytes.Clone(s.passwordHash), s.epoch
	s.credentialMu.RUnlock()
	a := sha256.Sum256([]byte(username))
	passwordOK := validAdministratorPassword(password) && bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	return epoch, subtle.ConstantTimeCompare(a[:], wantUsername[:]) == 1 && passwordOK
}

func (s *security) credentials(username, password string) bool {
	_, valid := s.authenticate(username, password)
	return valid
}

func (s *security) api(next http.Handler) http.Handler {
	guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bootstrap := (r.URL.Path == "/api/session" && r.Method == http.MethodPost) || (r.URL.Path == "/api/setup" && (r.Method == http.MethodGet || r.Method == http.MethodPost))
		if !bootstrap {
			s.credentialMu.RLock()
			epoch, initialized := s.epoch, len(s.passwordHash) != 0
			s.credentialMu.RUnlock()
			manager := s.manager(r)
			if !initialized || !manager.GetBool(r.Context(), "admin") || manager.GetInt(r.Context(), "adminEpoch") != epoch {
				httpError(w, r, http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	plain, tls := s.sessions.LoadAndSave(guard), s.tls.LoadAndSave(guard)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		origin, err := s.requestOrigin(r)
		if err != nil || !sameOriginAt(r, origin) || (r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-GuluDrive-Request") != "1") {
			httpError(w, r, http.StatusForbidden)
			return
		}
		// Freeze the manager with the Origin snapshot for this request even if
		// download settings change while its handler is running.
		manager, handler := s.sessions, plain
		if r.TLS != nil || strings.HasPrefix(origin, "https://") {
			manager, handler = s.tls, tls
		}
		r = r.WithContext(context.WithValue(r.Context(), sessionManagerKey{}, manager))
		handler.ServeHTTP(w, r)
	})
}

func (s *security) session(w http.ResponseWriter, r *http.Request) {
	manager := s.manager(r)
	if r.Method == http.MethodDelete {
		if _, err := readBody(w, r, maxJSON); err != nil {
			apiFailure(w, err)
			return
		}
		if err := manager.Destroy(r.Context()); err != nil {
			apiFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if !s.allowAttempt(w, r) {
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	epoch, valid := s.authenticate(in.Username, in.Password)
	if !valid {
		httpError(w, r, http.StatusUnauthorized)
		return
	}
	if err := manager.RenewToken(r.Context()); err != nil {
		apiFailure(w, err)
		return
	}
	manager.Put(r.Context(), "admin", true)
	manager.Put(r.Context(), "adminEpoch", epoch)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Setup and login share the same bounded, process-wide attempt window.
func (s *security) allowAttempt(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	if time.Since(s.window) >= time.Minute {
		s.window, s.attempts = time.Now(), 0
	}
	s.attempts++
	allowed := s.attempts <= 20
	s.mu.Unlock()
	if !allowed {
		w.Header().Set("Retry-After", "60")
		httpError(w, r, http.StatusTooManyRequests)
	}
	return allowed
}

func (s *security) dav(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.sameOrigin(r) {
			httpError(w, r, http.StatusForbidden)
			return
		}
		username, password, ok := r.BasicAuth()
		valid := s.credentials(username, password)
		if !ok || !valid {
			w.Header().Set("WWW-Authenticate", `Basic realm="guludrive", charset="UTF-8"`)
			httpError(w, r, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func canonicalOrigin(origin string) (string, error) {
	if origin == "" {
		return "", nil
	}
	if !utf8.ValidString(origin) || strings.IndexFunc(origin, unicode.IsSpace) >= 0 || strings.ContainsAny(origin, "#%\\") {
		return "", core.ErrInvalid
	}
	u, err := url.Parse(origin)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Host == "" {
		return "", core.ErrInvalid
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if (scheme != "http" && scheme != "https") || host == "" || strings.ContainsAny(host, "/?#@[]") || strings.IndexFunc(host, unicode.IsControl) >= 0 {
		return "", core.ErrInvalid
	}
	if strings.ContainsAny(u.Host, "[]") && (net.ParseIP(host) == nil || !strings.Contains(host, ":")) {
		return "", core.ErrInvalid
	}
	if strings.Contains(host, ":") && (net.ParseIP(host) == nil || !strings.HasPrefix(u.Host, "[")) {
		return "", core.ErrInvalid
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", core.ErrInvalid
	}
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", core.ErrInvalid
		}
		port = strconv.Itoa(n)
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

func (s *security) requestOrigin(r *http.Request) (string, error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	actual, err := canonicalOrigin(scheme + "://" + r.Host)
	if err != nil {
		return "", err
	}
	if configured := s.publicOrigin(); configured != "" {
		publicScheme, _, _ := strings.Cut(configured, "://")
		// An explicit origin applies only to the matching authority, including
		// non-default ports. Forwarded headers are deliberately untrusted.
		if hostOrigin, err := canonicalOrigin(publicScheme + "://" + r.Host); err == nil && hostOrigin == configured {
			return configured, nil
		}
	}
	if r.TLS == nil {
		// Zero-config HTTPS proxies may preserve Host while terminating TLS.
		// Allow only a single canonical HTTPS Origin for that same authority;
		// this never trusts a different host/port or permits an HTTP downgrade.
		origins := r.Header.Values("Origin")
		if len(origins) == 1 && origins[0] != "" {
			origin, originErr := canonicalOrigin(origins[0])
			upgraded, upgradeErr := canonicalOrigin("https://" + r.Host)
			if originErr == nil && upgradeErr == nil && origin == upgraded {
				return upgraded, nil
			}
		}
	}
	return actual, nil
}

// Native clients may omit Origin. Forwarded headers are deliberately untrusted.
func (s *security) sameOrigin(r *http.Request) bool {
	want, err := s.requestOrigin(r)
	return err == nil && sameOriginAt(r, want)
}

func sameOriginAt(r *http.Request, want string) bool {
	for _, site := range r.Header.Values("Sec-Fetch-Site") {
		if strings.EqualFold(strings.TrimSpace(site), "cross-site") {
			return false
		}
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 || origins[0] == "" {
		return false
	}
	origin, err := canonicalOrigin(origins[0])
	if err != nil {
		return false
	}
	return origin == want
}
