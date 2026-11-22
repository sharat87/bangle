package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open opens the SQLite database at path, creating the file (and its
// directory) if needed.
//
// Every connection gets the same pragmas through the DSN, because pragmas are
// per connection and database/sql pools connections:
//   - journal_mode(WAL): readers don't block the writer, and an MCP call in
//     flight doesn't stall the UI.
//   - foreign_keys(1): SQLite ignores REFERENCES clauses without it.
//   - busy_timeout(5000): a second writer waits instead of failing at once
//     with SQLITE_BUSY.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating directory for %s: %w", path, err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, fmt.Errorf("connecting to %s: %w", path, err)
	}
	if err := checkWritable(database); err != nil {
		database.Close()
		return nil, fmt.Errorf("%s is not writable (check the owner of it and its -wal and -shm files): %w", path, err)
	}
	return database, nil
}

// checkWritable makes a real write and rolls it back. SQLite opens a file
// it can't write read-only without a word, and then reads work and every
// write fails ("attempt to write a readonly database"). The first daemon
// install hit that through root-owned -wal and -shm files, and served for a
// while half-working; now that stops the boot. Taking the write lock alone
// (BEGIN IMMEDIATE) does not notice: only a write does.
func checkWritable(database *sql.DB) error {
	ctx := context.Background()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	_, writeErr := tx.ExecContext(ctx, `CREATE TABLE bangle_write_check (x)`)
	if err := tx.Rollback(); err != nil && writeErr == nil {
		return err
	}
	return writeErr
}
