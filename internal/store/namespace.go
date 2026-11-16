package store

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"guludrive/core"
)

func (s *Store) Stat(ctx context.Context, p string) (core.Entry, error) {
	p, e := core.NormalizePath(p)
	if e != nil {
		return core.Entry{}, e
	}
	ctx, done, e := s.start(ctx)
	if e != nil {
		return core.Entry{}, e
	}
	defer done()
	n, e := lookup(ctx, s.db, p)
	if e != nil || n.IsDir || !n.object.Valid {
		return n.Entry, e
	}
	o, e := s.object(ctx, n.object.Int64)
	if e != nil {
		return core.Entry{}, e
	}
	n.Entry.CarrierKind = manifestCarrierKind(o.manifest)
	return n.Entry, nil
}

func (s *Store) ReadDir(ctx context.Context, p string) ([]core.Entry, error) {
	p, e := core.NormalizePath(p)
	if e != nil {
		return nil, e
	}
	ctx, done, e := s.start(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	return s.children(ctx, p)
}

func (s *Store) ListBuckets(ctx context.Context) ([]core.Bucket, error) {
	ctx, done, e := s.start(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	rows, e := s.db.QueryContext(ctx, `SELECT e.id,e.name,b.album,b.account,b.created FROM buckets b JOIN entries e ON e.id=b.entry ORDER BY e.name COLLATE BINARY`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []core.Bucket{}
	for rows.Next() {
		var b core.Bucket
		var t int64
		if e = rows.Scan(&b.ID, &b.Name, &b.Album.ID, &b.Album.AccountID, &t); e != nil {
			return nil, e
		}
		b.CreatedAt = time.Unix(0, t).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) Mkdir(ctx context.Context, p string) error {
	p, e := core.NormalizePath(p)
	if e != nil {
		return e
	}
	if p == "/" {
		return core.ErrExists
	}
	ctx, done, e := s.mutation(ctx)
	if e != nil {
		return e
	}
	defer done()
	if _, e = lookup(ctx, s.db, p); e == nil {
		return core.ErrExists
	} else if !errors.Is(e, core.ErrNotFound) {
		return e
	}
	parent, e := lookup(ctx, s.db, path.Dir(p))
	if e != nil {
		return e
	}
	if !parent.IsDir {
		return core.ErrNotDir
	}
	var a core.AlbumRef
	if parent.ID == 1 {
		a.AccountID, e = s.selectedAccount(ctx)
		if e != nil {
			return e
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	res, e := tx.ExecContext(ctx, `INSERT INTO entries(parent,name,dir,mtime) VALUES(?,?,1,?)`, parent.ID, path.Base(p), now)
	if e != nil {
		return e
	}
	id, e := res.LastInsertId()
	if e != nil {
		return e
	}
	if parent.ID == 1 {
		if _, e = tx.ExecContext(ctx, `INSERT INTO buckets VALUES(?,?,?,?)`, id, a.ID, a.AccountID, now); e != nil {
			return e
		}
	}
	if e = touchAncestors(ctx, tx, parent.ID); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) Remove(ctx context.Context, p string) error {
	p, e := core.NormalizePath(p)
	if e != nil {
		return e
	}
	if p == "/" {
		return core.ErrInvalid
	}
	ctx, done, e := s.mutation(ctx)
	if e != nil {
		return e
	}
	defer done()
	n, e := lookup(ctx, s.db, p)
	if e != nil {
		return e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `DELETE FROM entries WHERE id=?`, n.ID); e != nil {
		return e
	}
	if e = touchAncestors(ctx, tx, n.parent); e != nil {
		return e
	}
	return tx.Commit()
}

type snapshotNode struct {
	n           node
	parentIndex int
}

func (s *Store) snapshot(ctx context.Context, p string) ([]snapshotNode, error) {
	root, e := lookup(ctx, s.db, p)
	if e != nil {
		return nil, e
	}
	out := []snapshotNode{{root, -1}}
	for i := 0; i < len(out); i++ {
		if !out[i].n.IsDir {
			continue
		}
		children, e := s.children(ctx, out[i].n.Path)
		if e != nil {
			return nil, e
		}
		for _, child := range children {
			n, e := lookup(ctx, s.db, child.Path)
			if e != nil {
				return nil, e
			}
			out = append(out, snapshotNode{n, i})
		}
	}
	return out, nil
}
func (s *Store) Copy(ctx context.Context, src, dst string, overwrite, recursive bool) (core.Entry, error) {
	return s.transfer(ctx, src, dst, overwrite, recursive, false)
}
func (s *Store) Move(ctx context.Context, src, dst string, overwrite bool) (core.Entry, error) {
	return s.transfer(ctx, src, dst, overwrite, true, true)
}
func (s *Store) transfer(ctx context.Context, src, dst string, overwrite, recursive, moving bool) (core.Entry, error) {
	var e error
	src, e = core.NormalizePath(src)
	if e != nil {
		return core.Entry{}, e
	}
	dst, e = core.NormalizePath(dst)
	if e != nil {
		return core.Entry{}, e
	}
	if src == "/" || dst == "/" {
		return core.Entry{}, core.ErrInvalid
	}
	ctx, done, e := s.mutation(ctx)
	if e != nil {
		return core.Entry{}, e
	}
	defer done()
	source, e := lookup(ctx, s.db, src)
	if e != nil {
		return core.Entry{}, e
	}
	if src == dst {
		if moving {
			return source.Entry, nil
		}
		return core.Entry{}, core.ErrConflict
	}
	if source.IsDir && !recursive {
		return core.Entry{}, core.ErrUnsupported
	}
	if source.IsDir && moving && strings.HasPrefix(dst, src+"/") {
		return core.Entry{}, core.ErrInvalid
	}
	if moving && strings.HasPrefix(src, dst+"/") {
		return core.Entry{}, core.ErrInvalid
	}
	parent, e := lookup(ctx, s.db, path.Dir(dst))
	if e != nil {
		return core.Entry{}, e
	}
	if !parent.IsDir {
		return core.Entry{}, core.ErrNotDir
	}
	if parent.ID == 1 && !source.IsDir {
		return core.Entry{}, core.ErrInvalid
	}
	target, e := lookup(ctx, s.db, dst)
	exists := e == nil
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return core.Entry{}, e
	}
	if exists && !overwrite {
		return core.Entry{}, core.ErrExists
	}
	snap, e := s.snapshot(ctx, src)
	if e != nil {
		return core.Entry{}, e
	}
	sourceAlbum, e := s.album(ctx, src)
	if e != nil {
		return core.Entry{}, e
	}
	c := ctx
	var dest core.Bucket
	newBucket := parent.ID == 1 && !exists
	// Renaming a bucket preserves even an unbound bucket's pinned account.
	renameBucket := moving && source.parent == 1 && newBucket
	if renameBucket {
		dest.Album = sourceAlbum
	} else if newBucket {
		dest.Name = path.Base(dst)
		dest.Album.AccountID, e = s.selectedAccount(c)
	} else {
		dest, e = s.bucket(c, dst)
	}
	if e != nil {
		return core.Entry{}, e
	}
	if dest.Album.AccountID != sourceAlbum.AccountID {
		// Cross-account transfer remains unsupported: a source account's
		// media keys must never be attached to another account's album.
		return core.Entry{}, core.ErrUnsupported
	}
	binding := albumReceipt{album: dest.Album}
	if dest.Album != sourceAlbum {
		seen := map[int64]bool{}
		objects := []objectRecord{}
		for _, item := range snap {
			if item.n.object.Valid && !seen[item.n.object.Int64] {
				o, e := s.object(c, item.n.object.Int64)
				if e != nil {
					return core.Entry{}, e
				}
				if o.account != dest.Album.AccountID || o.state != "live" || validateManifest(o.manifest, o.account, true) != nil {
					return core.Entry{}, core.ErrUnsupported
				}
				seen[o.id] = true
				objects = append(objects, o)
			}
		}
		if len(objects) > 0 {
			binding, e = s.ensureAlbum(c, dest, objects[0], "")
			if e != nil {
				return core.Entry{}, e
			}
			dest.Album = binding.album
			for _, object := range objects {
				if e = s.attachObject(c, dest.Album, object.id, false); e != nil {
					return core.Entry{}, e
				}
			}
		}
	}
	if e = c.Err(); e != nil {
		return core.Entry{}, e
	}
	tx, e := s.db.BeginTx(c, nil)
	if e != nil {
		return core.Entry{}, e
	}
	defer tx.Rollback()
	if exists {
		if _, e = tx.ExecContext(c, `DELETE FROM entries WHERE id=?`, target.ID); e != nil {
			return core.Entry{}, e
		}
	}
	var rootID int64
	if moving {
		rootID = source.ID
		if _, e = tx.ExecContext(c, `UPDATE entries SET parent=?,name=?,version=version+1 WHERE id=?`, parent.ID, path.Base(dst), source.ID); e != nil {
			return core.Entry{}, e
		}
		if source.parent == 1 && !renameBucket {
			if _, e = tx.ExecContext(c, `DELETE FROM buckets WHERE entry=?`, source.ID); e != nil {
				return core.Entry{}, e
			}
		}
	} else {
		ids := make([]int64, len(snap))
		for i, item := range snap {
			pid := parent.ID
			name := path.Base(dst)
			if item.parentIndex >= 0 {
				pid = ids[item.parentIndex]
				name = item.n.Name
			}
			var object any
			if item.n.object.Valid {
				object = item.n.object.Int64
			}
			res, e := tx.ExecContext(c, `INSERT INTO entries(parent,name,dir,object,size,mtime,content_type) VALUES(?,?,?,?,?,?,?)`, pid, name, item.n.IsDir, object, item.n.Size, item.n.ModTime.UnixNano(), item.n.ContentType)
			if e != nil {
				return core.Entry{}, e
			}
			ids[i], e = res.LastInsertId()
			if e != nil {
				return core.Entry{}, e
			}
		}
		rootID = ids[0]
	}
	if parent.ID == 1 && !renameBucket {
		if _, e = tx.ExecContext(c, `INSERT INTO buckets(entry,album,account,created) VALUES(?,?,?,?)`, rootID, dest.Album.ID, dest.Album.AccountID, time.Now().UnixNano()); e != nil {
			return core.Entry{}, e
		}
		if e = publishAlbumReceipt(c, tx, rootID, binding); e != nil {
			return core.Entry{}, e
		}
	}
	parents := []int64{parent.ID}
	if moving {
		parents = append(parents, source.parent)
	}
	if e = touchAncestors(c, tx, parents...); e != nil {
		return core.Entry{}, e
	}
	n, e := lookup(c, tx, dst)
	if e != nil {
		return core.Entry{}, e
	}
	if e = tx.Commit(); e != nil {
		return core.Entry{}, e
	}
	return n.Entry, nil
}
