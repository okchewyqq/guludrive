// Portions adapted from gotohp/core/api.go and gpmc/gpmc/api.py,
// Copyright (c) 2024 xob0t. See THIRD_PARTY_NOTICES.md for the MIT license.

// Package photos implements the private Android Photos protocol. It only
// operates on references supplied by guludrive's authoritative namespace/journal;
// it never scans or imports an account's existing photo library.
package photos

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"guludrive/core"
)

const rpcBase = "https://photosdata-pa.googleapis.com/6439526531001121323/"
const downloadRPC = "https://photosdata-pa.googleapis.com/$rpc/social.frontend.photos.preparedownloaddata.v1.PhotosPrepareDownloadDataService/PhotosPrepareDownload"
const uploadEndpoint = "https://photos.googleapis.com/data/upload/uploadmedia/interactive"
const maxPhotoBytes int64 = 150_000_000
const maxVideoBytes int64 = 9_000_000_000

// Existing objects are read from their journaled sizes, independently of the
// current upload caps. Keep the former outer bound for those downloads.
const maxDownloadCarrierBytes int64 = 10_000_000_000

// ErrUncertainCommit means a commit was attempted but no validated media key was
// received. It wraps core.ErrUncertain; both markers match with errors.Is.
// Never blindly retry: remote creation might already have succeeded.
var ErrUncertainCommit = fmt.Errorf("photos: commit outcome is uncertain; do not blindly retry: %w", core.ErrUncertain)

// TokenSource is the account/token port consumed by this backend. It does not
// own credential storage, login, or account selection policy.
type TokenSource interface {
	DefaultAccount(ctx context.Context) (core.Account, error)
	BearerToken(ctx context.Context, id string) (string, error)
	InvalidateBearer(ctx context.Context, id, token string) error
}

// Endpoints contains constructor-only loopback test overrides. Content is an
// allowed loopback origin for returned original download URLs; it does not
// replace the returned URL or forward RPC credentials to that origin.
type Endpoints struct{ Upload, Commit, CreateAlbum, AddToAlbum, Download, Trash, Content string }
type ClientOptions struct {
	Proxy      string
	HTTPClient *http.Client
	Endpoints  Endpoints
	// MaxDownloadBytes limits a complete existing carrier; zero defaults to the
	// former 10 GB outer bound so new 9 GB upload caps do not break old objects.
	// The actual bound is also always limited to the journaled RemoteObject.Size.
	MaxDownloadBytes int64
}

// Client manages account-bound carriers through the private Photos protocol.
type Client struct {
	tokens      TokenSource
	client      *http.Client
	endpoints   Endpoints
	maxDownload int64
}

var _ core.Backend = (*Client)(nil)

// New constructs a client; it never probes Google or lists existing media.
func New(tokens TokenSource, opts ClientOptions) (*Client, error) {
	if tokens == nil {
		return nil, fmt.Errorf("photos: token source: %w", core.ErrInvalid)
	}
	client, err := newHTTPClient(opts.Proxy, opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	ep := opts.Endpoints
	pairs := []struct {
		dst        *string
		production string
	}{{&ep.Upload, uploadEndpoint}, {&ep.Commit, rpcBase + "16538846908252377752"}, {&ep.CreateAlbum, rpcBase + "8386163679468898444"}, {&ep.AddToAlbum, rpcBase + "484917746253879292"}, {&ep.Download, downloadRPC}, {&ep.Trash, rpcBase + "17490284929287180316"}}
	for _, p := range pairs {
		*p.dst, err = testEndpoint(*p.dst, p.production)
		if err != nil {
			return nil, err
		}
	}
	if ep.Content != "" {
		ep.Content, err = testEndpoint(ep.Content, "")
		if err != nil {
			return nil, err
		}
	}
	max := opts.MaxDownloadBytes
	if max == 0 {
		max = maxDownloadCarrierBytes
	}
	if max < 1 || max > maxDownloadCarrierBytes {
		return nil, fmt.Errorf("photos: download bound: %w", core.ErrInvalid)
	}
	return &Client{tokens: tokens, client: client, endpoints: ep, maxDownload: max}, nil
}

// Preserve useful classifications, never the source's possibly secret-bearing
// message or error chain.
func tokenFailure(ctx context.Context, op string, err error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	for _, marker := range []error{context.Canceled, context.DeadlineExceeded, core.ErrUnauthorized, core.ErrInvalid, core.ErrUnsupported} {
		if errors.Is(err, marker) {
			return fmt.Errorf("photos: %s failed: %w", op, marker)
		}
	}
	return safeError(ctx, op)
}
func identifier(s string) bool {
	if len(s) == 0 || len(s) > 8192 || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c < 33 || c == 127 {
			return false
		}
	}
	return true
}

