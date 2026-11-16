package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/emersion/go-webdav"

	"guludrive/core"
)

const dataCSP = "sandbox; default-src 'none'; base-uri 'none'; form-action 'none'"

func requestConditions(r *http.Request) (webdav.CreateOptions, error) {
	if len(r.Header.Values("If-Match")) > 1 || len(r.Header.Values("If-None-Match")) > 1 {
		return webdav.CreateOptions{}, core.ErrInvalid
	}
	return webdav.CreateOptions{IfMatch: webdav.ConditionalMatch(r.Header.Get("If-Match")), IfNoneMatch: webdav.ConditionalMatch(r.Header.Get("If-None-Match"))}, nil
}

// We intentionally support one validator or wildcard, using the DAV library's
// parser for both transports. No lock-token/If-header condition language.
func checkConditions(entry core.Entry, exists bool, opts webdav.CreateOptions) error {
	etag := ""
	if exists {
		etag = strings.Trim(entry.ETag(), `"`)
	}
	for i, condition := range []webdav.ConditionalMatch{opts.IfMatch, opts.IfNoneMatch} {
		if !condition.IsSet() {
			continue
		}
		if !condition.IsWildcard() {
			if _, err := condition.ETag(); err != nil {
				return core.ErrInvalid
			}
		}
		match, err := condition.MatchETag(etag)
		if err != nil {
			return core.ErrInvalid
		}
		if (i == 0 && !match) || (i == 1 && match) {
			return core.ErrPrecondition
		}
	}
	return nil
}

func (s *Server) stat(ctx context.Context, p string) (core.Entry, bool, error) {
	entry, err := s.fs.Stat(ctx, p)
	if errors.Is(err, core.ErrNotFound) {
		return core.Entry{}, false, nil
	}
	return entry, err == nil, err
}

func (s *Server) existing(ctx context.Context, p string, opts webdav.CreateOptions) (core.Entry, error) {
	entry, exists, err := s.stat(ctx, p)
	if err == nil {
		err = checkConditions(entry, exists, opts)
	}
	if err == nil && !exists {
		err = core.ErrNotFound
	}
	return entry, err
}

// putFile is the only API/DAV upload path. Publication happens only after full,
// bounded, uncancelled input. Identity and version are rechecked by Commit.
func (s *Server) putFile(ctx context.Context, p string, body io.ReadCloser, options core.WriteOptions, conditions webdav.CreateOptions) (core.Entry, bool, error) {
	defer body.Close()
	if p == "/" || path.Dir(p) == "/" || options.ExpectedLength < -1 {
		return core.Entry{}, false, core.ErrInvalid
	}
	if options.ExpectedLength > s.maxFile {
		return core.Entry{}, false, &http.MaxBytesError{Limit: s.maxFile}
	}
	entry, exists, err := s.stat(ctx, p)
	if err == nil {
		err = checkConditions(entry, exists, conditions)
	}
	if err != nil {
		return core.Entry{}, false, err
	}
	if exists && entry.IsDir {
		return core.Entry{}, false, core.ErrIsDir
	}
	if options.ContentType != "" {
		if _, _, err = mime.ParseMediaType(options.ContentType); err != nil {
			return core.Entry{}, false, core.ErrInvalid
		}
	}
	options.IfNoneMatch = !exists
	if exists {
		options.ExpectedID, options.ExpectedVersion = &entry.ID, &entry.Version
	}
	session, err := s.fs.BeginWrite(ctx, p, options)
	if err != nil {
		return core.Entry{}, false, err
	}
	defer session.Abort()
	var input io.Reader = &contextReader{ctx, body}
	// An unconfigured cap (or explicit MaxInt64) needs no wrapper. Never
	// increment MaxInt64; the VFS checks its int64 write count independently.
	if s.maxFile < math.MaxInt64 {
		input = io.LimitReader(input, s.maxFile+1)
	}
	n, err := io.Copy(session, input)
	if err == nil && n > s.maxFile {
		err = &http.MaxBytesError{Limit: s.maxFile}
	}
	if err == nil && options.ExpectedLength >= 0 && n != options.ExpectedLength {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return core.Entry{}, false, err
	}
	entry, err = session.Commit(ctx)
	return entry, !exists, err
}

func (s *Server) removeFile(ctx context.Context, p string, conditions webdav.CreateOptions) error {
	if p == "/" {
		return core.ErrInvalid
	}
	if _, err := s.existing(ctx, p, conditions); err != nil {
		return err
	}
	return s.fs.Remove(ctx, p)
}

