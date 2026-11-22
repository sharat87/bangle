package migrations

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed *.sql
var sqlFiles embed.FS

// Run applies every embedded NNN_name.sql file not yet recorded in
// migration_log, in name order. 001 creates migration_log itself, so on a
// fresh database the table does not exist yet and every file runs.
func Run(database *sql.DB) error {
	var exists bool
	err := database.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'migration_log')`,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("checking migration_log existence: %w", err)
	}

	applied := map[string]bool{}
	if exists {
		rows, err := database.Query(`SELECT migration_name FROM migration_log`)
		if err != nil {
			return fmt.Errorf("reading migration_log: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return fmt.Errorf("reading migration_log: %w", err)
			}
			applied[name] = true
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reading migration_log: %w", err)
		}
	}

	entries, err := sqlFiles.ReadDir(".")
	if err != nil {
		return fmt.Errorf("reading migration files: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	for _, file := range files {
		if applied[file] {
			continue
		}
		content, err := sqlFiles.ReadFile(file)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", file, err)
		}

		// Run the migration and its log entry in one transaction so a failure
		// between the two cannot leave an applied-but-unlogged migration.
		tx, err := database.Begin()
		if err != nil {
			return fmt.Errorf("starting migration %s: %w", file, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s failed: %w", file, err)
		}
		if _, err := tx.Exec(`INSERT INTO migration_log (migration_name) VALUES (?)`, file); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %s failed: %w", file, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", file, err)
		}
	}
	return nil
}
