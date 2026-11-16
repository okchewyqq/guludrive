package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"guludrive/core"
)

type writeSession struct {
	// mu serializes this receiver and its Commit/Abort, not the whole Store.
	mu       sync.Mutex
	s        *Store
	id, p    string
	bucketID int64
	f        *os.File
	opts     core.WriteOptions
	count    int64
	done     bool
	result   core.Entry
	ctx      context.Context
	cancel   context.CancelFunc
}

func newID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Store) BeginWrite(ctx context.Context, p string, opts core.WriteOptions) (core.WriteSession, error) {
	p, e := core.NormalizePath(p)
	if e != nil {
		return nil, e
	}
	if p == "/" || path.Dir(p) == "/" || opts.ExpectedLength < -1 || opts.ExpectedLength > s.cfg.MaxFileSize {
		return nil, core.ErrInvalid
	}
	request := ctx
	ctx, done, e := s.mutation(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	parent, e := lookup(ctx, s.db, path.Dir(p))
	if e != nil {
		return nil, e
	}
	if !parent.IsDir {
		return nil, core.ErrNotDir
	}
	existing, e := lookup(ctx, s.db, p)
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return nil, e
	}
	if e == nil && existing.IsDir {
		return nil, core.ErrIsDir
	}
	if opts.IfNoneMatch && e == nil {
		return nil, core.ErrPrecondition
	}
	if opts.ExpectedID != nil && (e != nil || existing.ID != *opts.ExpectedID) {
		return nil, core.ErrPrecondition
	}
	if opts.ExpectedVersion != nil && (e != nil || existing.Version != *opts.ExpectedVersion) {
		return nil, core.ErrPrecondition
	}
	if e != nil {
		existing = node{}
	}
	bucket, e := s.bucket(ctx, p)
	if e != nil {
		return nil, e
	}
	album := bucket.Album
	id, e := newID()
	if e != nil {
		return nil, e
	}
	spool := filepath.Join(s.cfg.DataDir, "temp", id+".raw")
	f, e := os.OpenFile(spool, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	now := time.Now()
	mt := now
	if opts.ModTime != nil {
		mt = *opts.ModTime
		v := *opts.ModTime
		opts.ModTime = &v
	}
	if opts.ExpectedID != nil {
		v := *opts.ExpectedID
		opts.ExpectedID = &v
	}
	if opts.ExpectedVersion != nil {
		v := *opts.ExpectedVersion
		opts.ExpectedVersion = &v
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO jobs(id,kind,path,state,created,updated,spool,album,account,expected_id,expected_version,content_type,mtime) VALUES(?,'write',?,'receiving',?,?,?,?,?,?,?,?,?)`, id, p, now.UnixNano(), now.UnixNano(), spool, album.ID, album.AccountID, existing.ID, existing.Version, opts.ContentType, mt.UnixNano())
	if e != nil {
		f.Close()
		os.Remove(spool)
		return nil, e
	}
	c, cancel := context.WithCancel(request)
	w := &writeSession{s: s, id: id, p: p, bucketID: bucket.ID, f: f, opts: opts, ctx: c, cancel: cancel}
	s.stateMu.Lock()
	if e = ctx.Err(); e != nil || s.closed {
		s.stateMu.Unlock()
		w.abortLocked() // Not exposed yet; still part of this admitted operation.
		if e == nil {
			e = core.ErrBusy
		}
		return nil, e
	}
	s.sessions[w] = struct{}{}
	s.stateMu.Unlock()
	context.AfterFunc(c, func() { _ = w.Abort() })
	return w, nil
}

func (w *writeSession) Write(p []byte) (int, error) {
	ctx, done, e := w.s.start(w.ctx)
	if e != nil {
		return 0, e
	}
	defer done()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, w.finishedError()
	}
	if e = ctx.Err(); e != nil {
		w.abortLocked()
		return 0, e
	}
	limit := w.s.cfg.MaxFileSize
	if w.opts.ExpectedLength >= 0 && w.opts.ExpectedLength < limit {
		limit = w.opts.ExpectedLength
	}
	if int64(len(p)) > limit-w.count {
		w.abortLocked()
		return 0, fmt.Errorf("%w: write length exceeded", core.ErrInvalid)
	}
	n, e := w.f.Write(p)
	w.count += int64(n)
	if e != nil {
		w.abortLocked()
	}
	return n, e
}

func (w *writeSession) finishedError() error {
	if w.result.ID == 0 && w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	return core.ErrConflict
}

func (w *writeSession) detach() {
	w.s.stateMu.Lock()
	delete(w.s.sessions, w)
	w.s.stateMu.Unlock()
}

// abortLocked is only used while holding w.mu (or before exposing w).
func (w *writeSession) abortLocked() error {
	if w.done {
		return nil
	}
	w.done = true
	w.cancel()
	if w.f != nil {
		w.f.Close()
	}
	w.s.setJob(context.Background(), w.id, "aborted", "write aborted")
	w.s.cleanupJob(w.id)
	w.detach()
	return nil
}

// Abort never publishes, and cancellation interrupts a concurrent commit.
func (w *writeSession) Abort() error {
	w.cancel()
	_, done, e := w.s.start(context.Background())
	if e != nil {
		// Once shutdown owns the sessions, it performs cleanup before closing DB.
		return nil
	}
	defer done()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.abortLocked()
}

func (w *writeSession) Close() error { return w.Abort() }

func (w *writeSession) Commit(ctx context.Context) (core.Entry, error) {
	s := w.s
	c, done, e := s.start(ctx)
	if e != nil {
		w.Abort()
		return core.Entry{}, e
	}
	defer done()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return core.Entry{}, w.finishedError()
	}
	// Combine request, session and store cancellation without any state lock.
	c, cancel := context.WithCancel(c)
	stop := context.AfterFunc(w.ctx, cancel)
	defer func() { stop(); cancel(); w.cancel() }()
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if e = c.Err(); e == nil {
		e = w.ctx.Err()
	}
	if e != nil {
		w.abortLocked()
		return core.Entry{}, e
	}
	bucket, err := s.bucket(c, w.p)
	if err != nil || bucket.ID != w.bucketID {
		w.abortLocked()
		return core.Entry{}, core.ErrConflict
	}
	if w.opts.ExpectedLength >= 0 && w.count != w.opts.ExpectedLength {
		w.abortLocked()
		return core.Entry{}, fmt.Errorf("%w: short input", core.ErrInvalid)
	}
	if e = w.f.Sync(); e == nil {
		e = w.f.Close()
	}
	if e == nil {
		e = syncDirectory(filepath.Dir(w.f.Name()))
	}
	if e != nil {
		w.abortLocked()
		return core.Entry{}, e
	}
	policy, e := s.cfg.UploadPolicy(c)
	if e != nil || !validPolicy(policy) {
		w.abortLocked()
		if e == nil {
			e = core.ErrInvalid
		}
		return core.Entry{}, safeError(e, "upload policy")
	}
	plans, e := s.cfg.Packer.Plan(w.count, policy)
	manifest := core.PartsManifest{Version: 1, LogicalSize: w.count, Policy: policy, Parts: make([]core.ObjectPart, len(plans))}
	for i, plan := range plans {
		manifest.Parts[i] = core.ObjectPart{Plan: plan, UploadState: "queued"}
	}
	if e == nil {
		e = validateManifest(manifest, bucket.Album.AccountID, false)
	}
	if e == nil {
		e = c.Err()
	}
	if e != nil {
		w.abortLocked()
		return core.Entry{}, safeError(e, "plan upload")
	}
	raw, e := manifestJSON(manifest)
	if e != nil {
		w.abortLocked()
		return core.Entry{}, e
	}
	tx, e := s.db.BeginTx(c, nil)
	if e == nil {
		defer tx.Rollback()
		j, err := s.intentIn(c, tx, w.id)
		if err != nil {
			e = err
		} else {
			var res sql.Result
			res, e = tx.ExecContext(c, "INSERT INTO objects(account,remote,dedup,sha1,size,format,off,length,unused_at,state,parts) VALUES(?,?,'','',?,'parts-v1',0,?,0,'staging',?)", j.album.AccountID, "group:"+w.id, w.count, w.count, raw)
			if e == nil {
				var oid int64
				oid, e = res.LastInsertId()
				if e == nil {
					_, e = tx.ExecContext(c, "UPDATE jobs SET object=?,state='sealed',length=?,updated=? WHERE id=? AND state='receiving'", oid, w.count, time.Now().UnixNano(), w.id)
				}
			}
		}
		if e == nil {
			e = tx.Commit()
		}
	}
	if e != nil {
		w.abortLocked()
		return core.Entry{}, e
	}
	// The manifest and immutable policy are durable before any carrier work.
	w.done = true
	defer w.detach()
	entry, e := s.finish(c, w.id)
	if e == nil {
		w.result = entry
	} else {
		s.failJob(w.id, e)
	}
	s.cleanupJob(w.id)
	return entry, e
}

func syncFile(p string) error {
	f, e := os.OpenFile(p, os.O_RDWR, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

func syncDirectory(p string) error {
	// Windows has no directory FlushFileBuffers through os.File.Sync.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

func sameCarrier(a, b core.Carrier) bool {
	return a.Format != "" && a.Format == b.Format && a.Size >= 0 && a.Size < 1<<63-1 && a.Size == b.Size && a.PayloadOffset >= 0 && a.PayloadOffset == b.PayloadOffset && a.PayloadSize >= 0 && a.PayloadSize == b.PayloadSize && a.PayloadOffset <= a.Size && a.PayloadSize <= a.Size-a.PayloadOffset && (a.SHA1 == "" || b.SHA1 == "" || strings.EqualFold(a.SHA1, b.SHA1))
}

func (s *Store) inspectCarrier(p string, expected core.Carrier) error {
	info, e := os.Stat(p)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Size() != expected.Size {
		return core.ErrInvalid
	}
	c, e := s.cfg.Packer.Inspect(p)
	if e != nil {
		return safeError(e, "inspect carrier")
	}
	if !sameCarrier(expected, c) {
		return core.ErrInvalid
	}
	return nil
}

func (s *Store) finish(ctx context.Context, id string) (core.Entry, error) {
	j, err := s.intent(ctx, id)
	if err != nil {
		return core.Entry{}, err
	}
	if j.state != "sealed" && j.state != "uploading" && j.state != "uploaded" && j.state != "attaching" && j.state != "attached" {
		return core.Entry{}, core.ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return core.Entry{}, err
	}
	bucket, err := s.bucket(ctx, j.p)
	if err != nil {
		return core.Entry{}, err
	}
	if bucket.Album.AccountID != j.album.AccountID || (j.album.ID != "" && j.album.ID != bucket.Album.ID) {
		return core.Entry{}, core.ErrConflict
	}
	j.album = bucket.Album
	o, err := s.object(ctx, j.object.Int64)
	if err != nil {
		return core.Entry{}, err
	}
	if o.account != j.album.AccountID || o.manifest.LogicalSize != j.carrier.PayloadSize || o.manifest.LogicalSize > s.cfg.MaxFileSize || (o.state != "staging" && o.state != "live") {
		return core.Entry{}, core.ErrInvalid
	}
	for i := range o.manifest.Parts {
		part := &o.manifest.Parts[i]
		if part.UploadState == "confirmed" {
			continue
		}
		if part.UploadState == "uploading" || part.UploadState == "uncertain" {
			return core.Entry{}, core.ErrUncertain
		}
		if err = ctx.Err(); err != nil {
			return core.Entry{}, err
		}
		dst, err := s.partPath(id, i)
		if err != nil {
			return core.Entry{}, err
		}
		if j.spool != filepath.Join(s.cfg.DataDir, "temp", id+".raw") {
			return core.Entry{}, core.ErrInvalid
		}
		if part.UploadState == "queued" || part.UploadState == "packing" {
			part.UploadState = "packing"
			if err = saveManifest(ctx, s.db, o); err != nil {
				return core.Entry{}, err
			}
			// Packing has no durable SHA-1 receipt yet. Rebuild known-unsent output
			// from the full raw spool; Inspect intentionally never hashes payloads.
			for _, p := range []string{dst + ".packing", dst} {
				if err = os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return core.Entry{}, safeError(err, "discard unsealed part")
				}
			}
			carrier, err := s.cfg.Packer.PackPart(ctx, j.spool, dst, part.Plan)
			if err != nil {
				return core.Entry{}, safeError(err, "pack part")
			}
			carrier.Path = dst
			part.Carrier = carrier
			if !validPartCarrier(*part) {
				return core.Entry{}, core.ErrInvalid
			}
			if err = syncFile(dst); err == nil {
				err = syncDirectory(filepath.Dir(dst))
			}
			if err == nil {
				err = s.inspectCarrier(dst, carrier)
			}
			if err != nil {
				return core.Entry{}, safeError(err, "seal part")
			}
			part.UploadState = "sealed"
			if err = saveManifest(ctx, s.db, o); err != nil {
				return core.Entry{}, err
			}
		}
		if part.UploadState != "sealed" {
			return core.Entry{}, core.ErrInvalid
		}
		part.Carrier.Path = dst
		if err = s.inspectCarrier(dst, part.Carrier); err != nil {
			return core.Entry{}, safeError(err, "sealed part")
		}
		part.UploadState = "uploading"
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return core.Entry{}, err
		}
		err = saveManifest(ctx, tx, o)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE jobs SET state='uploading',album=?,updated=? WHERE id=?", j.album.ID, time.Now().UnixNano(), id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return core.Entry{}, err
		}
		remote, uploadErr := s.cfg.Backend.Upload(ctx, core.UploadRequest{OperationID: fmt.Sprintf("%s.%d", id, i), Album: j.album, Carrier: part.Carrier, Filename: path.Base(j.p)})
		if uploadErr != nil {
			part.UploadState = "sealed"
			if unknownResult(ctx, uploadErr) {
				part.UploadState = "uncertain"
			}
			if err = saveManifest(context.Background(), s.db, o); err != nil {
				return core.Entry{}, core.ErrUncertain
			}
			diagnostic := safeError(uploadErr, "upload")
			if part.UploadState == "uncertain" {
				diagnostic = errors.Join(core.ErrUncertain, diagnostic)
			}
			return core.Entry{}, diagnostic
		}
		if err = s.recordUpload(j, o, i, remote); err != nil {
			return core.Entry{}, core.ErrUncertain
		}
	}
	if err = validateManifest(o.manifest, o.account, true); err != nil {
		return core.Entry{}, err
	}
	if err = ctx.Err(); err != nil {
		return core.Entry{}, err
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE jobs SET state='uploaded',updated=? WHERE id=?", time.Now().UnixNano(), id); err != nil {
		return core.Entry{}, err
	}
	binding, err := s.ensureAlbum(ctx, bucket, o, id)
	if err != nil {
		return core.Entry{}, err
	}
	j.album = binding.album
	if err = s.attachObject(ctx, j.album, o.id, true); err != nil {
		return core.Entry{}, err
	}
	if err = s.setJob(ctx, id, "attached", ""); err != nil {
		return core.Entry{}, err
	}
	if err = ctx.Err(); err != nil {
		return core.Entry{}, err
	}
	return s.publish(ctx, j)
}

var _ core.WriteSession = (*writeSession)(nil)
