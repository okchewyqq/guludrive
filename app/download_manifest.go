package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"guludrive/core"
)

// maxManifestMetadata is a serialized JSON resource bound, not a logical file
// cap. Both the service and Worker reject metadata larger than 16 MiB.
const maxManifestMetadata = 16 << 20

var (
	errProxyMetadata     = errors.New("app: unsupported Worker metadata")
	errProxyManifestSize = errors.New("app: Worker manifest exceeds metadata resource limit")
	errProxyManifest     = errors.New("app: invalid prepared manifest")
)

// publicManifestOrigin deliberately does not affect ordinary local/reverse
// proxy origins. Only an explicitly configured public HTTP(S) origin qualifies;
// request Host/Forwarded headers never authorize a public manifest origin.
func publicManifestOrigin(origin string) string {
	// Accept a pasted origin's single root slash, not a path or URL suffix.
	origin = strings.TrimSuffix(origin, "/")
	canonical, err := canonicalOrigin(origin)
	if err != nil || canonical == "" || len(canonical) > 2048 {
		return ""
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "localhost" {
		return ""
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home"} {
		if strings.HasSuffix(host, suffix) {
			return ""
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicManifestIP(ip) {
			return ""
		}
		// Match WHATWG canonical IP spelling before pinning the Worker origin.
		host = ip.String()
		if port := u.Port(); port != "" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return u.Scheme + "://" + host
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") {
			return ""
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return ""
			}
			for _, c := range label {
				if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
					return ""
				}
			}
		}
		// WHATWG treats a numeric final label as an IPv4 address, including
		// short/octal/hex spellings. Only net.ParseIP canonical literals qualify.
		last := host[strings.LastIndex(host, ".")+1:]
		numeric := strings.Trim(last, "0123456789") == ""
		hex := strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == ""
		if numeric || hex {
			return ""
		}
	}
	return canonical
}

func publicManifestIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		a, b, c := v4[0], v4[1], v4[2]
		return !(a == 0 || a >= 224 || (a == 100 && b >= 64 && b <= 127) ||
			(a == 192 && (b == 0 || (b == 88 && c == 99))) ||
			(a == 198 && (b == 18 || b == 19 || (b == 51 && c == 100))) ||
			(a == 203 && b == 0 && c == 113))
	}
	return ip[0]&0xe0 == 0x20 && !(ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8)
}

