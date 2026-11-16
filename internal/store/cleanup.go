package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"guludrive/core"
)

func (s *Store) cleanup() {
	defer close(s.cleanupDone)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		_ = s.collect(s.ctx)
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recover never repeats a mutation whose result was not durably confirmed.
func (s *Store) recover(ctx context.Context) error {
	ctx, done, err := s.mutation(ctx)
	if err != nil {
		return err
	}
	defer done()
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM objects ORDER BY id")
	if err != nil {
		return err
	}
	var objects []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range objects {
		o, err := s.object(ctx, id)
		if err != nil {
			return err
		}
		changed := false
		for i := range o.manifest.Parts {
			p := &o.manifest.Parts[i]
			if p.UploadState == "uploading" {
				p.UploadState = "uncertain"
				changed = true
			}
			if p.TrashState == "trashing" {
				p.TrashState = "uncertain"
				changed = true
			}
			if p.AlbumState == "attaching" {
				p.AlbumState = "uncertain"
				changed = true
			}
		}
		for _, states := range o.manifest.AlbumStates {
			for i, state := range states {
				if state == "attaching" {
					states[i] = "uncertain"
					changed = true
				}
			}
		}
		if changed {
			if err = saveManifest(ctx, s.db, o); err != nil {
				return err
			}
		}
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE jobs SET state='uncertain',error='remote result unknown; manual reconciliation required' WHERE (kind='gc' AND state='trashing') OR (kind='album' AND state IN ('creating','attaching'))"); err != nil {
		return err
	}
	if err = s.recoverAlbums(ctx); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT id FROM jobs WHERE kind='write' AND state IN ('receiving','sealed','uploading','uploaded','attaching','attached') ORDER BY created,id")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return err
		}
		j, err := s.intent(ctx, id)
		if err != nil {
			return err
		}
		if j.state == "receiving" {
			if err = s.setJob(ctx, id, "aborted", "interrupted receiving"); err != nil {
				return err
			}
		} else {
			if _, err = s.finish(ctx, id); err != nil {
				s.failJob(id, err)
			}
		}
		s.cleanupJob(id)
	}
	rows, err = s.db.QueryContext(ctx, "SELECT id FROM jobs WHERE state IN ('published','aborted','failed','conflict','uncertain')")
	if err != nil {
		return err
	}
	ids = nil
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		s.cleanupJob(id)
	}
	return ctx.Err()
}

// A physical media ID is always an alias, even when one receipt omitted its
// dedup key. A known nonempty dedup key additionally links different media IDs.
// Scalar objects.remote is a local group identity and never reaches Backend.
type deletionDomain struct {
	account, id, key string
}

func domainOf(o core.RemoteObject) deletionDomain {
	return deletionDomain{o.AccountID, o.ID, o.DedupKey}
}

func (d deletionDomain) predicate() (string, []any) {
	// Include every same-ID alias of every dedup-linked ID, not just aliases
	// whose own optional key is populated. This is deliberately not a union of
	// conflicting dedup keys: claimDeletion blocks that inconsistent metadata.
	return "json_extract(p.value,'$.remote.accountID')=? AND (json_extract(p.value,'$.remote.id')=? OR (?<>'' AND json_extract(p.value,'$.remote.id') IN (SELECT json_extract(a.value,'$.remote.id') FROM objects ao,json_each(ao.parts,'$.parts') a WHERE json_extract(a.value,'$.remote.accountID')=? AND json_extract(a.value,'$.remote.dedupKey')=?)))", []any{d.account, d.id, d.key, d.account, d.key}
}

func (d deletionDomain) identities(objects []objectRecord) map[string]bool {
	ids := map[string]bool{d.id: true}
	for _, o := range objects {
		for _, p := range o.manifest.Parts {
			r := p.Remote
			if r.AccountID == d.account && d.key != "" && r.DedupKey == d.key {
				ids[r.ID] = true
			}
		}
	}
	return ids
}

func (d deletionDomain) matches(r core.RemoteObject, ids map[string]bool) bool {
	return r.AccountID == d.account && ids[r.ID]
}

type deletionClaim struct {
	job    string
	domain deletionDomain
	remote core.RemoteObject
	ids    map[string]bool
}

