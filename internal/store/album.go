package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"

	"guludrive/core"
)

// Account lists contain safe metadata only; selecting a different account must
// never change an existing bucket's binding, including an unbound bucket.
func (s *Store) selectedAccount(ctx context.Context) (string, error) {
	accounts, err := s.cfg.Accounts.Accounts(ctx)
	if err != nil {
		return "", safeError(err, "list accounts")
	}
	var selected string
	for _, account := range accounts {
		if !account.Selected {
			continue
		}
		if !validReference(account.ID) || selected != "" {
			return "", core.ErrInvalid
		}
		selected = account.ID
	}
	if selected == "" {
		return "", core.ErrUnauthorized
	}
	return selected, ctx.Err()
}

func validReference(id string) bool {
	if len(id) == 0 || len(id) > 8192 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r < 33 || r == 127 {
			return false
		}
	}
	return true
}

// albumReceipt uses the existing journal, not a synthetic remote identity.
// expected_id on an album job is the destination bucket's entry ID (zero only
// for a COPY/MOVE that has not yet published its new top-level directory).
// object and remote_* preserve the confirmed first media identity before RPC.
type albumReceipt struct {
	id     string
	album  core.AlbumRef
	object int64
}

func albumJournalFailure() error {
	return &core.RemoteError{Operation: "create_album", Stage: "record_album", Reason: "journal_failed", Cause: core.ErrUncertain}
}

// ensureAlbum is called only under mutationMu, after every supplied object has
// been confirmed and journaled. Existing buckets are re-read, so a receiving
// writer's empty Album.ID cannot replace a binding published by another writer.
// A new transfer destination is bound in its namespace transaction instead.
func (s *Store) ensureAlbum(ctx context.Context, bucket core.Bucket, first objectRecord, writeID string) (albumReceipt, error) {
	r := albumReceipt{album: bucket.Album}
	if bucket.ID != 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT album,account FROM buckets WHERE entry=?`, bucket.ID).Scan(&r.album.ID, &r.album.AccountID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return r, core.ErrConflict
			}
			return r, err
		}
		if r.album.AccountID != bucket.Album.AccountID || (bucket.Album.ID != "" && r.album.ID != bucket.Album.ID) {
			return r, core.ErrConflict
		}
	}
	if (first.state != "live" && first.state != "staging") || validateManifest(first.manifest, first.account, true) != nil || !validReference(r.album.AccountID) || first.account != r.album.AccountID {
		return r, core.ErrInvalid
	}
	if r.album.ID != "" {
		// The first object in a confirmed CreateAlbum is already a member.
		// This also covers recovery after binding but before the write receipt.
		err := s.db.QueryRowContext(ctx, `SELECT object FROM jobs WHERE kind='album' AND state='published' AND expected_id=? AND album=? AND account=? AND object=? LIMIT 1`, bucket.ID, r.album.ID, r.album.AccountID, first.id).Scan(&r.object)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return r, err
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT id,state,album,object FROM jobs WHERE kind='album' AND object IS NOT NULL AND account=? AND ((expected_id=? AND expected_id<>0) OR (expected_id=0 AND path=?)) AND state IN ('creating','created','uncertain') ORDER BY CASE state WHEN 'created' THEN 1 ELSE 0 END,created LIMIT 1`, r.album.AccountID, bucket.ID, "/"+bucket.Name).Scan(&r.id, &state, &r.album.ID, &r.object)
	if err == nil {
		if state != "created" {
			return r, core.ErrUncertain // Never replay an unconfirmed creation.
		}
		if !validReference(r.album.ID) || r.object == 0 {
			return r, core.ErrUncertain
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	} else {
		r.id, err = newID()
		if err != nil {
			return r, err
		}
		r.object = first.id
		now := time.Now().UnixNano()
		_, err = s.db.ExecContext(ctx, `INSERT INTO jobs(id,kind,path,state,created,updated,account,expected_id,object,remote,remote_account,remote_sha1,remote_dedup,remote_size) VALUES(?,'album',?,'creating',?,?,?,?,?,?,?,?,?,?)`, r.id, "/"+bucket.Name, now, now, r.album.AccountID, bucket.ID, first.id, first.manifest.Parts[0].Remote.ID, first.account, first.manifest.Parts[0].Remote.SHA1, first.manifest.Parts[0].Remote.DedupKey, first.manifest.Parts[0].Remote.Size)
		if err != nil {
			return r, err
		}
		album, remoteErr := s.cfg.Backend.CreateAlbum(ctx, bucket.Name, []core.RemoteObject{first.manifest.Parts[0].Remote})
		if remoteErr != nil {
			state = "failed"
			if unknownResult(ctx, remoteErr) {
				state = "uncertain"
			}
			diagnostic := safeError(remoteErr, "create album")
			if state == "uncertain" && !errors.Is(diagnostic, core.ErrUncertain) {
				diagnostic = errors.Join(core.ErrUncertain, diagnostic)
			}
			if err = s.setJob(context.Background(), r.id, state, diagnostic.Error()); err != nil {
				return r, albumJournalFailure()
			}
			return r, diagnostic
		}
		if !validReference(album.ID) || album.AccountID != r.album.AccountID {
			diagnostic := &core.RemoteError{Operation: "create_album", Stage: "record_album", Reason: "invalid_album_id", Cause: core.ErrUncertain}
			s.setJob(context.Background(), r.id, "uncertain", diagnostic.Error())
			return r, diagnostic
		}
		r.album = album
	}
	// Persist both the returned identity and its initiating write before
	// binding. Recovery must not redirect that write into a deleted/recreated
	// pending bucket just because its old Album.ID was empty at BeginWrite.
	if err = s.recordAlbum(r, writeID); err != nil {
		return r, albumJournalFailure()
	}
	if bucket.ID != 0 {
		if err = s.bindAlbum(context.Background(), bucket.ID, r); err != nil {
			return r, albumJournalFailure()
		}
	}
	return r, nil
}

