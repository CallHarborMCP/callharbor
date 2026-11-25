package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY CHECK (version >= 1),
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
)`

// foreignKeysOffDirective, as the first line of a migration script, asks the
// runner to disable foreign key enforcement for that script. SQLite ignores
// PRAGMA foreign_keys inside a transaction, so the runner sets it on the
// connection before BEGIN. It then checks foreign_key_check inside the
// transaction and fails the migration on any violation. Enforcement is
// restored afterward. Use it only for table rebuilds that follow the SQLite
// ALTER TABLE procedure.
const foreignKeysOffDirective = "-- callharbor:foreign_keys=off"

type migration struct {
	version int
	name    string
	up      string
	down    string
}

func needsForeignKeysOff(script string) bool {
	first, _, _ := strings.Cut(script, "\n")
	return strings.TrimSpace(first) == foreignKeysOffDirective
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	byVersion := map[int]*migration{}
	for _, entry := range entries {
		file := entry.Name()
		prefix, rest, ok := strings.Cut(file, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: name must start with NNNN_", file)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %s: invalid version", file)
		}
		body, err := migrationFiles.ReadFile(path.Join("migrations", file))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", file, err)
		}
		m := byVersion[version]
		if m == nil {
			m = &migration{version: version}
			byVersion[version] = m
		}
		switch {
		case strings.HasSuffix(rest, ".up.sql"):
			m.name = strings.TrimSuffix(rest, ".up.sql")
			m.up = string(body)
		case strings.HasSuffix(rest, ".down.sql"):
			if name := strings.TrimSuffix(rest, ".down.sql"); name != m.name && m.name != "" {
				return nil, fmt.Errorf("migration %s: down name mismatch", file)
			}
			m.down = string(body)
		default:
			return nil, fmt.Errorf("migration %s: must end in .up.sql or .down.sql", file)
		}
	}

	versions := make([]int, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Ints(versions)

	migrations := make([]migration, 0, len(versions))
	for i, v := range versions {
		if v != i+1 {
			return nil, fmt.Errorf("migrations must be contiguous: missing version %d", i+1)
		}
		m := byVersion[v]
		if m.up == "" || m.down == "" {
			return nil, fmt.Errorf("migration %04d_%s: up and down scripts are both required", v, m.name)
		}
		migrations = append(migrations, *m)
	}
	return migrations, nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, createMigrationsTable); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// Migrate applies every pending up migration in its own immediate transaction.
// It refuses to run against a database created by a newer binary.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	latest := migrations[len(migrations)-1].version
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, latest)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.runMigration(ctx, m.up, m.version, m.name, true); err != nil {
			return err
		}
	}
	return nil
}

// MigrateDown rolls back applied migrations until the schema is at target.
func (s *Store) MigrateDown(ctx context.Context, target int) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if target < 0 || target > current {
		return fmt.Errorf("rollback target %d is outside applied range 0..%d", target, current)
	}
	for i := len(migrations) - 1; i >= 0; i-- {
		m := migrations[i]
		if m.version > current || m.version <= target {
			continue
		}
		if err := s.runMigration(ctx, m.down, m.version, m.name, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) runMigration(ctx context.Context, script string, version int, name string, up bool) error {
	direction := "up"
	if !up {
		direction = "down"
	}
	// Pin one connection. The pool holds a single connection, so the pragma and
	// the transaction that follows both act on the same session.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect for migration %04d_%s %s: %w", version, name, direction, err)
	}
	defer func() { _ = conn.Close() }()

	fkOff := needsForeignKeysOff(script)
	if fkOff {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("migration %04d_%s %s: disable foreign keys: %w", version, name, direction, err)
		}
		defer func() {
			// Restore enforcement on the pinned connection before it returns to the pool.
			_, _ = conn.ExecContext(context.Background(), "PRAGMA foreign_keys = ON")
		}()
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %04d_%s: %w", version, name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, script); err != nil {
		return fmt.Errorf("migration %04d_%s %s: %w", version, name, direction, err)
	}
	if fkOff {
		if err := checkForeignKeys(ctx, tx); err != nil {
			return fmt.Errorf("migration %04d_%s %s: %w", version, name, direction, err)
		}
	}
	if up {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, name) VALUES (?, ?)", version, name)
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", version)
	}
	if err != nil {
		return fmt.Errorf("record migration %04d_%s %s: %w", version, name, direction, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %04d_%s %s: %w", version, name, direction, err)
	}
	return nil
}

// checkForeignKeys fails when foreign_key_check reports any violation. It runs
// inside the migration transaction so a bad rebuild never commits.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("foreign_key_check scan: %w", err)
		}
		return fmt.Errorf("foreign key violation in %s referencing %s", table, parent)
	}
	return rows.Err()
}
