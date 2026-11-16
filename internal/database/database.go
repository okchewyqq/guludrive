// Package database owns the single private store.db connection and schema.
// Recoverable Google credentials are stored in plaintext by design: the database,
// its SQLite sidecars and all backups are sensitive. POSIX permissions are
// restricted here; Windows operators must also restrict the private directory ACL.
// Callers hold the instance lock and close all borrowers before closing the DB.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens dataDir/store.db for the service owner. The caller must already
// hold the instance lock and keep the returned database open until all borrowers
// have closed. Only a new empty database or the current schema is accepted.
func Open(ctx context.Context, dataDir string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dataDir == "" {
		return nil, errors.New("database: private directory is required")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil || filepath.Dir(root) == root {
		return nil, errors.New("database: invalid private directory")
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, errors.New("database: cannot create private directory")
	}
	if st, e := os.Lstat(root); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("database: private directory must not be a symlink")
	}
	if resolved, e := filepath.EvalSymlinks(root); e != nil || filepath.Dir(resolved) == resolved {
		return nil, errors.New("database: invalid private directory")
	}
	if err = os.Chmod(root, 0700); err != nil {
		return nil, errors.New("database: cannot restrict private directory")
	}
	name := filepath.Join(root, "store.db")
	if _, err = os.Lstat(name); errors.Is(err, os.ErrNotExist) {
		f, e := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, errors.New("database: cannot create private database")
		}
		if e = f.Close(); e != nil {
			return nil, errors.New("database: cannot close private database file")
		}
	} else if err != nil {
		return nil, errors.New("database: cannot inspect private database")
	}
	if err = restrictFiles(name); err != nil {
		return nil, err
	}
	dbPath := filepath.ToSlash(name)
	if !strings.HasPrefix(dbPath, "/") {
		dbPath = "/" + dbPath
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	q := url.Values{}
	for _, pragma := range []string{"foreign_keys(1)", "busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	opened := false
	defer func() {
		if !opened {
			_ = db.Close()
		}
	}()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	switch version {
	case 0:
		var populated bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*')`).Scan(&populated); err != nil {
			return nil, err
		}
		if populated {
			return nil, errors.New("database: unsupported non-empty schema version 0")
		}
	case 3:
	default:
		return nil, fmt.Errorf("database: unsupported schema version %d", version)
	}
	if err = Initialize(ctx, db); err != nil {
		return nil, err
	}
	if err = restrictFiles(name); err != nil {
		return nil, err
	}
	opened = true
	return db, nil
}

func restrictFiles(name string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		st, err := os.Lstat(name + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("database: private database files must be regular files")
		}
		if err = os.Chmod(name+suffix, 0600); err != nil {
			return errors.New("database: cannot restrict private database files")
		}
	}
	return nil
}

// Initialize atomically initializes empty version 0 or reaffirms current version 3.
// Existing records, IDs and timestamps are retained. It never reads files or
// starts work.
func Initialize(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database: database is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 3 {
		return fmt.Errorf("database: unsupported schema version %d", version)
	}
	if version == 0 {
		var populated bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*')`).Scan(&populated); err != nil {
			return err
		}
		if populated {
			return errors.New("database: unsupported non-empty schema version 0")
		}
	}
	// The canonical manifest is the only physical-object representation.
	// unused_at is not a retention clock; collection checks references and pins.
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS objects (
 id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, remote TEXT NOT NULL, dedup TEXT NOT NULL, sha1 TEXT NOT NULL,
 size INTEGER NOT NULL, format TEXT NOT NULL, off INTEGER NOT NULL, length INTEGER NOT NULL,
 unused_at INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'live',
 parts TEXT NOT NULL CHECK(json_valid(parts)), UNIQUE(account,remote));
 CREATE TABLE IF NOT EXISTS entries (
 id INTEGER PRIMARY KEY AUTOINCREMENT, parent INTEGER REFERENCES entries(id) ON DELETE CASCADE,
 name TEXT COLLATE BINARY NOT NULL, dir INTEGER NOT NULL, object INTEGER REFERENCES objects(id),
 size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 1, content_type TEXT NOT NULL DEFAULT '', UNIQUE(parent,name));
 CREATE TABLE IF NOT EXISTS buckets (entry INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE, album TEXT NOT NULL, account TEXT NOT NULL, created INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, path TEXT NOT NULL, state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
 created INTEGER NOT NULL, updated INTEGER NOT NULL, spool TEXT NOT NULL DEFAULT '', carrier TEXT NOT NULL DEFAULT '',
 format TEXT NOT NULL DEFAULT '', sha1 TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0, off INTEGER NOT NULL DEFAULT 0, length INTEGER NOT NULL DEFAULT 0,
 album TEXT NOT NULL DEFAULT '', account TEXT NOT NULL DEFAULT '', expected_id INTEGER NOT NULL DEFAULT 0, expected_version INTEGER NOT NULL DEFAULT 0,
 content_type TEXT NOT NULL DEFAULT '', mtime INTEGER NOT NULL DEFAULT 0, object INTEGER REFERENCES objects(id),
 remote TEXT NOT NULL DEFAULT '', remote_account TEXT NOT NULL DEFAULT '', remote_sha1 TEXT NOT NULL DEFAULT '', remote_dedup TEXT NOT NULL DEFAULT '', remote_size INTEGER NOT NULL DEFAULT 0);
 INSERT OR IGNORE INTO entries(id,parent,name,dir,mtime) VALUES(1,NULL,'',1,0);
 CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY,email TEXT NOT NULL,android_id TEXT NOT NULL,master_token TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 PRAGMA user_version=3;`); err != nil {
		return err
	}
	return tx.Commit()
}
