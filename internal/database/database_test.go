package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestDatabaseSmoke(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if db.Stats().MaxOpenConnections != 1 {
		t.Fatal("database must use a single connection")
	}
	for query, want := range map[string]int{
		"PRAGMA user_version": 3,
		"PRAGMA foreign_keys": 1,
		"PRAGMA synchronous":  2,
		"PRAGMA busy_timeout": 5000,
		"SELECT count(*) FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*'":                         6,
		`SELECT count(*) FROM pragma_table_info('objects') WHERE name='parts' AND "notnull"=1`:                       1,
		"SELECT count(*) FROM entries WHERE id=1 AND parent IS NULL AND name='' AND dir=1 AND mtime=0 AND version=1": 1,
	} {
		var got int
		if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, want %d, err=%v", query, got, want, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO objects(account,remote,dedup,sha1,size,format,off,length,unused_at,parts) VALUES('mock','group:mock','','',0,'parts-v1',0,0,0,'invalid json')`); err == nil {
		t.Fatal("manifest JSON constraint missing")
	}
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatal("WAL mode", journal, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE entries SET mtime=123,version=7 WHERE id=1;
 INSERT INTO accounts(id,email,android_id,master_token) VALUES('test','test@example.invalid','android','mock-token');
 INSERT INTO settings(key,value) VALUES('google.selected_account','test'),('admin.username','admin'),('admin.password_hash','mock-hash');`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Initialize(ctx, db); err != nil {
			t.Fatal("current schema initialization is not idempotent", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Open(ctx, root)
	if err != nil {
		t.Fatal("cannot reopen current database", err)
	}
	t.Cleanup(func() { current.Close() })
	var retained int
	if err := current.QueryRowContext(ctx, `SELECT count(*) FROM entries WHERE id=1 AND parent IS NULL AND mtime=123 AND version=7`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("reopening changed namespace identities or timestamps", err)
	}
	if err := current.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id='test'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("reopening changed account state", err)
	}
	if _, err := current.ExecContext(ctx, "PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(ctx, current); err == nil {
		t.Fatal("initialization accepted an unsupported schema")
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := Open(ctx, root); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		if unexpected != nil {
			unexpected.Close()
		}
		t.Fatal("runtime accepted an unsupported schema", err)
	}
}

func TestRejectNonemptyVersionZero(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "CREATE TABLE existing (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err = Initialize(ctx, db); err == nil {
		t.Fatal("accepted populated version zero")
	}
	var version, tables int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatal(version, err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table'").Scan(&tables); err != nil || tables != 1 {
		t.Fatal("rejection mutated schema", tables, err)
	}
}
