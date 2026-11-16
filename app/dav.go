package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/emersion/go-webdav"

	"guludrive/core"
)

const davMount = "/dav"

type davRequestKey struct{}
type davRequest struct {
	length      int64
	contentType string
	conditions  webdav.CreateOptions
	reader      io.ReadSeekCloser
	entry       *core.Entry
}

type davFS struct{ server *Server }

var _ webdav.FileSystem = davFS{}

func davPath(name string) (string, error) {
	if name == davMount {
		return "/", nil
	}
	if !strings.HasPrefix(name, davMount+"/") {
		return "", core.ErrInvalid
	}
	return core.NormalizePath(strings.TrimPrefix(name, davMount))
}

func davInfo(entry core.Entry) *webdav.FileInfo {
	p := davMount + entry.Path
	if entry.IsDir && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return &webdav.FileInfo{Path: p, Size: entry.Size, ModTime: entry.ModTime, IsDir: entry.IsDir, MIMEType: entry.ContentType, ETag: strings.Trim(entry.ETag(), `"`)}
}

// Provider errors never reach the library's error printer. The uncertain marker
// remains recognizable and explicitly warns DAV clients against automatic replay.
func davError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, core.ErrUncertain) {
		return webdav.NewHTTPError(http.StatusServiceUnavailable, errors.Join(core.ErrUncertain, errors.New("do not retry automatically; preserve the data directory and inspect the private journal")))
	}
	status := errorStatus(err)
	return webdav.NewHTTPError(status, errors.New(http.StatusText(status)))
}

func (s *Server) davHandler() http.Handler {
	protocol := &webdav.Handler{FileSystem: davFS{s}}
	mounted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := davPath(r.URL.Path)
		if err != nil {
			httpError(w, r, http.StatusBadRequest)
			return
		}
		switch r.Method {
		case "OPTIONS", "GET", "HEAD", "PUT", "DELETE", "PROPFIND", "MKCOL", "COPY", "MOVE":
		default: // No LOCK/UNLOCK, dead properties or PROPPATCH.
			httpError(w, r, http.StatusMethodNotAllowed)
			return
		}
		if len(r.Header.Values("If")) != 0 {
			httpError(w, r, http.StatusNotImplemented)
			return
		}
		conditions, err := requestConditions(r)
		if err != nil {
			httpError(w, r, http.StatusBadRequest)
			return
		}
		if r.Method == "COPY" || r.Method == "MOVE" {
			if err = s.validateDestination(r); err != nil {
				httpError(w, r, http.StatusBadRequest)
				return
			}
		}
		meta := &davRequest{length: r.ContentLength, contentType: r.Header.Get("Content-Type"), conditions: conditions}
		if r.Method == "PUT" {
			if r.ContentLength > s.maxFile {
				httpError(w, r, http.StatusRequestEntityTooLarge)
				return
			}
			if s.maxFile < math.MaxInt64 {
				r.Body = http.MaxBytesReader(w, r.Body, s.maxFile)
			}
		} else {
			body, err := readBody(w, r, maxJSON)
			if err != nil {
				httpError(w, r, errorStatus(err))
				return
			}
			r.Body = http.NoBody
			if len(body) != 0 {
				if r.Method == "MKCOL" {
					httpError(w, r, http.StatusUnsupportedMediaType)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		var response *fileResponse
		output := w
		if r.Method == "GET" || r.Method == "HEAD" {
			w.Header().Set("Content-Security-Policy", dataCSP)
			if r.Method == "HEAD" {
				if err = s.headFile(w, r, p); err != nil {
					status := errorStatus(err)
					if errors.Is(err, core.ErrIsDir) {
						status = http.StatusMethodNotAllowed
					}
					httpError(w, r, status)
				}
				return
			}
			reader, entry, err := s.fs.Open(r.Context(), p)
			if err != nil {
				status := errorStatus(err)
				if errors.Is(err, core.ErrIsDir) {
					status = http.StatusMethodNotAllowed
				}
				httpError(w, r, status)
				return
			}
			defer reader.Close()
			output, r = mp4FileRequest(output, r, entry)
			response = &fileResponse{ResponseWriter: output, ReadSeeker: reader}
			output = response
			meta.reader, meta.entry = davReader{response}, &entry
			fileHeaders(w, entry)
		}
		// Keep the mounted URL intact: FileInfo.Path supplies /dav hrefs and
		// the adapter trims /dav from both source and parsed Destination paths.
		protocol.ServeHTTP(output, r.WithContext(context.WithValue(r.Context(), davRequestKey{}, meta)))
		if response != nil {
			response.finish(r)
		}
	})
	return s.security.dav(s.serialize(mounted))
}

func (s *Server) validateDestination(r *http.Request) error {
	values := r.Header.Values("Destination")
	if len(values) != 1 || values[0] == "" {
		return core.ErrInvalid
	}
	u, err := url.Parse(values[0])
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(values[0], "#") {
		return core.ErrInvalid
	}
	if u.IsAbs() {
		origin, err := canonicalOrigin(u.Scheme + "://" + u.Host)
		want, wantErr := s.security.requestOrigin(r)
		if err != nil || wantErr != nil || origin != want {
			return core.ErrInvalid
		}
	} else if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return core.ErrInvalid
	}
	_, err = davPath(u.Path)
	return err
}

