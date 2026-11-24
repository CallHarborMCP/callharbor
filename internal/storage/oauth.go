package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OAuthClient is a registered public client (dynamic registration or a
// verified Client ID Metadata Document).
type OAuthClient struct {
	ClientID     string
	Source       string
	ClientName   string
	ClientURI    string
	RedirectURIs string // JSON array
	GrantTypes   string // JSON array; empty means the default pair
	CreatedAt    string
	UpdatedAt    string
}

// OAuthGrant is one user's authorization of one client for one resource.
type OAuthGrant struct {
	ID         string
	UserID     string
	ClientID   string
	ClientName string
	Resource   string
	Scopes     string
	CreatedAt  string
	LastUsedAt string
	RevokedAt  string
}

// OAuthAuthRequest is a pending authorization request awaiting consent.
type OAuthAuthRequest struct {
	ID            string
	ClientID      string
	ClientName    string
	RedirectURI   string
	State         string
	Scopes        string
	Resource      string
	CodeChallenge string
	Nonce         string
	UserID        string
	CreatedAt     string
	ExpiresAt     string
	DecidedAt     string
	ConsumedAt    string
}

// OAuthCode is an issued authorization code, stored as a hash.
type OAuthCode struct {
	CodeHash      string
	GrantID       string
	AuthRequestID string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	CreatedAt     string
	ExpiresAt     string
	ConsumedAt    string
}

// OAuthToken is an access or refresh token row, stored as a hash. For access
// tokens ExpiresAt is the access expiry; for refresh tokens it is the absolute
// lifetime end. GrantRevokedAt is set by lookups that join the grant.
type OAuthToken struct {
	TokenHash      string
	GrantID        string
	UserID         string
	ClientID       string
	Audience       string
	Scopes         string
	CreatedAt      string
	ExpiresAt      string
	RotatedAt      string
	RevokedAt      string
	GrantRevokedAt string
}

// ErrOAuthCodeConsumed reports a code that was already exchanged.
var ErrOAuthCodeConsumed = errors.New("oauth code already consumed")

// ErrOAuthRequestNotDecidable reports an auth request that is missing,
// expired, already decided, or already consumed.
var ErrOAuthRequestNotDecidable = errors.New("oauth authorization request is not pending")

func (s *Store) UpsertOAuthClient(ctx context.Context, c OAuthClient) error {
	grants := c.GrantTypes
	if grants == "" {
		grants = `["authorization_code","refresh_token"]`
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_clients
		(client_id, source, client_name, client_uri, redirect_uris_json, grant_types_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (client_id) DO UPDATE SET client_name = excluded.client_name, client_uri = excluded.client_uri,
		redirect_uris_json = excluded.redirect_uris_json, grant_types_json = excluded.grant_types_json,
		updated_at = excluded.updated_at`,
		c.ClientID, c.Source, c.ClientName, c.ClientURI, c.RedirectURIs, grants, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	var c OAuthClient
	err := s.db.QueryRowContext(ctx, `SELECT client_id, source, client_name, client_uri, redirect_uris_json, grant_types_json, created_at, updated_at
		FROM oauth_clients WHERE client_id = ?`, clientID).Scan(
		&c.ClientID, &c.Source, &c.ClientName, &c.ClientURI, &c.RedirectURIs, &c.GrantTypes, &c.CreatedAt, &c.UpdatedAt)
	return c, notFound(err)
}

// InsertDynamicOAuthClient stores a dynamically registered client and its audit
// event in one transaction. A new client_id is never an update.
func (s *Store) InsertDynamicOAuthClient(ctx context.Context, c OAuthClient, audit AuditEvent) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_clients
			(client_id, source, client_name, client_uri, redirect_uris_json, grant_types_json, created_at, updated_at)
			VALUES (?, 'dynamic', ?, ?, ?, ?, ?, ?)`,
			c.ClientID, c.ClientName, c.ClientURI, c.RedirectURIs, c.GrantTypes, c.CreatedAt, c.UpdatedAt); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
}

func (s *Store) CountDynamicOAuthClients(ctx context.Context, sinceISO string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM oauth_clients WHERE source = 'dynamic' AND created_at >= ?", sinceISO).Scan(&n)
	return n, err
}

func (s *Store) InsertOAuthAuthRequest(ctx context.Context, r OAuthAuthRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_auth_requests
		(id, client_id, redirect_uri, state, scopes, resource, code_challenge, nonce, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ClientID, r.RedirectURI, r.State, r.Scopes, r.Resource, r.CodeChallenge, r.Nonce, r.CreatedAt, r.ExpiresAt)
	return err
}

// GetOAuthAuthRequest returns a request joined with its client name. It does
// not filter by expiry; callers compare ExpiresAt against their clock.
func (s *Store) GetOAuthAuthRequest(ctx context.Context, id string) (OAuthAuthRequest, error) {
	var r OAuthAuthRequest
	var userID, decided, consumed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT r.id, r.client_id, c.client_name, r.redirect_uri, r.state, r.scopes, r.resource,
		r.code_challenge, r.nonce, r.user_id, r.created_at, r.expires_at, r.decided_at, r.consumed_at
		FROM oauth_auth_requests r JOIN oauth_clients c ON c.client_id = r.client_id WHERE r.id = ?`, id).Scan(
		&r.ID, &r.ClientID, &r.ClientName, &r.RedirectURI, &r.State, &r.Scopes, &r.Resource,
		&r.CodeChallenge, &r.Nonce, &userID, &r.CreatedAt, &r.ExpiresAt, &decided, &consumed)
	if err != nil {
		return OAuthAuthRequest{}, notFound(err)
	}
	r.UserID, r.DecidedAt, r.ConsumedAt = userID.String, decided.String, consumed.String
	return r, nil
}

// DecideOAuthAuthRequest atomically binds a pending, unexpired request to a
// user and records the decision. Only one decision can succeed.
func (s *Store) DecideOAuthAuthRequest(ctx context.Context, id, userID, now string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE oauth_auth_requests SET user_id = ?, decided_at = ?
		WHERE id = ? AND decided_at IS NULL AND consumed_at IS NULL AND expires_at > ?`, userID, now, id, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOAuthRequestNotDecidable
	}
	return nil
}

// ApproveOAuthRequest issues the grant, the code and the audit event in one
// transaction. The request must be decided and unexpired; it is marked
// consumed so it can only ever produce one code.
func (s *Store) ApproveOAuthRequest(ctx context.Context, requestID, userID string, grant OAuthGrant, code OAuthCode, now string, audit AuditEvent) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE oauth_auth_requests SET consumed_at = ?
			WHERE id = ? AND user_id = ? AND decided_at IS NOT NULL AND consumed_at IS NULL AND expires_at > ?`,
			now, requestID, userID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrOAuthRequestNotDecidable
		}
		if err := insertOAuthGrant(ctx, tx, grant); err != nil {
			return err
		}
		if err := insertOAuthCode(ctx, tx, code); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
}

// DenyOAuthRequest marks a pending request as decided-denied and consumed.
func (s *Store) DenyOAuthRequest(ctx context.Context, requestID, userID, now string, audit AuditEvent) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE oauth_auth_requests SET user_id = ?, decided_at = ?, consumed_at = ?
			WHERE id = ? AND decided_at IS NULL AND consumed_at IS NULL AND expires_at > ?`,
			userID, now, now, requestID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrOAuthRequestNotDecidable
		}
		return insertAudit(ctx, tx, audit)
	})
}