// requestResult records the conservative mutation boundary. Transport, read and
// parse failures after Do starts cannot prove rejection; a definitive HTTP 4xx
// can (except 408). Token acquisition and preparation precede that boundary.
type requestResult struct {
	raw       []byte
	headers   http.Header
	status    int
	token     string
	attempted bool
}

func (b *Client) request(ctx context.Context, account, method, endpoint string, body io.Reader, size int64, extra http.Header) (requestResult, error) {
	var result requestResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	token, err := b.tokens.BearerToken(ctx, account)
	if err != nil {
		return result, tokenFailure(ctx, "bearer acquisition", err)
	}
	result.token = token
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return result, safeError(ctx, "request preparation")
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Accept-Language", "en_US")
	req.Header.Set("User-Agent", userAgent)
	for k, vs := range extra {
		req.Header[k] = vs
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.attempted = true
	resp, err := b.client.Do(req)
	if err != nil {
		return result, &core.RemoteError{Stage: "request", Reason: "network_error", Cause: ctx.Err()}
	}
	defer resp.Body.Close()
	result.status, result.headers = resp.StatusCode, resp.Header
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var cause error
		if resp.StatusCode == 401 {
			// Invalidate for the next operation, but never replay a mutation.
			_ = b.tokens.InvalidateBearer(ctx, account, token)
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			cause = core.ErrUnauthorized
		}
		// A diagnostic read/encoding failure cannot undo a known rejection.
		// Never retain raw error bodies in result or in an error cause.
		diagnostic := &core.RemoteError{BodyFormat: "unknown"}
		raw, readErr := readBody(resp.Body, 4<<20)
		if readErr == nil {
			raw, readErr = decodeAPIResponse(raw, strings.Join(resp.Header.Values("Content-Encoding"), ","))
		}
		if readErr == nil {
			diagnostic = rejectionDetails(raw, resp.Header.Get("Content-Type"))
		}
		diagnostic.Stage, diagnostic.UpstreamStatus, diagnostic.Reason, diagnostic.Cause = "http_response", resp.StatusCode, "rejected", cause
		return result, diagnostic
	}
	result.raw, err = readBody(resp.Body, 4<<20)
	if err == nil {
		result.raw, err = decodeAPIResponse(result.raw, strings.Join(result.headers.Values("Content-Encoding"), ","))
	}
	if err != nil || ctx.Err() != nil {
		reason := "network_error"
		switch {
		case errors.Is(err, errResponseTooLarge):
			reason = "response_too_large"
		case errors.Is(err, errResponseEncoding):
			reason = "unsupported_encoding"
		case errors.Is(err, errResponseGzip):
			reason = "invalid_gzip"
		}
		return result, &core.RemoteError{Stage: "read_response", UpstreamStatus: result.status, Reason: reason, Cause: ctx.Err()}
	}
	// A successful EOF/HTTP 200 can race cancellation; rpc also checks after
	// this function's deferred Body.Close and after response decoding.
	return result, nil
}

