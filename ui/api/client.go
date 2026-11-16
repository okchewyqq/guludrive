package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"time"

	"guludrive/core"
	"guludrive/ui/i18n"
)

// Error is safe to show to the user. Non-JSON response bodies are not echoed.
type Error struct {
	Status         int
	Message        string
	Code           string
	Operation      string
	Stage          string
	UpstreamStatus int
	Reason         string
	fallback       bool
}

const (
	uncertainMessage              = "Remote result is unknown (%s). Do not retry automatically; verify the result before continuing."
	googleAuthFailed              = "google_auth_failed"
	remoteOperationFailed         = "remote_operation_failed"
	outcomeUncertain              = "outcome_uncertain"
	gdirectNotReady               = "gdirect_not_ready"
	gdirectNotReadyMessage        = "GDirect has no verified IP available. Refresh the IP in Settings and try again. You are still signed in."
	proxyOriginRequired           = "proxy_origin_required"
	proxyOriginRequiredMessage    = "Configure the server public HTTPS address publicOrigin to use Web Proxy. You are still signed in."
	proxyUnsupportedMetadata      = "proxy_unsupported_metadata"
	proxyManifestTooLarge         = "proxy_manifest_too_large"
	invalidCurrentPassword        = "invalid_current_password"
	invalidCurrentPasswordMessage = "The current password is incorrect. You are still signed in."
)

func googleStageLabel(stage string) string {
	switch stage {
	case "exchange_master":
		return "Google master exchange"
	case "exchange_bearer":
		return "Google bearer exchange"
	case "validate_photos":
		return "Google Photos validation"
	default:
		return ""
	}
}

func googleReason(reason string) string {
	switch reason {
	case "BadAuthentication", "NeedsBrowser", "ServiceUnavailable":
		return reason
	default:
		return "unknown"
	}
}

func (e *Error) googleText(locale i18n.Locale) string {
	if remoteOperationLabel(e.Operation) != "" {
		return e.remoteText(locale)
	}
	var details []string
	if label := googleStageLabel(e.Stage); label != "" {
		details = append(details, i18n.T(locale, label))
	}
	if e.UpstreamStatus >= 100 && e.UpstreamStatus <= 599 {
		details = append(details, fmt.Sprintf("HTTP %d", e.UpstreamStatus))
	}
	reason := googleReason(e.Reason)
	details = append(details, reason)
	message := "Google authentication failed (%s). You are still signed in to GuluDrive. Obtain a fresh oauth_token before trying again."
	if e.Status == http.StatusUnprocessableEntity {
		message = "Google rejected the credentials (%s). Obtain a fresh oauth_token before trying again."
		if reason == "NeedsBrowser" {
			message = "Google requires browser sign-in (%s). Obtain a fresh oauth_token before trying again."
		}
	} else if reason == "ServiceUnavailable" {
		message = "Google authentication service is unavailable (%s). You are still signed in to GuluDrive. Obtain a fresh oauth_token before trying again."
	}
	return i18n.T(locale, message, strings.Join(details, " · "))
}

func remoteOperationLabel(operation string) string {
	switch operation {
	case "create_album":
		return "Create bucket"
	case "upload":
		return "Upload files"
	case "commit_upload":
		return "Commit upload"
	case "add_to_album":
		return "Add file to album"
	case "trash":
		return "Delete remote item"
	case "download":
		return "Download"
	default:
		return ""
	}
}

func remoteStageLabel(stage string) string {
	switch stage {
	case "request":
		return "Request"
	case "http_response":
		return "HTTP response"
	case "read_response":
		return "Read response"
	case "decode_response":
		return "Decode response"
	case "decode_album_id":
		return "Read album ID"
	case "decode_media_id":
		return "Read media ID"
	case "record_album":
		return "Record album"
	default:
		return ""
	}
}

func remoteReasonLabel(reason string) string {
	switch reason {
	case "rejected":
		return "Request rejected"
	case "network_error":
		return "Network error"
	case "response_too_large":
		return "Upstream response too large"
	case "invalid_gzip":
		return "Invalid gzip response"
	case "unsupported_encoding":
		return "Unsupported response encoding"
	case "invalid_protobuf":
		return "Invalid upstream response"
	case "missing_album_id":
		return "Missing album ID"
	case "invalid_album_id":
		return "Invalid album ID"
	case "missing_media_id":
		return "Missing media ID"
	case "invalid_media_id":
		return "Invalid media ID"
	case "journal_failed":
		return "Could not save operation record"
	default:
		return ""
	}
}

