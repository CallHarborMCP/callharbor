package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrConflict      = errors.New("record conflict")
	ErrSetupComplete = errors.New("setup already complete")
	ErrLastAdmin     = errors.New("operation would remove the last active admin")
)

type User struct {
	ID           string
	Username     string
	Role         string
	PasswordHash string
	Active       bool
	CreatedAt    string
	UpdatedAt    string
}

type Session struct {
	TokenHash  string
	CSRFToken  string
	UserID     string
	CreatedAt  string
	LastSeenAt string
	ExpiresAt  string
	RevokedAt  string
	User       User
}

type Throttle struct {
	Key             string
	Failures        int
	WindowStartedAt string
	LockedUntil     string
}

type AuditEvent struct {
	OccurredAt   string
	ActorUserID  string
	ActorRole    string
	TargetUserID string
	Action       string
	ResourceType string
	ResourceID   string
	Outcome      string
	DetailJSON   string
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const userColumns = "id, username, role, password_hash, active, created_at, updated_at"

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var hash sql.NullString
	var active int
	if err := row.Scan(&u.ID, &u.Username, &u.Role, &hash, &active, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, notFound(err)
	}
	u.PasswordHash = hash.String
	u.Active = active == 1
	return u, nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func mapConflict(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrConflict
	}
	return err
}

func insertUser(ctx context.Context, q execer, u User) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO users (id, username, role, password_hash, active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.Role, nullable(u.PasswordHash), boolInt(u.Active), u.CreatedAt, u.UpdatedAt)
	return mapConflict(err)
}

func insertAudit(ctx context.Context, q execer, e AuditEvent) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO audit_events (occurred_at, actor_user_id, actor_role, target_user_id, action, resource_type, resource_id, outcome, detail_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.OccurredAt, nullable(e.ActorUserID), nullable(e.ActorRole), nullable(e.TargetUserID),
		e.Action, e.ResourceType, nullable(e.ResourceID), e.Outcome, e.DetailJSON)
	return err
}

func (s *Store) InsertAudit(ctx context.Context, e AuditEvent) error {
	return insertAudit(ctx, s.db, e)
}

func (s *Store) CountAuditEvents(ctx context.Context, action, outcome string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audit_events WHERE action = ? AND outcome = ?", action, outcome).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CreateFirstAdmin inserts the initial administrator only while no users exist.
// The immediate transaction serializes concurrent setup attempts.
func (s *Store) CreateFirstAdmin(ctx context.Context, u User, audit AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSetupComplete
	}
	if err := insertUser(ctx, tx, u); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateUser(ctx context.Context, u User, audit AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertUser(ctx, tx, u); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userColumns+" FROM users ORDER BY username COLLATE NOCASE, id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id))
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE username = ?", username))
}

// UpdateUser changes role and/or active state. It refuses any change that would
// leave no active admin, and revokes all sessions when the user is deactivated.
func (s *Store) UpdateUser(ctx context.Context, id string, role *string, active *bool, now string, audit AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	target, err := scanUser(tx.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id))
	if err != nil {
		return err
	}
	newRole, newActive := target.Role, target.Active
	if role != nil {
		newRole = *role
	}
	if active != nil {
		newActive = *active
	}
	if target.Role == "admin" && target.Active && (newRole != "admin" || !newActive) {
		var others int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM users WHERE role = 'admin' AND active = 1 AND id <> ?", id).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET role = ?, active = ?, updated_at = ? WHERE id = ?",
		newRole, boolInt(newActive), now, id); err != nil {
		return err
	}
	if target.Active && !newActive {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL", now, id); err != nil {
			return err
		}
		if err := revokeUserOAuthGrants(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetPasswordHash(ctx context.Context, id, hash, now string, audit AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?", hash, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL", now, id); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, csrf_token, user_id, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.CSRFToken, sess.UserID, sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt)
	return err
}

func (s *Store) GetSession(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	var active int
	err := s.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.csrf_token, s.user_id, s.created_at, s.last_seen_at, s.expires_at, COALESCE(s.revoked_at, ''),
		        u.id, u.username, u.role, COALESCE(u.password_hash, ''), u.active, u.created_at, u.updated_at
		 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, tokenHash).Scan(
		&sess.TokenHash, &sess.CSRFToken, &sess.UserID, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt,
		&sess.User.ID, &sess.User.Username, &sess.User.Role, &sess.User.PasswordHash, &active, &sess.User.CreatedAt, &sess.User.UpdatedAt)
	if err != nil {
		return Session{}, notFound(err)
	}
	sess.User.Active = active == 1
	return sess, nil
}

func (s *Store) TouchSession(ctx context.Context, tokenHash, now string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE token_hash = ? AND revoked_at IS NULL", now, tokenHash)
	return err
}

func (s *Store) RevokeSession(ctx context.Context, tokenHash, now string, audit AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL", now, tokenHash); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetThrottle(ctx context.Context, key string) (Throttle, error) {
	var t Throttle
	var locked sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT key, failures, window_started_at, locked_until FROM login_throttle WHERE key = ?", key).Scan(
		&t.Key, &t.Failures, &t.WindowStartedAt, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return Throttle{Key: key}, nil
	}
	if err != nil {
		return Throttle{}, err
	}
	t.LockedUntil = locked.String
	return t, nil
}

// UpdateThrottle applies mutate to the current throttle state inside an
// immediate transaction so concurrent failures cannot lose increments.
func (s *Store) UpdateThrottle(ctx context.Context, key, now string, mutate func(Throttle) Throttle) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var cur Throttle
	var locked sql.NullString
	err = tx.QueryRowContext(ctx,
		"SELECT key, failures, window_started_at, locked_until FROM login_throttle WHERE key = ?", key).Scan(
		&cur.Key, &cur.Failures, &cur.WindowStartedAt, &locked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		cur = Throttle{Key: key}
	case err != nil:
		return err
	default:
		cur.LockedUntil = locked.String
	}
	next := mutate(cur)
	next.Key = key
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO login_throttle (key, failures, window_started_at, locked_until, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET failures = excluded.failures, window_started_at = excluded.window_started_at,
		 locked_until = excluded.locked_until, updated_at = excluded.updated_at`,
		key, next.Failures, next.WindowStartedAt, nullable(next.LockedUntil), now); err != nil {
		return err
	}
	return tx.Commit()
}
