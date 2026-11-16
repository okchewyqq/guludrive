package core_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"guludrive/core"
)

func TestPaths(t *testing.T) {
	for _, tt := range []struct{ input, path, bucket string }{
		{"", "/", ""},
		{"////", "/", ""},
		{"//bucket/./dir//file/", "/bucket/dir/file", "bucket"},
		{"/相册/文件.txt", "/相册/文件.txt", "相册"},
		{"/bucket/%2e%2e", "/bucket/%2e%2e", "bucket"},
	} {
		got, err := core.NormalizePath(tt.input)
		if err != nil || got != tt.path {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", tt.input, got, err, tt.path)
		}
		if got, err := core.BucketName(tt.input); err != nil || got != tt.bucket {
			t.Errorf("BucketName(%q) = %q, %v; want %q", tt.input, got, err, tt.bucket)
		}
	}
	for _, input := range []string{
		"relative", "..", "/..", "/bucket/../file", "/bucket/dir/..",
		"//bucket/./..//file", `/bucket/a\b`, "/bucket/\x00", "/bucket/\n",
		"/bucket/\u0085", "/bucket/\xff", "/bucket/\xc3", "/bucket/\xc0\xaf",
	} {
		if got, err := core.NormalizePath(input); got != "" || !errors.Is(err, core.ErrInvalid) {
			t.Errorf("NormalizePath(%q) = %q, %v; want empty, ErrInvalid", input, got, err)
		}
		if got, err := core.BucketName(input); got != "" || !errors.Is(err, core.ErrInvalid) {
			t.Errorf("BucketName(%q) = %q, %v; want empty, ErrInvalid", input, got, err)
		}
	}
}

func TestRemoteError(t *testing.T) {
	cause := errors.Join(core.ErrUncertain, errors.New("secret provider response"))
	err := &core.RemoteError{Operation: "create_album", Stage: "http_response", UpstreamStatus: 500, Reason: "rejected", BodyFormat: "protobuf", RPCCode: 3, ErrorReason: "SERVICE_DISABLED", ViolationFields: []string{"4"}, Cause: cause}
	if !errors.Is(err, core.ErrUncertain) || !strings.Contains(err.Error(), "upstreamStatus=500") || strings.Contains(err.Error(), "secret") {
		t.Fatal("remote error lost classification or exposed its cause")
	}
	raw, marshalErr := json.Marshal(err)
	if marshalErr != nil || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "Cause") {
		t.Fatal("remote error serialized a cause")
	}
	if !strings.Contains(err.Error(), "rpcCode=3") || !strings.Contains(err.Error(), "violationFields=4") {
		t.Fatal("safe RPC diagnostics were lost from journal text")
	}
	err.Operation, err.Stage, err.Reason, err.UpstreamStatus = "secret operation", "secret stage", "secret reason", 9999
	err.BodyFormat, err.RPCCode, err.ErrorReason = "secret format", 17, "OFFLINE_BEARER_SECRET"
	err.ViolationFields = []string{"fixture@example.com", "https://signed.invalid/", "request bad", strings.Repeat("A", 32), "4", "album.name", "items[0].title", "5", "ignored"}
	safe := err.Safe()
	if safe.Operation != "unknown" || safe.Stage != "unknown" || safe.Reason != "unknown" || safe.UpstreamStatus != 0 || safe.Cause != nil || safe.BodyFormat != "" || safe.RPCCode != 0 || safe.ErrorReason != "" || strings.Join(safe.ViolationFields, ",") != "4,album.name,items[0].title,5" || strings.Contains(err.Error(), "secret") {
		t.Fatal("remote diagnostic did not enforce the allowlist")
	}
}

func TestEntryETag(t *testing.T) {
	for _, tt := range []struct {
		id, version int64
		want        string
	}{
		{0, 0, `"0-0"`},
		{1, 23, `"1-23"`},
		{12, 3, `"12-3"`},
		{9223372036854775807, 7, `"9223372036854775807-7"`},
	} {
		entry := core.Entry{ID: tt.id, Version: tt.version}
		if got := entry.ETag(); got != tt.want {
			t.Errorf("Entry{%d, %d}.ETag() = %q; want %q", tt.id, tt.version, got, tt.want)
		}
		entry.Path, entry.Name, entry.Size = "/bucket/file", "file", 123
		if got := entry.ETag(); got != tt.want {
			t.Errorf("metadata changed ETag: got %q; want %q", got, tt.want)
		}
		entry.ID--
		if entry.ETag() == tt.want {
			t.Error("changing ID did not change ETag")
		}
		entry.ID++
		entry.Version++
		if entry.ETag() == tt.want {
			t.Error("changing Version did not change ETag")
		}
	}
}