// operationFailure is the shared mutation boundary, not a retry policy. Never
// retain the original error chain: transport/provider errors can contain secrets.
func operationFailure(operation string, result requestResult, err error, mutation bool) error {
	if err == nil || !result.attempted {
		return err
	}
	d := &core.RemoteError{Stage: "read_response", Reason: "network_error"}
	var remote *core.RemoteError
	if errors.As(err, &remote) {
		d = remote.Safe()
	}
	d.Operation, d.UpstreamStatus = operation, result.status
	for _, marker := range []error{context.Canceled, context.DeadlineExceeded, core.ErrUnauthorized} {
		if errors.Is(err, marker) {
			d.Cause = errors.Join(d.Cause, marker)
		}
	}
	definitiveRejection := result.status >= 400 && result.status < 500 && result.status != http.StatusRequestTimeout
	if mutation && !definitiveRejection {
		marker := core.ErrUncertain
		if operation == "commit_upload" {
			marker = ErrUncertainCommit
		}
		d.Cause = errors.Join(d.Cause, marker)
	}
	return d
}

func (b *Client) rpc(ctx context.Context, account, endpoint string, body []byte, safe bool) (requestResult, error) {
	if !identifier(account) {
		return requestResult{}, fmt.Errorf("photos: missing account binding: %w", core.ErrInvalid)
	}
	h := http.Header{"Content-Type": {"application/x-protobuf"}, "Accept-Encoding": {"gzip"}, "X-Goog-Ext-173412678-Bin": {"CgcIAhClARgC"}, "X-Goog-Ext-174067345-Bin": {"CgIIAg=="}}
	result, err := b.request(ctx, account, "POST", endpoint, bytes.NewReader(body), int64(len(body)), h)
	// Only read-only operations are retried, and only after a definitive 401.
	if safe && result.status == 401 {
		if e := b.tokens.InvalidateBearer(ctx, account, result.token); e != nil {
			return result, tokenFailure(ctx, "bearer invalidation", e)
		}
		result, err = b.request(ctx, account, "POST", endpoint, bytes.NewReader(body), int64(len(body)), h)
	}
	if err != nil {
		return result, err
	}
	_, err = fields(result.raw)
	// Parsing and request's deferred Body.Close may finish after cancellation.
	if e := ctx.Err(); e != nil {
		return result, &core.RemoteError{Stage: "read_response", Reason: "network_error", Cause: e}
	}
	if err != nil {
		return result, &core.RemoteError{Stage: "decode_response", Reason: "invalid_protobuf"}
	}
	return result, nil
}
func (b *Client) CreateAlbum(ctx context.Context, name string, objects []core.RemoteObject) (core.AlbumRef, error) {
	if strings.TrimSpace(name) == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") || len(objects) < 1 || len(objects) > 500 {
		return core.AlbumRef{}, core.ErrInvalid
	}
	account := objects[0].AccountID
	if !identifier(account) {
		return core.AlbumRef{}, core.ErrInvalid
	}
	var media []byte
	for _, object := range objects {
		if !identifier(object.ID) || !identifier(object.AccountID) {
			return core.AlbumRef{}, core.ErrInvalid
		}
		if object.AccountID != account {
			return core.AlbumRef{}, fmt.Errorf("photos: cross-account album creation: %w", core.ErrUnsupported)
		}
		media = append(media, blob(4, blob(1, str(1, object.ID)))...)
	}
	req := join(str(1, name), integer(2, uint64(time.Now().Unix())), integer(3, 1), media, blob(6, nil), blob(7, integer(1, 3)), blob(8, device()))
	result, err := b.rpc(ctx, account, b.endpoints.CreateAlbum, req, false)
	if err != nil {
		return core.AlbumRef{}, operationFailure("create_album", result, err, true)
	}
	// CreateAlbumResponse has the album identity only at field 1.1. Never
	// search arbitrary response fields or accept another response's identifier.
	rawID, err := bytesAt(result.raw, 1, 1)
	reason := ""
	switch {
	case err != nil:
		reason = "invalid_protobuf"
	case len(rawID) == 0:
		reason = "missing_album_id"
	case !identifier(string(rawID)):
		reason = "invalid_album_id"
	}
	if reason != "" {
		return core.AlbumRef{}, operationFailure("create_album", result, &core.RemoteError{Stage: "decode_album_id", Reason: reason}, true)
	}
	return core.AlbumRef{ID: string(rawID), AccountID: account}, nil
}

