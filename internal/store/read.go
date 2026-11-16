package store

import (
	"context"
	"io"
	"math"
	"os"
	"sort"
	"sync"

	"guludrive/core"
)

// A read handle pins immutable remote metadata until Close. It opens at most
// one contiguous stream at a time, from its logical position to payload end.
// Neither Open nor Seek downloads bytes or creates local files.
type readHandle struct {
	mu          sync.Mutex
	s           *Store
	object      objectRecord
	size        int64
	pos         int64
	body        io.ReadCloser
	upstreamPos int64
	closeBody   func() error
	ctx         context.Context
	cancel      func()
	closed      bool
	err         error
}

func (r *readHandle) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if err := r.ctx.Err(); err != nil {
		return 0, safeError(err, "read")
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for len(p) > 0 && r.pos < r.size {
		index := sort.Search(len(r.object.manifest.Parts), func(i int) bool {
			q := r.object.manifest.Parts[i].Plan
			return q.Offset+q.Length > r.pos
		})
		if index == len(r.object.manifest.Parts) {
			return total, core.ErrInvalid
		}
		part := r.object.manifest.Parts[index]
		end := part.Plan.Offset + part.Plan.Length
		if r.body != nil && r.upstreamPos != r.pos {
			_ = r.closeStreamLocked()
		}
		if r.body == nil {
			body, err := r.s.cfg.Backend.OpenRange(r.ctx, part.Remote, part.Carrier.PayloadOffset+(r.pos-part.Plan.Offset), end-r.pos)
			if err != nil {
				if body != nil {
					_ = body.Close()
				}
				return total, safeError(err, "read")
			}
			if body == nil {
				return total, core.ErrInvalid
			}
			r.body, r.upstreamPos = body, r.pos
			closeBody := sync.OnceValue(body.Close)
			stop := context.AfterFunc(r.ctx, func() { _ = closeBody() })
			r.closeBody = func() error { stop(); return closeBody() }
			if err := r.ctx.Err(); err != nil {
				_ = r.closeStreamLocked()
				return total, safeError(err, "read")
			}
		}
		chunk := p
		if remain := end - r.pos; int64(len(chunk)) > remain {
			chunk = chunk[:remain]
		}
		n, err := r.body.Read(chunk)
		r.pos += int64(n)
		r.upstreamPos += int64(n)
		total += n
		p = p[n:]
		if canceled := r.ctx.Err(); canceled != nil {
			err = canceled
		}
		if r.pos == end {
			_ = r.closeStreamLocked()
			if err == io.EOF {
				err = nil
			} // A physical boundary is not logical EOF.
		} else if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return total, safeError(err, "read")
		}
		if n == 0 {
			return total, nil
		}
	}
	return total, nil
}

func (r *readHandle) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.pos
	case io.SeekEnd:
		base = r.size
	default:
		return 0, core.ErrInvalid
	}
	if offset < -base || offset > math.MaxInt64-base {
		return 0, core.ErrInvalid
	}
	pos := base + offset
	if pos != r.pos {
		_ = r.closeStreamLocked()
		r.pos = pos
	}
	return r.pos, nil
}

func (r *readHandle) closeStreamLocked() error {
	if r.body == nil {
		return nil
	}
	err := r.closeBody()
	r.body, r.closeBody = nil, nil
	if err != nil {
		return safeError(err, "close read")
	}
	return nil
}

func (r *readHandle) Close() error {
	// In particular, do not wait for a blocked OpenRange or Read before
	// canceling its context and active response body.
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.err = r.closeStreamLocked()
		r.s.stateMu.Lock()
		r.s.unpinLocked(r.object.id)
		delete(r.s.handles, r)
		r.s.stateMu.Unlock()
	}
	return r.err
}

func (s *Store) unpinLocked(id int64) {
	s.leases[id]--
	if s.leases[id] == 0 {
		delete(s.leases, id)
	}
}

func (s *Store) readBounds(n node, o objectRecord) error {
	if n.Size != o.manifest.LogicalSize || n.Size > s.cfg.MaxFileSize {
		return core.ErrInvalid
	}
	return validateManifest(o.manifest, o.account, true)
}

// Lookup, live-state validation and pinning are atomic with cleanup claims.
// Network I/O never holds stateMu.
func (s *Store) pinRead(ctx context.Context, p string) (node, objectRecord, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return node{}, objectRecord{}, core.ErrBusy
	}
	n, e := lookup(ctx, s.db, p)
	if e != nil {
		return node{}, objectRecord{}, e
	}
	if n.IsDir {
		return node{}, objectRecord{}, core.ErrIsDir
	}
	o, e := s.object(ctx, n.object.Int64)
	if e != nil {
		return node{}, objectRecord{}, e
	}
	if o.state != "live" {
		return node{}, objectRecord{}, core.ErrBusy
	}
	bucket, e := s.bucket(ctx, p)
	if e != nil {
		return node{}, objectRecord{}, e
	}
	if bucket.Album.AccountID != o.account {
		return node{}, objectRecord{}, core.ErrInvalid
	}
	n.Entry.CarrierKind = manifestCarrierKind(o.manifest)
	s.leases[o.id]++
	return n, o, nil
}

func (s *Store) Open(ctx context.Context, p string) (io.ReadSeekCloser, core.Entry, error) {
	p, e := core.NormalizePath(p)
	if e != nil {
		return nil, core.Entry{}, e
	}
	opCtx, done, e := s.start(ctx)
	if e != nil {
		return nil, core.Entry{}, e
	}
	defer done()
	n, o, e := s.pinRead(opCtx, p)
	if e != nil {
		return nil, core.Entry{}, e
	}
	owned := true
	defer func() {
		if owned {
			s.stateMu.Lock()
			s.unpinLocked(o.id)
			s.stateMu.Unlock()
		}
	}()
	// These are metadata bounds, not carrier or payload validation. In
	// particular, no format, hash or remote content inspection is needed.
	if e = s.readBounds(n, o); e != nil {
		return nil, core.Entry{}, e
	}
	s.stateMu.Lock()
	if e = opCtx.Err(); e != nil || s.closed {
		s.stateMu.Unlock()
		if e == nil {
			e = core.ErrBusy
		}
		return nil, core.Entry{}, e
	}
	// start's operation context is canceled as soon as Open returns. A read
	// handle instead combines the original caller and store lifetimes.
	lifetime, cancel := s.network(ctx)
	r := &readHandle{s: s, object: o, size: n.Size, ctx: lifetime, cancel: cancel}
	s.handles[r] = struct{}{}
	owned = false
	s.stateMu.Unlock()
	return r, n.Entry, nil
}