// The handler owns the request's streaming read handle. Keeping Seek visible enables
// the library's standard ServeContent Range/conditional-download support.
type davReader struct{ io.ReadSeeker }

func (davReader) Close() error { return nil }

func (d davFS) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	p, err := davPath(name)
	if err != nil {
		return nil, davError(err)
	}
	if meta, ok := ctx.Value(davRequestKey{}).(*davRequest); ok && meta.reader != nil && meta.entry.Path == p {
		return davReader{meta.reader}, nil
	}
	reader, _, err := d.server.fs.Open(ctx, p)
	return reader, davError(err)
}

func (d davFS) Stat(ctx context.Context, name string) (*webdav.FileInfo, error) {
	p, err := davPath(name)
	if err != nil {
		return nil, davError(err)
	}
	if meta, ok := ctx.Value(davRequestKey{}).(*davRequest); ok && meta.entry != nil && meta.entry.Path == p {
		return davInfo(*meta.entry), nil
	}
	entry, err := d.server.fs.Stat(ctx, p)
	if err != nil {
		return nil, davError(err)
	}
	return davInfo(entry), nil
}

func (d davFS) ReadDir(ctx context.Context, name string, recursive bool) ([]webdav.FileInfo, error) {
	p, err := davPath(name)
	if err != nil {
		return nil, davError(err)
	}
	entry, err := d.server.fs.Stat(ctx, p)
	if err != nil {
		return nil, davError(err)
	}
	// go-webdav expects the requested collection itself as the first result.
	result := []webdav.FileInfo{*davInfo(entry)}
	pending := []string{p}
	for len(pending) > 0 {
		if err = ctx.Err(); err != nil {
			return nil, davError(err)
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		children, err := d.server.fs.ReadDir(ctx, current)
		if err != nil {
			return nil, davError(err)
		}
		for _, child := range children {
			result = append(result, *davInfo(child))
			if recursive && child.IsDir {
				pending = append(pending, child.Path)
			}
		}
	}
	return result, nil
}

func (d davFS) Create(ctx context.Context, name string, body io.ReadCloser, opts *webdav.CreateOptions) (*webdav.FileInfo, bool, error) {
	p, err := davPath(name)
	if err != nil {
		_ = body.Close()
		return nil, false, davError(err)
	}
	options := core.WriteOptions{ExpectedLength: -1}
	if meta, ok := ctx.Value(davRequestKey{}).(*davRequest); ok {
		options.ExpectedLength, options.ContentType = meta.length, meta.contentType
	}
	if opts == nil {
		opts = &webdav.CreateOptions{}
	}
	entry, created, err := d.server.putFile(ctx, p, body, options, *opts)
	if err != nil {
		return nil, false, davError(err)
	}
	return davInfo(entry), created, nil
}

func (d davFS) RemoveAll(ctx context.Context, name string, opts *webdav.RemoveAllOptions) error {
	p, err := davPath(name)
	if err != nil {
		return davError(err)
	}
	conditions := webdav.CreateOptions{}
	if opts != nil {
		conditions.IfMatch, conditions.IfNoneMatch = opts.IfMatch, opts.IfNoneMatch
	}
	return davError(d.server.removeFile(ctx, p, conditions))
}

func (d davFS) Mkdir(ctx context.Context, name string) error {
	p, err := davPath(name)
	if err == nil {
		conditions := webdav.CreateOptions{}
		if meta, ok := ctx.Value(davRequestKey{}).(*davRequest); ok {
			conditions = meta.conditions
		}
		entry, exists, statErr := d.server.stat(ctx, p)
		err = statErr
		if err == nil {
			err = checkConditions(entry, exists, conditions)
		}
	}
	if err == nil {
		err = d.server.fs.Mkdir(ctx, p)
	}
	if errors.Is(err, core.ErrExists) {
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, errors.New("Method Not Allowed"))
	}
	if errors.Is(err, core.ErrNotFound) {
		return webdav.NewHTTPError(http.StatusConflict, errors.New("Conflict"))
	}
	return davError(err)
}

func (d davFS) Copy(ctx context.Context, name, dest string, opts *webdav.CopyOptions) (bool, error) {
	if opts == nil {
		opts = &webdav.CopyOptions{}
	}
	return d.transfer(ctx, name, dest, !opts.NoOverwrite, !opts.NoRecursive, false)
}

func (d davFS) Move(ctx context.Context, name, dest string, opts *webdav.MoveOptions) (bool, error) {
	if opts == nil {
		opts = &webdav.MoveOptions{}
	}
	return d.transfer(ctx, name, dest, !opts.NoOverwrite, true, true)
}

func (d davFS) transfer(ctx context.Context, name, dest string, overwrite, recursive, move bool) (bool, error) {
	src, err := davPath(name)
	if err != nil {
		return false, davError(err)
	}
	dst, err := davPath(dest)
	if err != nil {
		return false, davError(err)
	}
	conditions := webdav.CreateOptions{}
	if meta, ok := ctx.Value(davRequestKey{}).(*davRequest); ok {
		conditions = meta.conditions
	}
	_, created, err := d.server.transferFile(ctx, core.Transfer{Source: src, Destination: dst, Overwrite: overwrite, Recursive: recursive}, move, conditions)
	return created, davError(err)
}