// Upload streams one BMP or MP4 carrier and commits original quality. Album attachment
// is deliberately separate so the store can journal the returned media key first.
// No stage of this mutating operation is automatically retried.
func (b *Client) Upload(ctx context.Context, req core.UploadRequest) (core.RemoteObject, error) {
	empty := core.RemoteObject{}
	if !identifier(req.Album.AccountID) || (req.Album.ID != "" && !identifier(req.Album.ID)) || req.Carrier.Size < 16 || req.Carrier.Size > maxVideoBytes || len(req.Filename) > 1024 || req.Filename == "" || !utf8.ValidString(req.Filename) || strings.ContainsAny(req.Filename, "/\\") || strings.ContainsFunc(req.Filename, unicode.IsControl) {
		return empty, core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	f, err := os.Open(req.Carrier.Path)
	if err != nil {
		return empty, errors.New("photos: cannot open carrier")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != req.Carrier.Size {
		return empty, errors.New("photos: carrier size does not match journal")
	}
	var header [32]byte
	n := min(int64(len(header)), st.Size())
	if _, err = io.ReadFull(f, header[:n]); err != nil {
		return empty, errors.New("photos: cannot read carrier header")
	}
	mime, suffix, err := carrierType(header[:n], st.Size())
	if err != nil {
		return empty, err
	}
	filename := req.Filename + suffix
	if len(filename) > 1024 {
		return empty, core.ErrInvalid
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return empty, errors.New("photos: cannot rewind carrier")
	}
	hash := sha1.New()
	if n, err = io.Copy(hash, &contextReader{ctx: ctx, r: f}); err != nil {
		return empty, safeError(ctx, "carrier hashing")
	}
	if n != st.Size() {
		return empty, errors.New("photos: carrier size changed during hashing")
	}
	sum := hash.Sum(nil)
	shaHex := hex.EncodeToString(sum)
	if req.Carrier.SHA1 != "" && !strings.EqualFold(req.Carrier.SHA1, shaHex) {
		return empty, errors.New("photos: carrier SHA-1 does not match journal")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return empty, errors.New("photos: cannot rewind carrier")
	}
	tokenReq := join(integer(1, 2), integer(2, 2), integer(3, 1), integer(4, 3), integer(7, uint64(st.Size())))
	h := http.Header{"Content-Type": {"application/x-protobuf"}, "X-Upload-Content-Type": {mime}, "X-Goog-Hash": {"sha1=" + base64.StdEncoding.EncodeToString(sum)}, "X-Upload-Content-Length": {strconv.FormatInt(st.Size(), 10)}}
	result, err := b.request(ctx, req.Album.AccountID, "POST", b.endpoints.Upload, bytes.NewReader(tokenReq), int64(len(tokenReq)), h)
	if err != nil {
		return empty, operationFailure("upload", result, err, false)
	}
	uploadID := result.headers.Get("X-GUploader-UploadID")
	if !identifier(uploadID) {
		return empty, operationFailure("upload", result, &core.RemoteError{Stage: "decode_response", Reason: "unknown"}, false)
	}
	u, err := url.Parse(b.endpoints.Upload)
	if err != nil {
		return empty, safeError(ctx, "upload preparation")
	}
	q := u.Query()
	q.Set("upload_id", uploadID)
	u.RawQuery = q.Encode()
	// Android uses chunked transfer for this PUT. Preserve that behavior instead
	// of buffering the carrier or adding a retryable GetBody function.
	count := new(uploadByteCount)
	upload := io.TeeReader(io.LimitReader(&contextReader{ctx: ctx, r: f}, st.Size()+1), count)
	result, err = b.request(ctx, req.Album.AccountID, "PUT", u.String(), upload, -1, http.Header{"Content-Type": {mime}})
	if err != nil {
		return empty, operationFailure("upload", result, err, false)
	}
	if atomic.LoadInt64((*int64)(count)) != st.Size() {
		return empty, errors.New("photos: incomplete or changed carrier upload")
	}
	finalize := result.raw
	if err = validateFinalize(finalize); err != nil {
		return empty, operationFailure("upload", result, &core.RemoteError{Stage: "decode_response", Reason: "invalid_protobuf"}, false)
	}
	blueprint := join(blob(1, finalize), str(2, filename), blob(3, sum), blob(4, join(integer(1, uint64(time.Now().Unix())), integer(2, 46000000))), integer(7, 3), integer(10, 1))
	commit := join(blob(1, blueprint), blob(2, device()), blob(3, []byte{1, 3}))
	result, err = b.rpc(ctx, req.Album.AccountID, b.endpoints.Commit, commit, false)
	if err != nil {
		return empty, operationFailure("commit_upload", result, err, true)
	}
	id, err := textAt(result.raw, 1, 3, 1)
	reason := ""
	switch {
	case err != nil:
		reason = "invalid_protobuf"
	case id == "":
		reason = "missing_media_id"
	case !identifier(id):
		reason = "invalid_media_id"
	}
	if reason != "" {
		return empty, operationFailure("commit_upload", result, &core.RemoteError{Stage: "decode_media_id", Reason: reason}, true)
	}
	// The media key confirms commit success. Optional metadata can be absent
	// or opaque; it must not discard that confirmed identity. SHA-1 URL-safe
	// base64 is gpmc's supported trash-by-hash fallback.
	dedup, err := textAt(result.raw, 1, 3, 2, 21, 1)
	if err != nil || !identifier(dedup) {
		dedup = base64.RawURLEncoding.EncodeToString(sum)
	}
	return core.RemoteObject{ID: id, AccountID: req.Album.AccountID, DedupKey: dedup, SHA1: shaHex, Size: st.Size()}, nil
}

// carrierType recognizes only the bounded first header, not the sealed payload
// framing checked by the store packer before upload. It never trusts a suffix.
func carrierType(header []byte, size int64) (mime, suffix string, err error) {
	if size >= 54 && size <= maxPhotoBytes && len(header) >= 14 && string(header[:2]) == "BM" && int64(binary.LittleEndian.Uint32(header[2:6])) == size {
		return "image/bmp", ".bmp", nil
	}
	if size >= 16 && size <= maxVideoBytes && len(header) >= 16 && string(header[4:8]) == "ftyp" {
		boxSize, minimum := uint64(binary.BigEndian.Uint32(header[:4])), uint64(16)
		if boxSize == 1 && len(header) >= 24 {
			boxSize, minimum = binary.BigEndian.Uint64(header[8:16]), 24
		}
		if boxSize >= minimum && boxSize <= uint64(len(header)) && boxSize <= uint64(size) && (boxSize-minimum)%4 == 0 {
			return "video/mp4", ".mp4", nil
		}
	}
	return "", "", fmt.Errorf("photos: invalid carrier header or size: %w", core.ErrInvalid)
}

// The transport may finish reading the request concurrently with an early
// response. Count bytes atomically without sharing LimitedReader state.
type uploadByteCount int64

func (c *uploadByteCount) Write(p []byte) (int, error) {
	atomic.AddInt64((*int64)(c), int64(len(p)))
	return len(p), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

func (b *Client) AddToAlbum(ctx context.Context, album core.AlbumRef, objects []core.RemoteObject) error {
	if !identifier(album.ID) || !identifier(album.AccountID) {
		return core.ErrInvalid
	}
	if len(objects) == 0 {
		return ctx.Err()
	}
	mediaIDs := make([]string, 0, len(objects))
	seen := make(map[string]struct{}, len(objects))
	// Validate every entry, including duplicates, before any remote mutation.
	for _, o := range objects {
		if o.AccountID != album.AccountID {
			return fmt.Errorf("photos: cross-account attachment: %w", core.ErrUnsupported)
		}
		if !identifier(o.ID) {
			return core.ErrInvalid
		}
		if _, exists := seen[o.ID]; !exists {
			seen[o.ID] = struct{}{}
			mediaIDs = append(mediaIDs, o.ID)
		}
	}
	const batchSize = 500
	for start := 0; start < len(mediaIDs); start += batchSize {
		var req []byte
		for _, id := range mediaIDs[start:min(start+batchSize, len(mediaIDs))] {
			req = append(req, str(1, id)...)
		}
		req = join(req, str(2, album.ID), blob(5, integer(1, 2)), blob(6, device()), integer(7, uint64(time.Now().Unix())))
		result, err := b.rpc(ctx, album.AccountID, b.endpoints.AddToAlbum, req, false)
		if err != nil {
			return operationFailure("add_to_album", result, err, true)
		}
	}
	return nil
}
func (b *Client) Trash(ctx context.Context, object core.RemoteObject) error {
	if !identifier(object.AccountID) || !identifier(object.ID) || !identifier(object.DedupKey) {
		return core.ErrInvalid
	}
	req := join(integer(2, 1), str(3, object.DedupKey), integer(4, 1), blob(8, blob(4, join(blob(2, nil), blob(3, blob(1, nil)), blob(4, nil), blob(5, blob(1, nil))))), blob(9, join(integer(1, 5), blob(2, join(integer(1, 49029607), str(2, "28"))))))
	result, err := b.rpc(ctx, object.AccountID, b.endpoints.Trash, req, false)
	return operationFailure("trash", result, err, true)
}

func (b *Client) allowedContent(u *url.URL) bool {
	if u == nil || u.User != nil || u.Fragment != "" || u.Hostname() == "" {
		return false
	}
	if b.endpoints.Content != "" {
		test, _ := url.Parse(b.endpoints.Content)
		if u.Scheme == test.Scheme && strings.EqualFold(u.Host, test.Host) {
			return true
		}
	}
	if u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com") || host == "ggpht.com" || strings.HasSuffix(host, ".ggpht.com") || host == "photos.google.com"
}

// DownloadURL prefers the original video at 1.5.3.5, otherwise the original
// image at 1.5.2.6. The edited image at 1.5.2.5 is never used.
// No carrier bytes are read, and the returned URL contains no RPC credentials.
func (b *Client) DownloadURL(ctx context.Context, object core.RemoteObject) (string, error) {
	if !identifier(object.ID) || !identifier(object.AccountID) || object.Size <= 0 || object.Size > b.maxDownload {
		return "", core.ErrInvalid
	}
	req := join(blob(1, blob(1, str(1, object.ID))), blob(2, join(blob(1, blob(7, blob(2, nil))), blob(5, join(blob(2, nil), blob(3, nil), blob(5, join(blob(1, nil), integer(3, 0))))))))
	result, err := b.rpc(ctx, object.AccountID, b.endpoints.Download, req, true)
	if err != nil {
		return "", operationFailure("download", result, err, false)
	}
	original, err := textAt(result.raw, 1, 5, 3, 5)
	if err == nil && original == "" {
		original, err = textAt(result.raw, 1, 5, 2, 6)
	}
	if err != nil {
		return "", operationFailure("download", result, &core.RemoteError{Stage: "decode_response", Reason: "invalid_protobuf"}, false)
	}
	if original == "" {
		return "", operationFailure("download", result, &core.RemoteError{Stage: "decode_response", Reason: "unknown"}, false)
	}
	u, err := url.Parse(original)
	if err != nil || !b.allowedContent(u) {
		return "", operationFailure("download", result, &core.RemoteError{Stage: "decode_response", Reason: "unknown"}, false)
	}
	return u.String(), nil
}

// OpenRange streams bounded full-carrier coordinates from DownloadURL. When
// Range is ignored, the full response prefix is discarded lazily; reads remain
// bounded, but upstream transfer may extend beyond the requested end until Close.
// RPC credentials are never forwarded to content URLs or their redirects.
func (b *Client) OpenRange(ctx context.Context, object core.RemoteObject, offset, length int64) (io.ReadCloser, error) {
	if !identifier(object.ID) || !identifier(object.AccountID) || object.Size <= 0 || object.Size > b.maxDownload || offset < 0 || length < 0 || offset > object.Size || length > object.Size-offset {
		return nil, core.ErrInvalid
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	original, err := b.DownloadURL(ctx, object)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(original)
	if err != nil { // DownloadURL has already validated the URL.
		return nil, safeError(ctx, "content preparation")
	}
	end := offset + (length - 1)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, end)
	for redirects := 0; redirects <= 3; redirects++ {
		request, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return nil, safeError(ctx, "content preparation")
		}
		request.Header.Set("Range", rangeHeader)
		request.Header.Set("Accept-Encoding", "identity")
		resp, err := b.client.Do(request)
		if err != nil {
			return nil, safeError(ctx, "content download")
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			next, e := url.Parse(loc)
			if e != nil || loc == "" {
				return nil, errors.New("photos: invalid content redirect")
			}
			u = u.ResolveReference(next)
			if !b.allowedContent(u) {
				return nil, errors.New("photos: untrusted content redirect")
			}
			continue
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: resp.StatusCode, Reason: "rejected"}
		}
		if resp.StatusCode == http.StatusPartialContent && (len(resp.Header.Values("Content-Range")) != 1 || !matchesContentRange(resp.Header.Get("Content-Range"), offset, end, object.Size)) {
			resp.Body.Close()
			return nil, &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: resp.StatusCode, Reason: "rejected"}
		}
		expectedLength, skip := length, int64(0)
		if resp.StatusCode == http.StatusOK {
			if len(resp.Header.Values("Content-Range")) != 0 {
				resp.Body.Close()
				return nil, &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: resp.StatusCode, Reason: "rejected"}
			}
			expectedLength, skip = object.Size, offset
		}
		encoding := resp.Header.Values("Content-Encoding")
		if resp.Uncompressed || len(encoding) > 1 || (len(encoding) == 1 && encoding[0] != "" && encoding[0] != "identity") {
			resp.Body.Close()
			return nil, &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: resp.StatusCode, Reason: "unsupported_encoding"}
		}
		if resp.ContentLength >= 0 && resp.ContentLength != expectedLength {
			resp.Body.Close()
			return nil, &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: resp.StatusCode, Reason: "rejected"}
		}
		body := &rangeBody{ctx: ctx, body: resp.Body, skip: skip, remaining: length}
		body.stop = context.AfterFunc(ctx, func() { _ = body.closeBody() })
		if err := ctx.Err(); err != nil {
			_ = body.Close()
			return nil, err
		}
		return body, nil
	}
	return nil, errors.New("photos: too many content redirects")
}