func (e *Error) remoteText(locale i18n.Locale) string {
	operation := remoteOperationLabel(e.Operation)
	if operation == "" {
		operation = "Remote operation"
	}
	details := []string{i18n.T(locale, operation)}
	if label := remoteStageLabel(e.Stage); label != "" {
		details = append(details, i18n.T(locale, label))
	}
	if e.UpstreamStatus >= 100 && e.UpstreamStatus <= 599 {
		details = append(details, fmt.Sprintf("HTTP %d", e.UpstreamStatus))
	}
	if label := remoteReasonLabel(e.Reason); label != "" {
		details = append(details, i18n.T(locale, label))
	}
	message := "Remote operation failed (%s)."
	switch e.Code {
	case googleAuthFailed:
		message = "Google authentication failed (%s). You are still signed in to GuluDrive. Obtain a fresh oauth_token before trying again."
	case outcomeUncertain:
		message = uncertainMessage
	}
	return i18n.T(locale, message, strings.Join(details, " · "))
}

func proxyErrorMessage(code string) string {
	switch code {
	case proxyOriginRequired:
		return proxyOriginRequiredMessage
	case proxyUnsupportedMetadata:
		return "This file’s metadata is not supported by Web Proxy. Choose another download mode in Settings. You are still signed in."
	case proxyManifestTooLarge:
		return "This file has too many parts for a Web Proxy link. Choose another download mode in Settings. You are still signed in."
	}
	return ""
}

func (e *Error) Localized(locale i18n.Locale) string {
	if message := proxyErrorMessage(e.Code); message != "" {
		return i18n.T(locale, message)
	}
	if e.Code == gdirectNotReady {
		return i18n.T(locale, gdirectNotReadyMessage)
	}
	if e.Code == invalidCurrentPassword {
		return i18n.T(locale, invalidCurrentPasswordMessage)
	}
	if e.Code == googleAuthFailed {
		return e.googleText(locale)
	}
	if e.Code == remoteOperationFailed {
		if e.Operation == "upload" || e.Operation == "commit_upload" {
			return i18n.T(locale, "Upload failed. Check the file and connection, then try again.")
		}
		return e.remoteText(locale)
	}
	if e.Code == outcomeUncertain {
		if e.Operation == "upload" || e.Operation == "commit_upload" {
			return i18n.T(locale, "Google could not confirm this upload. Refresh the file list and check before uploading again.")
		}
		return i18n.T(locale, "The operation result is unconfirmed. Refresh and check before trying again.")
	}
	if e.fallback {
		return i18n.T(locale, "Server returned HTTP %d (%s)", e.Status, http.StatusText(e.Status))
	}
	return i18n.T(locale, "Remote error: %s", e.Message)
}

func (e *Error) Error() string {
	if message := proxyErrorMessage(e.Code); message != "" {
		return message
	}
	if e.Code == gdirectNotReady {
		return gdirectNotReadyMessage
	}
	if e.Code == invalidCurrentPassword {
		return invalidCurrentPasswordMessage
	}
	if e.Code == googleAuthFailed {
		return e.googleText(i18n.English)
	}
	if e.Code == remoteOperationFailed || e.Code == outcomeUncertain {
		return e.remoteText(i18n.English)
	}
	return e.Message
}
func (e *Error) Unwrap() error {
	if e.Code == outcomeUncertain {
		return core.ErrUncertain
	}
	return nil
}
func Unauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized && e.Code != googleAuthFailed && e.Code != remoteOperationFailed && e.Code != outcomeUncertain && proxyErrorMessage(e.Code) == ""
}

type Client struct {
	base string
	http *http.Client
}

// Preconditions are HTTP headers, not additional server JSON fields.
type Preconditions struct{ IfMatch, IfNoneMatch string }

func PreconditionFailed(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusPreconditionFailed
}

// FileContentType describes the logical payload, never its BMP carrier.
func FileContentType(name string) string {
	if value := mime.TypeByExtension(path.Ext(name)); value != "" {
		return value
	}
	return "application/octet-stream"
}