// These private wire types intentionally serialize the provider URL only at
// the scoped manifest boundary. core.DownloadSegment never serializes it.
type workerManifest struct {
	V        int    `json:"v"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Size     int64  `json:"size"`
	ETag     string `json:"etag"`
	Modified int64  `json:"modified"`
}

type workerSegment struct {
	Offset        int64  `json:"offset"`
	Size          int64  `json:"size"`
	URL           string `json:"url"`
	PayloadOffset int64  `json:"payloadOffset"`
	CarrierSize   int64  `json:"carrierSize"`
}

func manifestMetadata(entry core.Entry) (workerManifest, error) {
	kind := entry.ContentType
	if kind == "" {
		kind = "application/octet-stream"
	}
	if entry.IsDir || entry.ID <= 0 || entry.Version <= 0 || entry.Size < 0 || entry.Size > maxWorkerInteger || !utf8.ValidString(entry.Name) || len(entry.Name) > 1024 || strings.IndexFunc(entry.Name, unicode.IsControl) >= 0 || len(kind) > 255 || strings.IndexFunc(kind, func(r rune) bool { return r < 32 || r > 126 }) >= 0 || entry.ModTime.Unix() < 0 || entry.ModTime.Unix() > 253402300799 {
		return workerManifest{}, errProxyMetadata
	}
	return workerManifest{V: 2, Name: entry.Name, Type: kind, Size: entry.Size, ETag: entry.ETag(), Modified: entry.ModTime.Unix()}, nil
}

func safePreparedURL(value string) bool {
	if len(value) == 0 || len(value) > 8192 || !utf8.ValidString(value) || strings.ContainsAny(value, "\\#") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(value)
	// Google host allowlisting belongs to Backend.DownloadURL; this boundary
	// still refuses control URLs, relative URLs, userinfo and non-HTTPS schemes.
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.Opaque == "" && u.User == nil && u.Fragment == ""
}

// encodeManifest serializes one bounded segment at a time, rather than first
// marshaling an unbounded aggregate. On any error nothing is sent or truncated.
func encodeManifest(entry core.Entry, segments []core.DownloadSegment, metadataOnly bool) ([]byte, error) {
	metadata, err := manifestMetadata(entry)
	if err != nil {
		return nil, err
	}
	header, err := json.Marshal(metadata)
	if err != nil {
		return nil, errProxyManifest
	}
	if metadataOnly {
		return header, nil
	}
	// Every segment needs more than 64 JSON bytes even without a URL. Reject
	// impossible resource sizes before walking or duplicating a huge slice.
	if len(segments) > maxManifestMetadata/64 {
		return nil, errProxyManifestSize
	}
	var out bytes.Buffer
	out.Write(header[:len(header)-1])
	out.WriteString(`,"segments":[`)
	var next int64
	for index, segment := range segments {
		if segment.Offset < 0 || segment.Offset > maxWorkerInteger || segment.Size < 0 || segment.Size > maxWorkerInteger || segment.PayloadOffset < 0 || segment.PayloadOffset > maxWorkerInteger || segment.CarrierSize <= 0 || segment.CarrierSize > maxWorkerInteger {
			return nil, errProxyMetadata
		}
		if segment.Offset != next || segment.Size > entry.Size-next || (segment.Size == 0 && (entry.Size != 0 || len(segments) != 1)) || segment.PayloadOffset > segment.CarrierSize || segment.Size > segment.CarrierSize-segment.PayloadOffset || !safePreparedURL(segment.URL) {
			return nil, errProxyManifest
		}
		next += segment.Size // Safe: size <= entry.Size-next, entry.Size <= 2^53-1.
		raw, err := json.Marshal(workerSegment{Offset: segment.Offset, Size: segment.Size, URL: segment.URL, PayloadOffset: segment.PayloadOffset, CarrierSize: segment.CarrierSize})
		if err != nil {
			return nil, errProxyManifest
		}
		comma := 0
		if index != 0 {
			comma = 1
		}
		if len(raw) > maxManifestMetadata-out.Len()-comma-2 {
			return nil, errProxyManifestSize
		}
		if comma != 0 {
			out.WriteByte(',')
		}
		out.Write(raw)
	}
	if next != entry.Size {
		return nil, errProxyManifest
	}
	out.WriteString(`]}`)
	return out.Bytes(), nil
}

func proxyFailure(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": http.StatusText(status), "code": code})
}

func manifestFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errProxyMetadata):
		proxyFailure(w, http.StatusUnprocessableEntity, "proxy_unsupported_metadata")
	case errors.Is(err, errProxyManifestSize):
		proxyFailure(w, http.StatusUnprocessableEntity, "proxy_manifest_too_large")
	case errors.Is(err, core.ErrNotFound):
		proxyFailure(w, http.StatusNotFound, "proxy_manifest_unavailable")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		proxyFailure(w, http.StatusRequestTimeout, "proxy_manifest_unavailable")
	default:
		proxyFailure(w, http.StatusBadGateway, "proxy_manifest_unavailable")
	}
}

// downloadManifest grants no session or mutation authority. PrepareDownload
// owns immutable-group pinning and URL resolution; no carrier bytes are read.
func (s *Server) downloadManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", dataCSP)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpError(w, r, http.StatusMethodNotAllowed)
		return
	}
	payload, meta, valid := verifyDownload(r.URL.RawQuery, s.downloads.key, "manifest", time.Now().Unix())
	if !valid {
		httpError(w, r, http.StatusForbidden)
		return
	}
	s.downloads.mu.RLock()
	password := s.downloads.settings.workerSecret
	s.downloads.mu.RUnlock()
	if !validProxyPassword(password) {
		proxyFailure(w, http.StatusBadGateway, "proxy_manifest_unavailable")
		return
	}
	metadataOnly := meta || r.Method == http.MethodHead
	var entry core.Entry
	var segments []core.DownloadSegment
	var err error
	if metadataOnly {
		entry, err = s.fs.Stat(r.Context(), payload.Path)
	} else {
		var source core.DownloadSource
		source, err = s.fs.PrepareDownload(r.Context(), payload.Path)
		entry, segments = source.Entry, source.Segments
	}
	if err != nil {
		manifestFailure(w, err)
		return
	}
	// Check the identity returned by the SAME preparation, never a prior Stat.
	if entry.IsDir || entry.Path != payload.Path || entry.ID != payload.ID || entry.Version != payload.Version {
		httpError(w, r, http.StatusForbidden)
		return
	}
	raw, err := encodeManifest(entry, segments, metadataOnly)
	if err != nil {
		manifestFailure(w, err)
		return
	}
	// Authenticate the exact bounded metadata bytes, not a reserialized object.
	// The query parser decodes p once, exactly as capability verification does.
	mac := hmac.New(sha256.New, []byte(password))
	_, _ = mac.Write([]byte(r.URL.Query().Get("p")))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write(raw)
	w.Header().Set("X-GuluDrive-Manifest-Auth", base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}
