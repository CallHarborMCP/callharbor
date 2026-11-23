package storage

import (
	"context"
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

type migration struct {
	version int
	name    string
	up      string
	down    string
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %04d_%s: %w", version, name, err)
	}
	defer func() { _ = tx.Rollback() }()

	direction := "up"
	if !up {
		direction = "down"
	}
	if _, err := tx.ExecContext(ctx, script); err != nil {
		return fmt.Errorf("migration %04d_%s %s: %w", version, name, direction, err)
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
