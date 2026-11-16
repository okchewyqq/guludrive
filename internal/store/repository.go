package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"guludrive/core"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type node struct {
	core.Entry
	parent int64
	object sql.NullInt64
}

func lookup(ctx context.Context, q queryer, p string) (node, error) {
	n := node{}
	id := int64(1)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if p == "/" {
		parts = nil
	}
	scan := func(id int64, name string, root bool) error {
		query := `SELECT id,parent,name,dir,object,size,mtime,version,content_type FROM entries WHERE parent=? AND name=?`
		args := []any{id, name}
		if root {
			query = `SELECT id,parent,name,dir,object,size,mtime,version,content_type FROM entries WHERE id=1`
			args = nil
		}
		var parent sql.NullInt64
		var mt int64
		err := q.QueryRowContext(ctx, query, args...).Scan(&n.ID, &parent, &n.Name, &n.IsDir, &n.object, &n.Size, &mt, &n.Version, &n.ContentType)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrNotFound
		}
		n.parent = parent.Int64
		n.ModTime = time.Unix(0, mt).UTC()
		return err
	}
	if err := scan(1, "", true); err != nil {
		return n, err
	}
	for _, part := range parts {
		if !n.IsDir {
			return n, core.ErrNotDir
		}
		if err := scan(id, part, false); err != nil {
			return n, err
		}
		id = n.ID
	}
	n.Path = p
	n.Bucket, _ = core.BucketName(p)
	return n, nil
}

func (s *Store) children(ctx context.Context, p string) ([]core.Entry, error) {
	n, err := lookup(ctx, s.db, p)
	if err != nil {
		return nil, err
	}
	if !n.IsDir {
		return nil, core.ErrNotDir
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM entries WHERE parent=? ORDER BY name COLLATE BINARY`, n.ID)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]core.Entry, 0, len(names))
	for _, name := range names {
		child, err := lookup(ctx, s.db, path.Join(p, name))
		if err != nil {
			return nil, err
		}
		out = append(out, child.Entry)
	}
	return out, nil
}

func (s *Store) bucket(ctx context.Context, p string) (core.Bucket, error) {
	name, _ := core.BucketName(p)
	n, err := lookup(ctx, s.db, "/"+name)
	if err != nil {
		return core.Bucket{}, err
	}
	b := core.Bucket{ID: n.ID, Name: name}
	err = s.db.QueryRowContext(ctx, `SELECT album,account FROM buckets WHERE entry=?`, n.ID).Scan(&b.Album.ID, &b.Album.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrInvalid
	}
	return b, err
}

func (s *Store) album(ctx context.Context, p string) (core.AlbumRef, error) {
	b, err := s.bucket(ctx, p)
	return b.Album, err
}

type objectRecord struct {
	id       int64
	account  string
	manifest core.PartsManifest
	state    string
}

func objectFrom(ctx context.Context, q queryer, id int64) (objectRecord, error) {
	o := objectRecord{id: id}
	var raw string
	err := q.QueryRowContext(ctx, "SELECT account,parts,state FROM objects WHERE id=?", id).Scan(&o.account, &raw, &o.state)
	if err == nil {
		o.manifest, err = decodeManifest(raw, o.account)
	}
	return o, err
}

func (s *Store) object(ctx context.Context, id int64) (objectRecord, error) {
	return objectFrom(ctx, s.db, id)
}

// touchAncestors advances directory subtree revisions in the namespace mutation
// transaction. UNION deduplicates both seed parents and shared ancestors (root
// included); missing/deleted seed directories are deliberately ignored.
func touchAncestors(ctx context.Context, tx *sql.Tx, parents ...int64) error {
	if len(parents) == 0 {
		return nil
	}
	args := make([]any, 0, len(parents)+1)
	for _, id := range parents {
		args = append(args, id)
	}
	args = append(args, time.Now().UnixNano())
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(parents)), ",")
	_, err := tx.ExecContext(ctx, `WITH RECURSIVE ancestors(id) AS (
 SELECT id FROM entries WHERE dir=1 AND id IN (`+placeholders+`)
 UNION
 SELECT e.parent FROM entries e JOIN ancestors a ON e.id=a.id WHERE e.parent IS NOT NULL
) UPDATE entries SET version=version+1,mtime=? WHERE dir=1 AND id IN (SELECT id FROM ancestors)`, args...)
	return err
}

func (s *Store) setJob(ctx context.Context, id, state, msg string) error {
	_, e := s.db.ExecContext(ctx, `UPDATE jobs SET state=?,error=?,updated=? WHERE id=?`, state, msg, time.Now().UnixNano(), id)
	return e
}

type intent struct {
	id, p, state, spool         string
	carrier                     core.Carrier
	album                       core.AlbumRef
	expectedID, expectedVersion int64
	ct                          string
	mtime                       int64
	object                      sql.NullInt64
}

func (s *Store) intent(ctx context.Context, id string) (intent, error) {
	return s.intentIn(ctx, s.db, id)
}

func (s *Store) intentIn(ctx context.Context, q queryer, id string) (intent, error) {
	j := intent{id: id}
	e := q.QueryRowContext(ctx, `SELECT path,state,spool,carrier,format,sha1,size,off,length,album,account,expected_id,expected_version,content_type,mtime,object FROM jobs WHERE id=?`, id).Scan(&j.p, &j.state, &j.spool, &j.carrier.Path, &j.carrier.Format, &j.carrier.SHA1, &j.carrier.Size, &j.carrier.PayloadOffset, &j.carrier.PayloadSize, &j.album.ID, &j.album.AccountID, &j.expectedID, &j.expectedVersion, &j.ct, &j.mtime, &j.object)
	return j, e
}

func (s *Store) failJob(id string, err error) {
	var state string
	if e := s.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, id).Scan(&state); e != nil || state == "published" {
		return
	}
	if state == "uploaded" {
		// A confirmed album receipt whose local binding failed is recoverable,
		// not an unknown remote creation. Keep the uploaded write intent so
		// recovery can bind that exact receipt and complete its normal CAS.
		var confirmed bool
		if e := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM jobs a JOIN jobs w ON w.id=? WHERE a.kind='album' AND a.state='created' AND a.object=w.object AND a.account=w.account AND a.album<>'' AND a.album=w.album)`, id).Scan(&confirmed); e == nil && confirmed {
			s.db.Exec(`UPDATE jobs SET error=?,updated=? WHERE id=?`, safeError(err, "commit").Error(), time.Now().UnixNano(), id)
			return
		}
	}
	next := "failed"
	if state == "uncertain" || errors.Is(err, core.ErrUncertain) {
		next = "uncertain"
	} else if errors.Is(err, core.ErrConflict) {
		next = "conflict"
	}
	// An uploading journal remains protected if this write itself fails. On
	// recovery it becomes uncertain, never another blind Upload.
	s.setJob(context.Background(), id, next, safeError(err, "commit").Error())
}