func (s *Store) recordAlbum(r albumReceipt, writeID string) error {
	ctx := context.Background() // A successful response outlives caller cancellation.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='created',album=?,updated=? WHERE id=? AND state IN ('creating','created')`, r.album.ID, time.Now().UnixNano(), r.id)
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return core.ErrConflict
	}
	o, err := objectFrom(ctx, tx, r.object)
	if err != nil {
		return err
	}
	if o.account != r.album.AccountID || len(o.manifest.Parts) == 0 {
		return core.ErrInvalid
	}
	membership(&o.manifest, r.album.ID)[0] = "attached"
	if writeID != "" {
		o.manifest.Parts[0].AlbumState = "attached"
	}
	if err = saveManifest(ctx, tx, o); err != nil {
		return err
	}
	if writeID != "" {
		res, err = tx.ExecContext(ctx, `UPDATE jobs SET album=?,updated=? WHERE id=? AND kind='write' AND account=? AND (album='' OR album=?) AND state='uploaded'`, r.album.ID, time.Now().UnixNano(), writeID, r.album.AccountID, r.album.ID)
		if err != nil {
			return err
		}
		if count, err := res.RowsAffected(); err != nil || count != 1 {
			return core.ErrConflict
		}
	}
	return tx.Commit()
}

// publishAlbumReceipt runs in the same short transaction that binds the bucket.
func publishAlbumReceipt(ctx context.Context, tx *sql.Tx, bucketID int64, r albumReceipt) error {
	if r.id == "" {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='published',expected_id=?,updated=? WHERE id=? AND state IN ('created','published') AND album=? AND account=?`, bucketID, time.Now().UnixNano(), r.id, r.album.ID, r.album.AccountID)
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return core.ErrConflict
	}
	return nil
}

func (s *Store) bindAlbum(ctx context.Context, bucketID int64, r albumReceipt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current core.AlbumRef
	if err = tx.QueryRowContext(ctx, `SELECT album,account FROM buckets WHERE entry=?`, bucketID).Scan(&current.ID, &current.AccountID); err != nil {
		return err
	}
	if current.AccountID != r.album.AccountID || (current.ID != "" && current.ID != r.album.ID) {
		return core.ErrConflict
	}
	if current.ID == "" {
		if _, err = tx.ExecContext(ctx, `UPDATE buckets SET album=? WHERE entry=?`, r.album.ID, bucketID); err != nil {
			return err
		}
		if err = touchAncestors(ctx, tx, bucketID); err != nil {
			return err
		}
	}
	if err = publishAlbumReceipt(ctx, tx, bucketID, r); err != nil {
		return err
	}
	return tx.Commit()
}

// Only confirmed receipts tied to an existing bucket can be recovered locally.
// Missing destinations and legacy journals stay intact; no remote RPC is sent.
func (s *Store) recoverAlbums(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id,j.album,j.account,j.object,j.expected_id FROM jobs j JOIN buckets b ON b.entry=j.expected_id WHERE j.kind='album' AND j.state='created' AND j.object IS NOT NULL AND j.album<>'' AND j.account=b.account AND (b.album='' OR b.album=j.album) ORDER BY j.created`)
	if err != nil {
		return err
	}
	type binding struct {
		r      albumReceipt
		bucket int64
	}
	var bindings []binding
	for rows.Next() {
		var b binding
		if err = rows.Scan(&b.r.id, &b.r.album.ID, &b.r.album.AccountID, &b.r.object, &b.bucket); err != nil {
			rows.Close()
			return err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if !validReference(b.r.album.ID) || !validReference(b.r.album.AccountID) {
			continue
		}
		if err = s.bindAlbum(ctx, b.bucket, b.r); err != nil {
			// Retain the created receipt. Open must not erase it or turn a
			// failed local bind into another remote album creation attempt.
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}