func New(base string, transport *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, i18n.Err("Enter an absolute http:// or https:// server URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, i18n.Err("Server URL must not contain credentials, query parameters or a fragment")
	}
	var client http.Client
	if transport != nil {
		client = *transport
	}
	// Sessions stay in a process-local cookie jar, never in preferences.
	if client.Jar == nil {
		client.Jar, err = cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(u.String(), "/"), http: &client}, nil
}
func (c *Client) BaseURL() string         { return c.base }
func (c *Client) URL(route string) string { return c.base + route }
func pathQuery(route, p string) string    { return route + "?" + url.Values{"path": {p}}.Encode() }
func (c *Client) WebDAVURL() string       { return c.URL("/dav/") }

func (c *Client) request(ctx context.Context, method, route string, body io.Reader, size int64, conditions ...Preconditions) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL(route), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	req.Header.Set("Accept", "application/json")
	if method != "GET" && method != "HEAD" {
		req.Header.Set("X-GuluDrive-Request", "1")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if method == "PUT" && strings.HasPrefix(route, "/api/files?") {
			req.Header.Set("Content-Type", FileContentType(req.URL.Query().Get("path")))
		}
	}
	if len(conditions) > 0 {
		condition := conditions[0]
		if condition.IfMatch != "" {
			req.Header.Set("If-Match", condition.IfMatch)
		}
		if condition.IfNoneMatch != "" {
			req.Header.Set("If-None-Match", condition.IfNoneMatch)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Do not surface URL credentials or transport internals containing secrets.
		return nil, i18n.Err("Cannot reach the server. Check the address, TLS and network connection")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp, nil
}
func responseError(resp *http.Response) error {
	var v struct {
		Error          string `json:"error"`
		Code           string `json:"code"`
		Operation      string `json:"operation"`
		Stage          string `json:"stage"`
		UpstreamStatus int    `json:"upstreamStatus"`
		Reason         string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&v)
	if message := proxyErrorMessage(v.Code); message != "" {
		return &Error{Status: resp.StatusCode, Code: v.Code, Message: message}
	}
	if v.Code == gdirectNotReady {
		return &Error{Status: resp.StatusCode, Code: gdirectNotReady, Message: gdirectNotReadyMessage}
	}
	if v.Code == invalidCurrentPassword {
		return &Error{Status: resp.StatusCode, Code: invalidCurrentPassword, Message: invalidCurrentPasswordMessage}
	}
	remoteAuth := v.Code == googleAuthFailed && remoteOperationLabel(v.Operation) != ""
	if v.Code == remoteOperationFailed || v.Code == outcomeUncertain || remoteAuth {
		// Remote operations share one safe diagnostic parser, including upstream
		// Google authorization failures. Never echo prose or expire local login.
		e := &Error{Status: resp.StatusCode, Code: v.Code, Reason: "unknown"}
		if remoteOperationLabel(v.Operation) != "" {
			e.Operation = v.Operation
		}
		if remoteStageLabel(v.Stage) != "" {
			e.Stage = v.Stage
		}
		if v.UpstreamStatus >= 100 && v.UpstreamStatus <= 599 {
			e.UpstreamStatus = v.UpstreamStatus
		}
		if remoteReasonLabel(v.Reason) != "" {
			e.Reason = v.Reason
		}
		return e
	}
	if v.Code == googleAuthFailed {
		// Login exchanges use LoginStage, not the remote-operation stage enum.
		// Ignore backend prose and reject arbitrary diagnostic strings.
		e := &Error{Status: resp.StatusCode, Message: "Google authentication failed", Code: v.Code, Reason: googleReason(v.Reason)}
		if googleStageLabel(v.Stage) != "" {
			e.Stage = v.Stage
		}
		if v.UpstreamStatus >= 100 && v.UpstreamStatus <= 599 {
			e.UpstreamStatus = v.UpstreamStatus
		}
		return e
	}
	fallback := v.Error == ""
	if fallback {
		v.Error = fmt.Sprintf("Server returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return &Error{Status: resp.StatusCode, Message: v.Error, Code: v.Code, fallback: fallback}
}
func (c *Client) json(ctx context.Context, method, route string, body, output any, conditions ...Preconditions) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var buf []byte
	var err error
	if body != nil {
		buf, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	var reader io.Reader
	if buf != nil {
		reader = bytes.NewReader(buf)
	}
	resp, err := c.request(ctx, method, route, reader, int64(len(buf)), conditions...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	limit := int64(32 << 20)
	strict := route == "/api/setup" || route == "/api/proxy-script" || route == "/api/upload-settings" || route == "/api/download-settings" || route == "/api/download-links" || route == "/api/admin" || route == "/api/gdirect/refresh"
	if strict {
		limit = 64 << 10
	}
	if route == "/api/proxy-script" {
		limit = 256 << 10
	}
	bounded := &io.LimitedReader{R: resp.Body, N: limit + 1}
	decoder := json.NewDecoder(bounded)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err = decoder.Decode(output); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return i18n.Err("The server returned an invalid API response")
	}
	if decoder.Decode(new(any)) != io.EOF || bounded.N <= 0 {
		return i18n.Err("The server returned an invalid API response")
	}
	return nil
}
func hideSecret(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var e *Error
	if errors.As(err, &e) {
		safe := *e
		safe.Message = strings.ReplaceAll(safe.Message, secret, "[redacted]")
		return &safe
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}
func (c *Client) Login(ctx context.Context, username, password string) error {
	var out struct {
		OK bool `json:"ok"`
	}
	err := c.json(ctx, "POST", "/api/session", map[string]string{"username": username, "password": password}, &out)
	if err != nil {
		return hideSecret(err, password)
	}
	if !out.OK {
		return i18n.Err("The server returned an invalid API response")
	}
	return nil
}
func (c *Client) Logout(ctx context.Context) error {
	return c.json(ctx, "DELETE", "/api/session", nil, nil)
}
func (c *Client) Status(ctx context.Context) (v Status, err error) {
	err = c.json(ctx, "GET", "/api/status", nil, &v)
	if err == nil && v.GDirect.IP != "" && net.ParseIP(v.GDirect.IP) == nil {
		err = i18n.Err("The server returned an invalid API response")
	}
	return
}
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var v struct {
		Accounts []Account `json:"accounts"`
	}
	err := c.json(ctx, "GET", "/api/accounts", nil, &v)
	return v.Accounts, err
}
func (c *Client) AddGoogle(ctx context.Context, token string) (Account, error) {
	var out struct {
		Account Account `json:"account"`
	}
	err := c.json(ctx, "POST", "/api/accounts/google", map[string]string{"oauth_token": token}, &out)
	return out.Account, hideSecret(err, token)
}
func (c *Client) SelectAccount(ctx context.Context, id string) error {
	return c.json(ctx, "POST", "/api/accounts/"+url.PathEscape(id)+"/select", nil, nil)
}
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	return c.json(ctx, "DELETE", "/api/accounts/"+url.PathEscape(id), nil, nil)
}
func (c *Client) Buckets(ctx context.Context) ([]Bucket, error) {
	var v struct {
		Buckets []Bucket `json:"buckets"`
	}
	err := c.json(ctx, "GET", "/api/buckets", nil, &v)
	return v.Buckets, err
}
func (c *Client) CreateBucket(ctx context.Context, name string) error {
	return c.json(ctx, "POST", "/api/buckets", map[string]string{"name": name}, nil)
}
func (c *Client) Files(ctx context.Context, p string) (v Listing, err error) {
	err = c.json(ctx, "GET", pathQuery("/api/files", p), nil, &v)
	return
}
func (c *Client) CreateDirectory(ctx context.Context, p string) error {
	return c.json(ctx, "POST", "/api/directories", map[string]string{"path": p}, nil)
}
func (c *Client) Delete(ctx context.Context, p string, conditions ...Preconditions) error {
	return c.json(ctx, "DELETE", pathQuery("/api/files", p), nil, nil, conditions...)
}
func (c *Client) Move(ctx context.Context, v Transfer, conditions ...Preconditions) error {
	return c.json(ctx, "POST", "/api/move", v, nil, conditions...)
}
func (c *Client) Copy(ctx context.Context, v Transfer, conditions ...Preconditions) error {
	return c.json(ctx, "POST", "/api/copy", v, nil, conditions...)
}

// Upload streams the file directly from its reader.
func (c *Client) Upload(ctx context.Context, p string, r io.Reader, size int64, conditions ...Preconditions) error {
	resp, err := c.request(ctx, "PUT", pathQuery("/api/files", p), r, size, conditions...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}
