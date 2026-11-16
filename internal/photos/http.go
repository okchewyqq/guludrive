// Portions derived from gotohp/core/googleauth.go, Copyright (c) 2024 xob0t.
// See THIRD_PARTY_NOTICES.md for the MIT license.
package photos

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const userAgent = "com.google.android.apps.photos/49029607 (Linux; U; Android 9; en_US; Pixel XL; Build/PQ2A.190205.001; Cronet/127.0.6510.5) (gzip)"

// newHTTPClient clones the supplied client (or the standard transport), enforces
// TLS certificate validation, and refuses all redirects. Callers must never
// return transport errors verbatim: they can contain signed URLs or proxy secrets.
// A custom RoundTripper is a trusted, constructor-only test dependency.
func newHTTPClient(proxy string, base *http.Client) (*http.Client, error) {
	c := http.Client{}
	if base != nil {
		c = *base
	}
	t := c.Transport
	if t == nil {
		t = http.DefaultTransport
	}
	if tr, ok := t.(*http.Transport); ok {
		tr = tr.Clone()
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		tr.TLSClientConfig.InsecureSkipVerify = false
		tr.TLSClientConfig.MinVersion = tls.VersionTLS12
		if tr.ResponseHeaderTimeout == 0 {
			tr.ResponseHeaderTimeout = 2 * time.Minute
		}
		if tr.MaxResponseHeaderBytes == 0 {
			tr.MaxResponseHeaderBytes = 1 << 20
		}
		if strings.TrimSpace(proxy) != "" {
			u, err := url.Parse(proxy)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				return nil, errors.New("photos: invalid proxy configuration")
			}
			tr.Proxy = http.ProxyURL(u)
		}
		c.Transport = tr
	} else if proxy != "" {
		return nil, errors.New("photos: proxy requires an HTTP transport")
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Jar = nil
	return &c, nil
}

// testEndpoint selects a production URL unless an explicit loopback endpoint
// is provided. Overrides exist for offline constructor tests, not UI settings.
func testEndpoint(override, production string) (string, error) {
	if override == "" {
		return production, nil
	}
	u, err := url.Parse(override)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("photos: invalid test endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("photos: endpoint override must be loopback")
	}
	return u.String(), nil
}

func safeError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("photos: %s failed", operation)
}

var (
	errResponseTooLarge = errors.New("photos: response too large")
	errResponseEncoding = errors.New("photos: unsupported response encoding")
	errResponseGzip     = errors.New("photos: invalid gzip response")
)

func readBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("photos: response read failed")
	}
	if int64(len(b)) > limit {
		return nil, errResponseTooLarge
	}
	return b, nil
}

func errorBodyFormat(raw []byte, contentType string) string {
	if len(raw) == 0 {
		return "empty"
	}
	kind, _, _ := mime.ParseMediaType(contentType)
	switch kind {
	case "application/x-protobuf", "application/protobuf":
		return "protobuf"
	case "text/html", "application/xhtml+xml":
		return "html"
	case "application/json", "application/problem+json":
		return "json"
	}
	if strings.HasPrefix(http.DetectContentType(raw), "text/html") {
		return "html"
	}
	return "unknown"
}

// request bounds API control responses before decoding them. The decoded body
// is independently bounded, including gzip checksum/truncation validation.
// Authentication and original-carrier downloads do not use this decoder.
func decodeAPIResponse(raw []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, errResponseGzip
		}
		defer r.Close()
		b, err := readBody(r, 4<<20)
		if errors.Is(err, errResponseTooLarge) {
			return nil, err
		}
		if err != nil {
			return nil, errResponseGzip
		}
		return b, nil
	default:
		return nil, errResponseEncoding
	}
}
