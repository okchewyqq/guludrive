package core

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Shared operation errors can be wrapped with context; use errors.Is to match them.
var (
	ErrNotFound     = errors.New("guludrive: not found")
	ErrExists       = errors.New("guludrive: already exists")
	ErrNotDir       = errors.New("guludrive: not a directory")
	ErrIsDir        = errors.New("guludrive: is a directory")
	ErrInvalid      = errors.New("guludrive: invalid argument")
	ErrConflict     = errors.New("guludrive: conflict")
	ErrPrecondition = errors.New("guludrive: precondition failed")
	ErrBusy         = errors.New("guludrive: busy")
	ErrUnsupported  = errors.New("guludrive: unsupported operation")
	ErrUnauthorized = errors.New("guludrive: unauthorized")

	// ErrUncertain marks a remote mutation that might have succeeded. Callers
	// must preserve reconciliation state, not blindly retry or reclaim it.
	ErrUncertain = errors.New("guludrive: remote outcome uncertain")
)

// RemoteError carries safe diagnostics across the provider, filesystem and HTTP
// boundaries. Cause is for error classification only, never a provider response,
// transport error, URL or credential. It is deliberately excluded from JSON.
type RemoteError struct {
	Operation       string   `json:"operation"`
	Stage           string   `json:"stage"`
	UpstreamStatus  int      `json:"upstreamStatus"`
	Reason          string   `json:"reason"`
	BodyFormat      string   `json:"bodyFormat,omitempty"`
	RPCCode         int      `json:"rpcCode,omitempty"`
	ErrorReason     string   `json:"errorReason,omitempty"`
	ViolationFields []string `json:"violationFields,omitempty"`
	Cause           error    `json:"-"`
}

var (
	remoteReasonCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,62}$`)
	// Numeric indices only: map keys and request values are not schema paths.
	remoteFieldPath = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]{0,30}|[0-9]{1,31})(\[[0-9]{1,31}\])*(\.([A-Za-z_][A-Za-z0-9_]{0,30}|[0-9]{1,31})(\[[0-9]{1,31}\])*)*$`)
)

// Safe returns an allowlisted diagnostic snapshot without an error cause. A
// boundary retaining classifications must attach only known shared sentinels.
func (e *RemoteError) Safe() *RemoteError {
	d := &RemoteError{Operation: "unknown", Stage: "unknown", Reason: "unknown"}
	if e == nil {
		return d
	}
	switch e.Operation {
	case "create_album", "upload", "commit_upload", "add_to_album", "trash", "download":
		d.Operation = e.Operation
	}
	switch e.Stage {
	case "request", "http_response", "read_response", "decode_response", "decode_album_id", "decode_media_id", "record_album":
		d.Stage = e.Stage
	}
	if e.UpstreamStatus >= 100 && e.UpstreamStatus <= 599 {
		d.UpstreamStatus = e.UpstreamStatus
	}
	switch e.Reason {
	case "rejected", "network_error", "response_too_large", "invalid_gzip", "unsupported_encoding", "invalid_protobuf", "missing_album_id", "invalid_album_id", "missing_media_id", "invalid_media_id", "journal_failed":
		d.Reason = e.Reason
	}
	switch e.BodyFormat {
	case "protobuf", "html", "json", "empty", "unknown":
		d.BodyFormat = e.BodyFormat
	}
	if e.RPCCode > 0 && e.RPCCode <= 16 {
		d.RPCCode = e.RPCCode
	}
	if remoteReasonCode.MatchString(e.ErrorReason) {
		// Recognized Google RPC/API machine codes only, not arbitrary strings
		// that merely resemble a code. Unknown service-specific reasons vanish.
		switch e.ErrorReason {
		case "INVALID_ARGUMENT", "API_KEY_INVALID", "SERVICE_DISABLED", "BILLING_DISABLED", "RATE_LIMIT_EXCEEDED", "RESOURCE_QUOTA_EXCEEDED", "CONSUMER_INVALID", "ACCESS_TOKEN_EXPIRED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "MISSING_AUTHENTICATION_CREDENTIALS", "IAM_PERMISSION_DENIED":
			d.ErrorReason = e.ErrorReason
		}
	}
	for _, field := range e.ViolationFields {
		if len(field) <= 96 && remoteFieldPath.MatchString(field) && !slices.Contains(d.ViolationFields, field) {
			d.ViolationFields = append(d.ViolationFields, field)
			if len(d.ViolationFields) == 4 {
				break
			}
		}
	}
	return d
}

func (e *RemoteError) Error() string {
	d := e.Safe()
	outcome := "remote operation failed"
	if e != nil && errors.Is(e.Cause, ErrUncertain) {
		outcome = "remote outcome uncertain"
	}
	details := fmt.Sprintf("operation=%s stage=%s upstreamStatus=%d reason=%s", d.Operation, d.Stage, d.UpstreamStatus, d.Reason)
	if d.BodyFormat != "" {
		details += " bodyFormat=" + d.BodyFormat
	}
	if d.RPCCode != 0 {
		details += fmt.Sprintf(" rpcCode=%d", d.RPCCode)
	}
	if d.ErrorReason != "" {
		details += " errorReason=" + d.ErrorReason
	}
	if len(d.ViolationFields) != 0 {
		details += " violationFields=" + strings.Join(d.ViolationFields, ",")
	}
	return "guludrive: " + outcome + " (" + details + ")"
}

func (e *RemoteError) Unwrap() error { return e.Cause }