func matchesContentRange(value string, offset, end, size int64) bool {
	value, ok := strings.CutPrefix(strings.TrimSpace(value), "bytes ")
	if !ok {
		return false
	}
	interval, total, ok := strings.Cut(value, "/")
	if !ok {
		return false
	}
	start, last, ok := strings.Cut(interval, "-")
	if !ok {
		return false
	}
	var bounds [3]int64
	for i, s := range []string{start, last, total} {
		if s == "" || s[0] < '0' || s[0] > '9' {
			return false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		bounds[i] = n
	}
	return bounds == [3]int64{offset, end, size}
}

// rangeBody discards any full-response prefix, then bounds reads without
// buffering the carrier or probing beyond the requested range.
type rangeBody struct {
	ctx       context.Context
	body      io.ReadCloser
	skip      int64
	remaining int64
	stop      func() bool
	closeOnce sync.Once
	closeErr  error
}

func (r *rangeBody) closeBody() error {
	r.closeOnce.Do(func() {
		if err := r.body.Close(); err != nil {
			r.closeErr = safeError(r.ctx, "content close")
		}
	})
	return r.closeErr
}

func (r *rangeBody) Close() error {
	r.stop()
	return r.closeBody()
}

func (r *rangeBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		_ = r.Close()
		return 0, err
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if r.skip > 0 {
		// CopyN uses a bounded scratch buffer and never reads past the prefix.
		n, err := io.CopyN(io.Discard, &contextReader{ctx: r.ctx, r: r.body}, r.skip)
		r.skip -= n
		if e := r.ctx.Err(); e != nil {
			_ = r.Close()
			return 0, e
		}
		if err != nil {
			_ = r.Close()
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, safeError(r.ctx, "content read")
		}
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	if e := r.ctx.Err(); e != nil {
		_ = r.Close()
		return n, e
	}
	if (err == io.EOF && r.remaining > 0) || err == io.ErrUnexpectedEOF {
		_ = r.Close()
		return n, io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		_ = r.Close()
		return n, safeError(r.ctx, "content read")
	}
	return n, err
}