func (s *Store) domainObjects(ctx context.Context, tx *sql.Tx, d deletionDomain) ([]objectRecord, error) {
	where, args := d.predicate()
	rows, err := tx.QueryContext(ctx, "SELECT o.id,o.account,o.parts,o.state FROM objects o WHERE EXISTS(SELECT 1 FROM json_each(o.parts,'$.parts') p WHERE "+where+") ORDER BY o.id", args...)
	if err != nil {
		return nil, err
	}
	var out []objectRecord
	for rows.Next() {
		var o objectRecord
		var raw string
		if err = rows.Scan(&o.id, &o.account, &raw, &o.state); err != nil {
			rows.Close()
			return nil, err
		}
		o.manifest, err = decodeManifest(raw, o.account)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, o)
	}
	err = rows.Err()
	rows.Close()
	return out, err
}

const activeGroupJobs = "SELECT EXISTS(SELECT 1 FROM jobs WHERE object=? AND ((kind='write' AND state IN ('sealed','uploading','uploaded','attaching','attached','uncertain')) OR (kind='album' AND state IN ('creating','created','attaching','uncertain'))))"

// claimDeletion atomically checks all occurrences, namespace references, write
// journals and group read leases. There is no network under stateMu or SQL.
func (s *Store) claimDeletion(ctx context.Context, d deletionDomain) (*deletionClaim, error) {
	if !validReference(d.account) || !validReference(d.id) {
		return nil, nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Normalize a missing key only from unambiguous same-ID receipts. Keep
	// the selected candidate ID so the dispatch descriptor cannot be an
	// arbitrary keyless alias from another media ID.
	aliases, err := s.domainObjects(ctx, tx, deletionDomain{account: d.account, id: d.id})
	if err != nil {
		return nil, err
	}
	for _, o := range aliases {
		for _, p := range o.manifest.Parts {
			r := p.Remote
			if r.AccountID != d.account || r.ID != d.id || r.DedupKey == "" {
				continue
			}
			if d.key != "" && d.key != r.DedupKey {
				return nil, core.ErrConflict
			}
			d.key = r.DedupKey
		}
	}
	objects, err := s.domainObjects(ctx, tx, d)
	if err != nil || len(objects) == 0 {
		return nil, err
	}
	ids := d.identities(objects)
	// An in-flight write without a confirmed identity could have deduplicated
	// to any physical object in its account. Album-only NULL receipts do not.
	var unknown bool
	err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs j LEFT JOIN objects o ON o.id=j.object WHERE j.kind='write' AND j.state IN ('uploading','uncertain') AND (j.account=? OR j.remote_account=?) AND (j.object IS NULL OR EXISTS(SELECT 1 FROM json_each(o.parts,'$.parts') p WHERE json_extract(p.value,'$.uploadState') IN ('uploading','uncertain'))))", d.account, d.account).Scan(&unknown)
	if err != nil || unknown {
		return nil, err
	}
	claim := &deletionClaim{domain: d, ids: ids}
	for _, o := range objects {
		if s.leases[o.id] != 0 {
			return nil, nil
		}
		var referenced, protected bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM entries WHERE object=?)", o.id).Scan(&referenced); err != nil {
			return nil, err
		}
		if err = tx.QueryRowContext(ctx, activeGroupJobs, o.id).Scan(&protected); err != nil {
			return nil, err
		}
		if referenced || protected {
			return nil, nil
		}
		for _, p := range o.manifest.Parts {
			if !d.matches(p.Remote, ids) {
				continue
			}
			if p.Remote.DedupKey != "" && p.Remote.DedupKey != d.key {
				return nil, core.ErrConflict
			}
			if p.TrashState == "uncertain" || p.TrashState == "trashing" || p.UploadState != "confirmed" {
				return nil, nil
			}
			if p.TrashState != "trashed" && p.Remote.ID == d.id && p.Remote.DedupKey == d.key && claim.remote.ID == "" {
				claim.remote = p.Remote
			}
		}
	}
	if claim.remote.ID == "" {
		return nil, nil
	}
	claim.job, err = newID()
	if err != nil {
		return nil, err
	}
	for _, o := range objects {
		for i, p := range o.manifest.Parts {
			if d.matches(p.Remote, ids) {
				o.manifest.Parts[i].TrashState = "trashing"
			}
		}
		if err = saveManifest(ctx, tx, o); err != nil {
			return nil, err
		}
	}
	now := time.Now().UnixNano()
	r := claim.remote
	_, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,kind,path,state,created,updated,object,remote,remote_account,remote_dedup,remote_sha1,remote_size) VALUES(?,'gc','','trashing',?,?,?,?,?,?,?,?)", claim.job, now, now, objects[0].id, r.ID, r.AccountID, r.DedupKey, r.SHA1, r.Size)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