func insertOAuthGrant(ctx context.Context, q execer, g OAuthGrant) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_grants (id, user_id, client_id, resource, scopes, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, g.ID, g.UserID, g.ClientID, g.Resource, g.Scopes, g.CreatedAt)
	return err
}

func insertOAuthCode(ctx context.Context, q execer, c OAuthCode) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_codes
		(code_hash, grant_id, auth_request_id, client_id, redirect_uri, code_challenge, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CodeHash, c.GrantID, c.AuthRequestID, c.ClientID, c.RedirectURI, c.CodeChallenge, c.CreatedAt, c.ExpiresAt)
	return err
}

// GetOAuthCode returns a code row by hash.
func (s *Store) GetOAuthCode(ctx context.Context, codeHash string) (OAuthCode, error) {
	var c OAuthCode
	var consumed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT code_hash, grant_id, auth_request_id, client_id, redirect_uri, code_challenge, expires_at, consumed_at
		FROM oauth_codes WHERE code_hash = ?`, codeHash).Scan(
		&c.CodeHash, &c.GrantID, &c.AuthRequestID, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.ExpiresAt, &consumed)
	c.ConsumedAt = consumed.String
	return c, notFound(err)
}

// ConsumeOAuthCode marks a code consumed exactly once. A second caller gets
// ErrOAuthCodeConsumed, so concurrent exchanges have a single winner.
func (s *Store) ConsumeOAuthCode(ctx context.Context, codeHash, now string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE oauth_codes SET consumed_at = ?
		WHERE code_hash = ? AND consumed_at IS NULL AND expires_at > ?`, now, codeHash, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOAuthCodeConsumed
	}
	return nil
}

// IssueOAuthTokens writes an access token and, when refresh is non-empty, a
// refresh token in one transaction, updating the grant's last-used time.
func (s *Store) IssueOAuthTokens(ctx context.Context, access OAuthToken, refresh *OAuthToken, now string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := insertOAuthAccess(ctx, tx, access); err != nil {
			return err
		}
		if refresh != nil {
			if err := insertOAuthRefresh(ctx, tx, *refresh); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE oauth_grants SET last_used_at = ? WHERE id = ?", now, access.GrantID)
		return err
	})
}

