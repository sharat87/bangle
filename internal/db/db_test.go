package db

import (
	"os"
	"path/filepath"
	"testing"
)

// A database Bangle can read but not write must stop the boot, not serve
// reads and fail every write ("attempt to write a readonly database"): the
// first daemon install hit exactly that, through root-owned -wal/-shm files.
func TestOpenRefusesReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bangle.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if d, err := Open(path); err == nil {
		d.Close()
		t.Fatal("a read-only database was opened")
	}
}