func (s *Server) transferFile(ctx context.Context, in core.Transfer, move bool, conditions webdav.CreateOptions) (core.Entry, bool, error) {
	if in.Source == "" || in.Destination == "" {
		return core.Entry{}, false, core.ErrInvalid
	}
	src, err := core.NormalizePath(in.Source)
	if err != nil {
		return core.Entry{}, false, err
	}
	dst, err := core.NormalizePath(in.Destination)
	if err != nil || src == "/" || dst == "/" {
		return core.Entry{}, false, core.ErrInvalid
	}
	if _, err = s.existing(ctx, src, conditions); err != nil {
		return core.Entry{}, false, err
	}
	_, exists, err := s.stat(ctx, dst)
	if err != nil {
		return core.Entry{}, false, err
	}
	if exists && !in.Overwrite {
		return core.Entry{}, false, core.ErrPrecondition
	}
	var entry core.Entry
	if move {
		entry, err = s.fs.Move(ctx, src, dst, in.Overwrite)
	} else {
		entry, err = s.fs.Copy(ctx, src, dst, in.Overwrite, in.Recursive)
	}
	return entry, !exists, err
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	p, err := queryPath(r)
	if err != nil {
		apiFailure(w, err)
		return
	}
	entry, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		apiFailure(w, err)
		return
	}
	listing := core.Listing{Entry: entry, Entries: []core.Entry{}}
	if entry.IsDir {
		listing.Entries, err = s.fs.ReadDir(r.Context(), p)
		if err != nil {
			apiFailure(w, err)
			return
		}
		if listing.Entries == nil {
			listing.Entries = []core.Entry{}
		}
	}
	writeJSON(w, http.StatusOK, listing)
}

func (s *Server) mkdir(w http.ResponseWriter, r *http.Request) {
	var p string
	if r.URL.Path == "/api/buckets" {
		var in struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.Name == "" || in.Name == "." || in.Name == ".." || strings.ContainsAny(in.Name, "/\\") {
			apiFailure(w, core.ErrInvalid)
			return
		}
		p = "/" + in.Name
	} else {
		var in struct {
			Path string `json:"path"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		p = in.Path
	}
	p, err := core.NormalizePath(p)
	if err == nil && (p == "/" || (r.URL.Path == "/api/directories" && path.Dir(p) == "/")) {
		err = core.ErrInvalid
	}
	if err == nil {
		err = s.fs.Mkdir(r.Context(), p)
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	entry, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		apiFailure(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag())
	writeJSON(w, http.StatusCreated, map[string]any{"entry": entry})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	p, err := queryPath(r)
	conditions, conditionErr := requestConditions(r)
	if err == nil {
		err = conditionErr
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	body := r.Body
	if s.maxFile < math.MaxInt64 {
		body = http.MaxBytesReader(w, body, s.maxFile)
	}
	entry, _, err := s.putFile(r.Context(), p, body, core.WriteOptions{ExpectedLength: r.ContentLength, ContentType: r.Header.Get("Content-Type")}, conditions)
	if err != nil {
		apiFailure(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag())
	writeJSON(w, http.StatusCreated, map[string]any{"entry": entry})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	p, err := queryPath(r)
	conditions, conditionErr := requestConditions(r)
	if err == nil {
		err = conditionErr
	}
	if err == nil {
		_, err = readBody(w, r, maxJSON)
	}
	if err == nil {
		err = s.removeFile(r.Context(), p, conditions)
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) transfer(w http.ResponseWriter, r *http.Request) {
	var in core.Transfer
	if !decodeJSON(w, r, &in) {
		return
	}
	conditions, err := requestConditions(r)
	if err != nil {
		apiFailure(w, err)
		return
	}
	entry, _, err := s.transferFile(r.Context(), in, r.URL.Path == "/api/move", conditions)
	if err != nil {
		apiFailure(w, err)
		return
	}
	w.Header().Set("ETag", entry.ETag())
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", dataCSP)
	p, err := queryPath(r)
	if err != nil {
		apiFailure(w, err)
		return
	}
	if r.Method == http.MethodHead {
		if err = s.headFile(w, r, p); err != nil {
			apiFailure(w, err)
		}
		return
	}
	reader, entry, err := s.fs.Open(r.Context(), p)
	if err != nil {
		apiFailure(w, err)
		return
	}
	defer reader.Close()
	fileHeaders(w, entry)
	w, r = mp4FileRequest(w, r, entry)
	response := &fileResponse{ResponseWriter: w, ReadSeeker: reader}
	http.ServeContent(response, r, entry.Name, entry.ModTime, response)
	response.finish(r)
}

func fileHeaders(w http.ResponseWriter, entry core.Entry) {
	kind := entry.ContentType
	if kind == "" {
		kind = "application/octet-stream"
	}
	w.Header().Set("ETag", entry.ETag())
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": entry.Name}))
}

// HEAD needs only metadata. ServeContent supplies standard validators and
// lengths from this metadata-only section; HEAD never reads its empty backing.
func (s *Server) headFile(w http.ResponseWriter, r *http.Request, p string) error {
	entry, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		return err
	}
	if entry.IsDir {
		return core.ErrIsDir
	}
	fileHeaders(w, entry)
	w, r = mp4FileRequest(w, r, entry)
	http.ServeContent(w, r, entry.Name, entry.ModTime, io.NewSectionReader(bytes.NewReader(nil), 0, entry.Size))
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