// RedeemOAuthCode consumes a code and writes its tokens in one transaction. A
// caller that loses the consumption race gets ErrOAuthCodeConsumed and no
// tokens are written, so the winner's tokens are always committed before any
// replay revocation can run.
func (s *Store) RedeemOAuthCode(ctx context.Context, codeHash, now string, access OAuthToken, refresh *OAuthToken) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET consumed_at = ?
			WHERE code_hash = ? AND consumed_at IS NULL AND expires_at > ?`, now, codeHash, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrOAuthCodeConsumed
		}
		if err := insertOAuthAccess(ctx, tx, access); err != nil {
			return err
		}
		if refresh != nil {
			if err := insertOAuthRefresh(ctx, tx, *refresh); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE oauth_grants SET last_used_at = ? WHERE id = ?", now, access.GrantID)
		return err
	})
}

func insertOAuthAccess(ctx context.Context, q execer, t OAuthToken) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_access_tokens
		(token_hash, grant_id, user_id, client_id, audience, scopes, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TokenHash, t.GrantID, t.UserID, t.ClientID, t.Audience, t.Scopes, t.CreatedAt, t.ExpiresAt)
	return err
}

func insertOAuthRefresh(ctx context.Context, q execer, t OAuthToken) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_refresh_tokens
		(token_hash, grant_id, user_id, client_id, audience, scopes, created_at, absolute_expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TokenHash, t.GrantID, t.UserID, t.ClientID, t.Audience, t.Scopes, t.CreatedAt, t.ExpiresAt)
	return err
}

// GetOAuthAccessToken loads an access token with its user and grant state.
func (s *Store) GetOAuthAccessToken(ctx context.Context, tokenHash string) (OAuthToken, error) {
	return s.scanOAuthToken(ctx, `SELECT t.token_hash, t.grant_id, t.user_id, t.client_id, t.audience, t.scopes, t.expires_at,
		COALESCE(t.revoked_at, ''), g.revoked_at
		FROM oauth_access_tokens t JOIN oauth_grants g ON g.id = t.grant_id WHERE t.token_hash = ?`, tokenHash)
}

func (s *Store) scanOAuthToken(ctx context.Context, query string, args ...any) (OAuthToken, error) {
	var t OAuthToken
	var grantRevoked sql.NullString
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&t.TokenHash, &t.GrantID, &t.UserID, &t.ClientID,
		&t.Audience, &t.Scopes, &t.ExpiresAt, &t.RevokedAt, &grantRevoked)
	t.GrantRevokedAt = grantRevoked.String
	if err != nil {
		return OAuthToken{}, notFound(err)
	}
	return t, nil
}

// RotateOAuthRefreshToken atomically marks the presented refresh token as
// rotated and inserts its successor and the new access token. It returns
// ErrOAuthTokenReuse-style semantics via the caller: a token that was already
// rotated or revoked is reported as (false, nil) so the caller can revoke the
// family.
func (s *Store) RotateOAuthRefreshToken(ctx context.Context, oldHash string, next OAuthToken, nextRefresh OAuthToken, now string) (bool, error) {
	var ok bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE oauth_refresh_tokens SET rotated_at = ?
			WHERE token_hash = ? AND rotated_at IS NULL AND revoked_at IS NULL`, now, oldHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if err := insertOAuthAccess(ctx, tx, next); err != nil {
			return err
		}
		if err := insertOAuthRefresh(ctx, tx, nextRefresh); err != nil {
			return err
		}
		ok = true
		_, err = tx.ExecContext(ctx, "UPDATE oauth_grants SET last_used_at = ? WHERE id = ?", now, next.GrantID)
		return err
	})
	return ok, err
}