func (s *Store) finishDeletion(c *deletionClaim, remoteErr error) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	objects, err := s.domainObjects(ctx, tx, c.domain)
	if err != nil {
		return err
	}
	ids := c.domain.identities(objects)
	state, jobState, message := "trashed", "published", ""
	if remoteErr != nil {
		state, jobState, message = "", "failed", safeError(remoteErr, "trash").Error()
		if errors.Is(remoteErr, core.ErrUncertain) || errors.Is(remoteErr, context.Canceled) || errors.Is(remoteErr, context.DeadlineExceeded) {
			state, jobState = "uncertain", "uncertain"
		}
	}
	for _, o := range objects {
		for i, p := range o.manifest.Parts {
			if c.domain.matches(p.Remote, ids) {
				o.manifest.Parts[i].TrashState = state
			}
		}
		if err = saveManifest(ctx, tx, o); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE jobs SET state=?,error=?,updated=? WHERE id=?", jobState, message, time.Now().UnixNano(), c.job); err != nil {
		return err
	}
	return tx.Commit()
}

// Drop a group only after every known physical part has been reclaimed; empty,
// known-unsent terminal plans can also be discarded. Receipt jobs lose only FK.
func (s *Store) pruneGroups(ctx context.Context) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM objects o WHERE NOT EXISTS(SELECT 1 FROM entries WHERE object=o.id)")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if s.leases[id] != 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		o, err := objectFrom(ctx, tx, id)
		if err != nil {
			tx.Rollback()
			return err
		}
		var protected bool
		err = tx.QueryRowContext(ctx, activeGroupJobs, id).Scan(&protected)
		eligible := err == nil && !protected
		for _, p := range o.manifest.Parts {
			if p.UploadState == "uploading" || p.UploadState == "uncertain" || (p.Remote.ID != "" && p.TrashState != "trashed") {
				eligible = false
			}
		}
		if !eligible {
			tx.Rollback()
			if err != nil {
				return err
			}
			continue
		}
		_, err = tx.ExecContext(ctx, "UPDATE jobs SET object=NULL WHERE object=?", id)
		if err == nil {
			_, err = tx.ExecContext(ctx, "DELETE FROM objects WHERE id=?", id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// A single candidate snapshot prevents a rejected account from starving others.
func (s *Store) collect(ctx context.Context) error {
	ctx, done, err := s.mutation(ctx)
	if err != nil {
		return err
	}
	defer done()
	rows, err := s.db.QueryContext(ctx, "SELECT json_extract(p.value,'$.remote.accountID'),json_extract(p.value,'$.remote.id'),json_extract(p.value,'$.remote.dedupKey') FROM objects o,json_each(o.parts,'$.parts') p WHERE json_extract(p.value,'$.uploadState')='confirmed' AND json_extract(p.value,'$.trashState')='' AND NOT EXISTS(SELECT 1 FROM entries WHERE object=o.id) ORDER BY o.id,p.key")
	if err != nil {
		return err
	}
	var domains []deletionDomain
	seen := map[deletionDomain]bool{}
	for rows.Next() {
		var r core.RemoteObject
		if err = rows.Scan(&r.AccountID, &r.ID, &r.DedupKey); err != nil {
			rows.Close()
			return err
		}
		d := domainOf(r)
		if !seen[d] {
			seen[d] = true
			domains = append(domains, d)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var first error
	// Alias candidates must not retry a definite rejection in the same pass.
	attempted := map[deletionDomain]bool{}
	for _, d := range domains {
		if attempted[deletionDomain{account: d.account, id: d.id}] || (d.key != "" && attempted[deletionDomain{account: d.account, key: d.key}]) {
			continue
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		claim, err := s.claimDeletion(ctx, d)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if claim == nil {
			continue
		}
		for id := range claim.ids {
			attempted[deletionDomain{account: claim.domain.account, id: id}] = true
		}
		if claim.domain.key != "" {
			attempted[deletionDomain{account: claim.domain.account, key: claim.domain.key}] = true
		}
		remoteErr := s.cfg.Backend.Trash(ctx, claim.remote)
		if err = s.finishDeletion(claim, remoteErr); err != nil {
			if first == nil {
				first = core.ErrUncertain
			}
			continue
		}
		if remoteErr != nil && first == nil {
			first = safeError(remoteErr, "trash")
		}
	}
	if err = s.pruneGroups(ctx); err != nil && first == nil {
		first = err
	}
	return first
}
