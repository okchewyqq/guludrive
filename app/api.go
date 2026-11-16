package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"guludrive/core"
	"guludrive/internal/photos"
)

const maxJSON = 64 << 10

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if !(r.URL.Path == "/api/files" && r.Method == "PUT") && r.ContentLength > maxJSON {
		httpError(w, r, http.StatusRequestEntityTooLarge)
		return
	}
	switch {
	case r.URL.Path == "/api/setup" && (r.Method == "GET" || r.Method == "POST"):
		s.setup(w, r)
	case r.URL.Path == "/api/proxy-script" && r.Method == "POST":
		s.proxyScript(w, r)
	case r.URL.Path == "/api/session" && (r.Method == "POST" || r.Method == "DELETE"):
		s.security.session(w, r)
	case r.URL.Path == "/api/admin" && (r.Method == "GET" || r.Method == "PATCH"):
		s.administratorAPI(w, r)
	case r.URL.Path == "/api/status" && r.Method == "GET":
		accounts, err := s.accounts.Accounts(r.Context())
		if err != nil {
			apiFailure(w, err)
			return
		}
		status := core.Status{Version: Version}
		for _, account := range accounts {
			status.GoogleReady = status.GoogleReady || account.Selected
		}
		writeJSON(w, http.StatusOK, struct {
			core.Status
			GDirect core.GDirectStatus `json:"gdirect"`
		}{status, s.gdirect.Status()})
	case r.URL.Path == "/api/upload-settings" && (r.Method == "GET" || r.Method == "PUT"):
		s.uploadSettingsAPI(w, r)
	case r.URL.Path == "/api/download-settings" && (r.Method == "GET" || r.Method == "PUT"):
		s.downloadSettingsAPI(w, r)
	case r.URL.Path == "/api/gdirect/refresh" && r.Method == "POST":
		s.refreshGDirect(w, r)
	case r.URL.Path == "/api/download-links" && r.Method == "POST":
		s.createDownloadLink(w, r)
	case r.URL.Path == "/api/accounts" && r.Method == "GET":
		accounts, err := s.accounts.Accounts(r.Context())
		if err != nil {
			apiFailure(w, err)
			return
		}
		if accounts == nil {
			accounts = []core.Account{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
	case strings.HasPrefix(r.URL.Path, "/api/accounts/"):
		s.account(w, r)
	case r.URL.Path == "/api/buckets" && r.Method == "GET":
		buckets, err := s.fs.ListBuckets(r.Context())
		if err != nil {
			apiFailure(w, err)
			return
		}
		if buckets == nil {
			buckets = []core.Bucket{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
	case (r.URL.Path == "/api/buckets" || r.URL.Path == "/api/directories") && r.Method == "POST":
		s.mkdir(w, r)
	case r.URL.Path == "/api/files" && r.Method == "GET":
		s.list(w, r)
	case r.URL.Path == "/api/files" && r.Method == "PUT":
		s.upload(w, r)
	case r.URL.Path == "/api/files" && r.Method == "DELETE":
		s.remove(w, r)
	case r.URL.Path == "/api/file" && (r.Method == "GET" || r.Method == "HEAD"):
		s.download(w, r)
	case (r.URL.Path == "/api/copy" || r.URL.Path == "/api/move") && r.Method == "POST":
		s.transfer(w, r)
	default:
		httpError(w, r, http.StatusNotFound)
	}
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/accounts/google" && r.Method == "POST" {
		var in struct {
			Token string `json:"oauth_token"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.Token == "" {
			apiFailure(w, core.ErrInvalid)
			return
		}
		account, err := s.accounts.Login(r.Context(), in.Token)
		if err != nil {
			apiFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"account": account})
		return
	}
	id, action, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	if id == "" || (found && (action != "select" || r.Method != "POST")) || (!found && r.Method != "DELETE") {
		httpError(w, r, http.StatusNotFound)
		return
	}
	if _, err := readBody(w, r, maxJSON); err != nil {
		apiFailure(w, err)
		return
	}
	var err error
	if found {
		err = s.accounts.Select(r.Context(), id)
	} else {
		err = s.removeAccount(r.Context(), id)
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func queryPath(r *http.Request) (string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q["path"]) != 1 || q["path"][0] == "" {
		return "", core.ErrInvalid
	}
	return core.NormalizePath(q["path"][0]) // Query decoded once; never unescape again.
}

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	return io.ReadAll(http.MaxBytesReader(w, r.Body, max))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		httpError(w, r, http.StatusUnsupportedMediaType)
		return false
	}
	raw, err := readBody(w, r, maxJSON)
	if err != nil {
		apiFailure(w, err)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(raw) || decoder.Decode(value) != nil {
		httpError(w, r, http.StatusBadRequest)
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		httpError(w, r, http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func errorStatus(err error) int {
	var limit *http.MaxBytesError
	switch {
	case errors.Is(err, core.ErrUncertain), errors.Is(err, core.ErrBusy):
		return http.StatusServiceUnavailable
	case errors.As(err, &limit):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, core.ErrInvalid), errors.Is(err, io.ErrUnexpectedEOF):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrExists), errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrNotDir), errors.Is(err, core.ErrIsDir):
		return http.StatusConflict
	case errors.Is(err, core.ErrPrecondition):
		return http.StatusPreconditionFailed
	case errors.Is(err, core.ErrUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

func apiFailure(w http.ResponseWriter, err error) {
	var remote *core.RemoteError
	errors.As(err, &remote)
	var login *photos.LoginError
	errors.As(err, &login)
	status := http.StatusBadGateway
	var body map[string]any
	switch {
	case errors.Is(err, core.ErrUncertain):
		status = http.StatusServiceUnavailable
		body = map[string]any{
			"error": "Service Unavailable", "code": "outcome_uncertain", "retryable": false,
			"message": "The remote operation may have succeeded. Do not retry automatically.",
		}
	case login != nil || errors.Is(err, core.ErrUnauthorized):
		// JSON API 401 belongs only to guludrive's session guard/login. A backend
		// credential failure must not make a still-authenticated UI sign out.
		body = map[string]any{"error": "Google authentication failed", "code": "google_auth_failed"}
		if login != nil {
			if errors.Is(login, core.ErrUnauthorized) {
				status = http.StatusUnprocessableEntity
			}
			body["stage"], body["upstreamStatus"], body["reason"] = login.Stage, login.UpstreamStatus, login.Reason
		}
	case remote != nil:
		body = map[string]any{"error": "Remote operation failed", "code": "remote_operation_failed", "message": remote.Error()}
	default:
		status = errorStatus(err)
		writeJSON(w, status, map[string]string{"error": http.StatusText(status)})
		return
	}
	if remote != nil {
		diagnostic := remote.Safe()
		body["operation"], body["stage"] = diagnostic.Operation, diagnostic.Stage
		body["upstreamStatus"], body["reason"] = diagnostic.UpstreamStatus, diagnostic.Reason
		if diagnostic.BodyFormat != "" {
			body["bodyFormat"] = diagnostic.BodyFormat
		}
		if diagnostic.RPCCode != 0 {
			body["rpcCode"] = diagnostic.RPCCode
		}
		if diagnostic.ErrorReason != "" {
			body["errorReason"] = diagnostic.ErrorReason
		}
		if len(diagnostic.ViolationFields) != 0 {
			body["violationFields"] = diagnostic.ViolationFields
		}
	}
	writeJSON(w, status, body)
}