// GetOAuthRefreshToken loads a refresh token row by hash.
func (s *Store) GetOAuthRefreshToken(ctx context.Context, tokenHash string) (OAuthToken, error) {
	var t OAuthToken
	var rotated, revoked, grantRevoked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT r.token_hash, r.grant_id, r.user_id, r.client_id, r.audience, r.scopes,
		r.absolute_expires_at, r.rotated_at, r.revoked_at, g.revoked_at
		FROM oauth_refresh_tokens r JOIN oauth_grants g ON g.id = r.grant_id WHERE r.token_hash = ?`, tokenHash).Scan(
		&t.TokenHash, &t.GrantID, &t.UserID, &t.ClientID, &t.Audience, &t.Scopes, &t.ExpiresAt, &rotated, &revoked, &grantRevoked)
	if err != nil {
		return OAuthToken{}, notFound(err)
	}
	t.RotatedAt, t.RevokedAt, t.GrantRevokedAt = rotated.String, revoked.String, grantRevoked.String
	return t, nil
}

// RevokeOAuthGrant revokes a grant family: the grant, its access and refresh
// tokens. Revoking an already revoked grant is a no-op.
func (s *Store) RevokeOAuthGrant(ctx context.Context, grantID, now string, audit AuditEvent) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := revokeGrantFamily(ctx, tx, grantID, now); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
}

func revokeGrantFamily(ctx context.Context, tx *sql.Tx, grantID, now string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE oauth_grants SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?", now, grantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE oauth_access_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ?", now, grantID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE oauth_refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ?", now, grantID)
	return err
}

// RevokeOAuthToken revokes one access or refresh token by hash. For a refresh
// token the whole family is revoked, per RFC 7009 guidance.
func (s *Store) RevokeOAuthTokenByHash(ctx context.Context, tokenHash, now string) (bool, error) {
	var found bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var grantID string
		err := tx.QueryRowContext(ctx, "SELECT grant_id FROM oauth_access_tokens WHERE token_hash = ?", tokenHash).Scan(&grantID)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, "SELECT grant_id FROM oauth_refresh_tokens WHERE token_hash = ?", tokenHash).Scan(&grantID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
		}
		if err != nil {
			return err
		}
		found = true
		return revokeGrantFamily(ctx, tx, grantID, now)
	})
	return found, err
}

// ListOAuthGrants returns a user's grants, newest first, excluding revoked ones.
func (s *Store) ListOAuthGrants(ctx context.Context, userID string) ([]OAuthGrant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.user_id, g.client_id, c.client_name, g.resource, g.scopes,
		g.created_at, COALESCE(g.last_used_at, ''), COALESCE(g.revoked_at, '')
		FROM oauth_grants g JOIN oauth_clients c ON c.client_id = g.client_id
		WHERE g.user_id = ? AND g.revoked_at IS NULL ORDER BY g.created_at DESC, g.id`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OAuthGrant
	for rows.Next() {
		var g OAuthGrant
		if err := rows.Scan(&g.ID, &g.UserID, &g.ClientID, &g.ClientName, &g.Resource, &g.Scopes, &g.CreatedAt, &g.LastUsedAt, &g.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// TouchOAuthGrant records a use of a grant at now. It only writes when the
// stored value is older than before, so concurrent callers cannot move it back.
func (s *Store) TouchOAuthGrant(ctx context.Context, grantID, now, before string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE oauth_grants SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)",
		now, grantID, before)
	return err
}

// GetOAuthGrantByID returns a grant regardless of owner, including revoked
// grants (RevokedAt is set). Token paths use it to check grant state.
func (s *Store) GetOAuthGrantByID(ctx context.Context, grantID string) (OAuthGrant, error) {
	var g OAuthGrant
	err := s.db.QueryRowContext(ctx, `SELECT g.id, g.user_id, g.client_id, c.client_name, g.resource, g.scopes,
		g.created_at, COALESCE(g.last_used_at, ''), COALESCE(g.revoked_at, '')
		FROM oauth_grants g JOIN oauth_clients c ON c.client_id = g.client_id WHERE g.id = ?`, grantID).Scan(
		&g.ID, &g.UserID, &g.ClientID, &g.ClientName, &g.Resource, &g.Scopes, &g.CreatedAt, &g.LastUsedAt, &g.RevokedAt)
	return g, notFound(err)
}

// GetOAuthGrantForUser returns a grant only when it belongs to userID.
func (s *Store) GetOAuthGrantForUser(ctx context.Context, userID, grantID string) (OAuthGrant, error) {
	var g OAuthGrant
	err := s.db.QueryRowContext(ctx, `SELECT g.id, g.user_id, g.client_id, c.client_name, g.resource, g.scopes,
		g.created_at, COALESCE(g.last_used_at, ''), COALESCE(g.revoked_at, '')
		FROM oauth_grants g JOIN oauth_clients c ON c.client_id = g.client_id WHERE g.id = ? AND g.user_id = ?`, grantID, userID).Scan(
		&g.ID, &g.UserID, &g.ClientID, &g.ClientName, &g.Resource, &g.Scopes, &g.CreatedAt, &g.LastUsedAt, &g.RevokedAt)
	return g, notFound(err)
}

// RevokeUserOAuthGrants revokes every live grant family of a user inside tx.
// Identity deactivation calls it in the same transaction as the user update.
func revokeUserOAuthGrants(ctx context.Context, tx *sql.Tx, userID, now string) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM oauth_grants WHERE user_id = ? AND revoked_at IS NULL", userID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := revokeGrantFamily(ctx, tx, id, now); err != nil {
			return fmt.Errorf("revoke oauth grant: %w", err)
		}
	}
	return nil
}

// inTx runs fn in an immediate transaction and commits on success.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
