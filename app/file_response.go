package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"guludrive/core"
)

const maxDownloadPreamble = 64 << 10

// mp4FileRequest disables client range handling only for a consistently
// manifest-identified MP4 carrier. Clone the request so other handlers retain
// the original headers and all non-Range preconditions keep standard semantics.
func mp4FileRequest(w http.ResponseWriter, r *http.Request, entry core.Entry) (http.ResponseWriter, *http.Request) {
	if entry.CarrierKind != "mp4" {
		return w, r
	}
	r = r.Clone(r.Context())
	r.Header.Del("Range")
	r.Header.Del("If-Range")
	return &acceptRangesNoneWriter{ResponseWriter: w}, r
}

// acceptRangesNoneWriter overrides ServeContent at the commit boundary. Error
// responses must retain fileResponse.finish's stripped file headers.
type acceptRangesNoneWriter struct {
	http.ResponseWriter
	status int
}

func (w *acceptRangesNoneWriter) WriteHeader(status int) {
	w.status = status
	if status == http.StatusOK || status == http.StatusPartialContent {
		w.Header().Set("Accept-Ranges", "none")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *acceptRangesNoneWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
		w.Header().Set("Accept-Ranges", "none")
	}
	return w.ResponseWriter.Write(p)
}

// fileResponse keeps ServeContent's successful headers and multipart preamble
// private until a logical payload Read succeeds. It never pre-reads the VFS or
// buffers payload. ServeContent still owns all ranges and preconditions.
// Its multipart reader runs in a goroutine, so read state is synchronized.
type fileResponse struct {
	http.ResponseWriter
	io.ReadSeeker
	mu        sync.Mutex
	payload   bool
	readErr   error
	status    int
	committed bool
	preamble  []byte
	writeErr  error
}

func (w *fileResponse) Read(p []byte) (int, error) {
	n, err := w.ReadSeeker.Read(p)
	w.mu.Lock()
	if n > 0 {
		w.payload = true // Preserve bytes even when Read also returns an error.
	} else if err != nil && !w.payload && w.readErr == nil {
		w.readErr = err
	}
	w.mu.Unlock()
	return n, err
}

func (w *fileResponse) Seek(offset int64, whence int) (int64, error) {
	n, err := w.ReadSeeker.Seek(offset, whence)
	w.mu.Lock()
	if err != nil && !w.payload && w.readErr == nil {
		w.readErr = err
	}
	w.mu.Unlock()
	return n, err
}

func (w *fileResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status != http.StatusOK && status != http.StatusPartialContent {
		w.committed = true
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *fileResponse) commit() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(w.status)
	if len(w.preamble) != 0 {
		n, err := w.ResponseWriter.Write(w.preamble)
		if err == nil && n != len(w.preamble) {
			err = io.ErrShortWrite
		}
		w.preamble = nil
		return err
	}
	return nil
}

func (w *fileResponse) Write(p []byte) (int, error) {
	if !w.committed {
		w.mu.Lock()
		payload := w.payload
		w.mu.Unlock()
		if !payload {
			if len(p) > maxDownloadPreamble-len(w.preamble) {
				w.writeErr = io.ErrShortBuffer
				return 0, w.writeErr
			}
			if w.preamble == nil {
				w.preamble = make([]byte, 0, maxDownloadPreamble)
			}
			w.preamble = append(w.preamble, p...)
			return len(p), nil
		}
		if err := w.commit(); err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *fileResponse) finish(r *http.Request) {
	if w.committed {
		return // Late failures remain truncated responses, never a new status.
	}
	w.mu.Lock()
	err := w.readErr
	w.mu.Unlock()
	if ctxErr := r.Context().Err(); ctxErr != nil {
		err = ctxErr
	}
	if err == nil && w.writeErr == nil {
		_ = w.commit() // Empty files and other metadata-only responses.
		return
	}
	for _, key := range []string{"Content-Length", "Content-Range", "Content-Disposition", "ETag", "Last-Modified", "Accept-Ranges", "Content-Type", "Content-Encoding"} {
		w.Header().Del(key)
	}
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusBadGateway
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = errorStatus(err)
	}
	// No provider body, URL, credentials, or attachment metadata in errors.
	writeJSON(w.ResponseWriter, status, map[string]string{"error": http.StatusText(status)})
}