func (s *Store) cleanupJob(id string) {
	j, err := s.intent(context.Background(), id)
	if err != nil {
		return
	}
	switch j.state {
	case "published", "aborted", "failed", "conflict", "uncertain":
		first, err := s.partPath(id, 0)
		if err != nil {
			return
		}
		os.Remove(filepath.Join(filepath.Dir(first), id+".raw"))
		if j.object.Valid {
			if o, err := s.object(context.Background(), j.object.Int64); err == nil {
				for i := range o.manifest.Parts {
					p, err := s.partPath(id, i)
					if err == nil {
						os.Remove(p)
						os.Remove(p + ".packing")
					}
				}
			}
		}
	}
}

// A successful physical receipt is saved independently of caller cancellation.
// Failure to save leaves the pre-RPC uploading marker protected from replay.
func (s *Store) recordUpload(j intent, o objectRecord, index int, remote core.RemoteObject) error {
	ctx := context.Background()
	p := &o.manifest.Parts[index]
	if !validRemote(remote, o.account, p.Carrier) {
		p.UploadState = "uncertain"
		_ = saveManifest(ctx, s.db, o)
		_, _ = s.db.ExecContext(ctx, "UPDATE jobs SET remote=?,remote_account=?,remote_sha1=?,remote_dedup=?,remote_size=? WHERE id=?", remote.ID, remote.AccountID, remote.SHA1, remote.DedupKey, remote.Size, j.id)
		return core.ErrUncertain
	}
	p.Remote, p.UploadState = remote, "confirmed"
	return saveManifest(ctx, s.db, o)
}

// publish is a short, atomic CAS transaction. The durable expected identity is
// captured at BeginWrite even without an explicit user precondition, preventing
// a same-version delete/recreate from inheriting another request's upload.
func (s *Store) publish(ctx context.Context, j intent) (core.Entry, error) {
	current, e := s.album(ctx, j.p)
	if e != nil || current.ID == "" || current != j.album {
		return core.Entry{}, core.ErrConflict
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return core.Entry{}, e
	}
	defer tx.Rollback()
	old, e := lookup(ctx, tx, j.p)
	if j.expectedID == 0 {
		if !errors.Is(e, core.ErrNotFound) {
			return core.Entry{}, core.ErrConflict
		}
	} else if e != nil || old.ID != j.expectedID || old.Version != j.expectedVersion {
		return core.Entry{}, core.ErrConflict
	}
	parent, e := lookup(ctx, tx, path.Dir(j.p))
	if e != nil || !parent.IsDir {
		return core.Entry{}, core.ErrConflict
	}
	o, e := objectFrom(ctx, tx, j.object.Int64)
	if e != nil {
		return core.Entry{}, e
	}
	if e = validateManifest(o.manifest, o.account, true); e != nil {
		return core.Entry{}, e
	}
	for _, state := range o.manifest.AlbumStates[j.album.ID] {
		if state != "attached" {
			return core.Entry{}, core.ErrConflict
		}
	}
	if len(o.manifest.AlbumStates[j.album.ID]) != len(o.manifest.Parts) {
		return core.Entry{}, core.ErrConflict
	}
	if _, e = tx.ExecContext(ctx, `UPDATE objects SET state='live' WHERE id=? AND state IN ('staging','live')`, o.id); e != nil {
		return core.Entry{}, e
	}
	if j.expectedID == 0 {
		_, e = tx.ExecContext(ctx, `INSERT INTO entries(parent,name,dir,object,size,mtime,content_type) VALUES(?,?,0,?,?,?,?)`, parent.ID, path.Base(j.p), j.object.Int64, j.carrier.PayloadSize, j.mtime, j.ct)
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE entries SET object=?,size=?,mtime=?,content_type=?,version=version+1 WHERE id=?`, j.object.Int64, j.carrier.PayloadSize, j.mtime, j.ct, j.expectedID)
	}
	if e != nil {
		return core.Entry{}, e
	}
	if e = touchAncestors(ctx, tx, parent.ID); e != nil {
		return core.Entry{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE jobs SET state='published',updated=?,error='' WHERE id=?`, time.Now().UnixNano(), j.id); e != nil {
		return core.Entry{}, e
	}
	n, e := lookup(ctx, tx, j.p)
	if e != nil {
		return core.Entry{}, e
	}
	if e = tx.Commit(); e != nil {
		return core.Entry{}, e
	}
	return n.Entry, nil
}

var _ queryer = (*sql.Tx)(nil)
