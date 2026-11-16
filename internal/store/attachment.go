package store

import (
	"context"
	"errors"
	"time"

	"guludrive/core"
)

// attachObject records memberships independently for each album. Every bounded
// RPC has a journal before dispatch and a cancellation-independent receipt.
func (s *Store) attachObject(ctx context.Context, album core.AlbumRef, oid int64, primary bool) error {
	for {
		o, err := s.object(ctx, oid)
		if err != nil {
			return err
		}
		if o.account != album.AccountID || !validReference(album.ID) || validateManifest(o.manifest, o.account, true) != nil {
			return core.ErrInvalid
		}
		states := membership(&o.manifest, album.ID)
		var indices []int
		var remotes []core.RemoteObject
		for i, state := range states {
			if state == "attaching" || state == "uncertain" {
				return core.ErrUncertain
			}
			if state == "attached" {
				continue
			}
			if len(indices) < 500 {
				indices = append(indices, i)
				remotes = append(remotes, o.manifest.Parts[i].Remote)
			}
		}
		if len(indices) == 0 {
			return ctx.Err()
		}
		id, err := newID()
		if err != nil {
			return err
		}
		for _, i := range indices {
			states[i] = "attaching"
			if primary {
				o.manifest.Parts[i].AlbumState = "attaching"
			}
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		now := time.Now().UnixNano()
		_, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,kind,path,state,created,updated,album,account,object,off,length,remote,remote_account,remote_dedup,remote_sha1,remote_size,format) VALUES(?,'album','','attaching',?,?,?,?,?,?,?,?,?,?,?,?, 'attach')", id, now, now, album.ID, album.AccountID, oid, indices[0], len(indices), remotes[0].ID, remotes[0].AccountID, remotes[0].DedupKey, remotes[0].SHA1, remotes[0].Size)
		if err == nil {
			err = saveManifest(ctx, tx, o)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return err
		}
		remoteErr := s.cfg.Backend.AddToAlbum(ctx, album, remotes)
		state, jobState := "attached", "published"
		if remoteErr != nil {
			state, jobState = "", "failed"
			if unknownResult(ctx, remoteErr) {
				state, jobState = "uncertain", "uncertain"
			}
		}
		for _, i := range indices {
			states[i] = state
			if primary {
				o.manifest.Parts[i].AlbumState = state
			}
		}
		receiptCtx := context.Background()
		tx, err = s.db.BeginTx(receiptCtx, nil)
		if err != nil {
			return core.ErrUncertain
		}
		err = saveManifest(receiptCtx, tx, o)
		message := ""
		if remoteErr != nil {
			message = safeError(remoteErr, "attach album").Error()
		}
		if err == nil {
			_, err = tx.ExecContext(receiptCtx, "UPDATE jobs SET state=?,error=?,updated=? WHERE id=?", jobState, message, time.Now().UnixNano(), id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return core.ErrUncertain
		}
		if remoteErr != nil {
			if state == "uncertain" {
				return errors.Join(core.ErrUncertain, safeError(remoteErr, "attach album"))
			}
			return safeError(remoteErr, "attach album")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}
