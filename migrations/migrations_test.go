package migrations

import (
	"path/filepath"
	"testing"

	"bangle/internal/db"
)

// Running twice must be a no-op the second time: startup runs migrations on
// every boot.
func TestRunIsIdempotent(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "bangle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	for i := range 2 {
		if err := Run(database); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM migration_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	files, err := sqlFiles.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if n != len(files) {
		t.Fatalf("migration_log has %d rows, want one per file (%d)", n, len(files))
	}
}
