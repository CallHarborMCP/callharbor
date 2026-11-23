package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	driverName = "sqlite"
	dsnOptions = "?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_txlock=immediate"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("database path %q must not contain '?' or '#'", path)
	}
	db, err := sql.Open(driverName, "file:"+path+dsnOptions)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.verifyPragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) verifyPragmas(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("journal_mode is %q, want wal", mode)
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("foreign key enforcement is disabled")
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign_key_check: dangling references present")
	}
	return rows.Err()
}

// Backup writes a transactionally consistent snapshot to dst, including pages
// still held in the WAL, without stopping writers. dst must not already exist.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup destination %s already exists", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat backup destination: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("vacuum into backup: %w", err)
	}
	return nil
}
