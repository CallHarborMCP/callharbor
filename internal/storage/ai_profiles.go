package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// MaxInstructions bounds the per-profile system instructions.
const MaxInstructions = 8192

// AIProtocol is the explicit AI voice protocol stored on a profile. It is
// never inferred from the model or endpoint.
type AIProtocol string

const (
	AIProtocolRealtime AIProtocol = "realtime_ws"
	AIProtocolLive     AIProtocol = "live_ws"
)

// ErrAIProfileNotFound reports a missing profile or one owned by another user.
var ErrAIProfileNotFound = errors.New("storage: ai profile not found")

// ErrInvalidAIProfile reports a profile that failed ValidateAIProfile.
var ErrInvalidAIProfile = errors.New("storage: invalid ai profile")

// AIProfile is one user-owned AI voice configuration. APISecretID references
// an owner-scoped secret; the secret value is never stored here.
type AIProfile struct {
	ID           string
	OwnerUserID  string
	Name         string
	Protocol     AIProtocol
	EndpointURL  string
	Model        string
	Voice        string
	Instructions string
	Headers      map[string]string
	APISecretID  string
	CreatedAt    string
	UpdatedAt    string
}

// ValidateAIProfile fails closed on an unknown protocol or an endpoint that
// is not a WebSocket URL. The protocol is always explicit.
func ValidateAIProfile(p AIProfile) error {
	if err := validateAIProfile(p); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidAIProfile, err)
	}
	return nil
}

func validateAIProfile(p AIProfile) error {
	switch p.Protocol {
	case AIProtocolRealtime, AIProtocolLive:
	default:
		return fmt.Errorf("unsupported ai protocol %q", p.Protocol)
	}
	u, err := url.Parse(p.EndpointURL)
	if err != nil || u.Host == "" {
		return errors.New("invalid endpoint url")
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !isLoopbackHost(u.Hostname()) {
			return errors.New("ws endpoints must be loopback; use wss")
		}
	default:
		return errors.New("endpoint must use wss")
	}
	if strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Voice) == "" {
		return errors.New("model and voice are required")
	}
	if len(p.Instructions) > MaxInstructions {
		return fmt.Errorf("instructions exceed %d bytes", MaxInstructions)
	}
	if _, err := realtime.ValidateHeaders(p.Headers); err != nil {
		return err
	}
	return nil
}

// checkAISecret requires a referenced secret to belong to the owner and to be
// an ai_api_key. Missing, foreign and wrong-purpose secrets get the same
// ErrInvalidAIProfile so the response does not reveal which ids exist.
func (s *Store) checkAISecret(ctx context.Context, ownerUserID, secretID string) error {
	if secretID == "" {
		return nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets
		WHERE id = ? AND owner_user_id = ? AND purpose = 'ai_api_key'`, secretID, ownerUserID).Scan(&n)
	if err != nil {
		return fmt.Errorf("storage: check ai secret: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: api secret is not an ai key owned by this user", ErrInvalidAIProfile)
	}
	return nil
}

func encodeHeaders(h map[string]string) (string, error) {
	if len(h) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("storage: encode headers: %w", err)
	}
	return string(b), nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// CreateAIProfile inserts a profile for its owner after validation.
func (s *Store) CreateAIProfile(ctx context.Context, p AIProfile) error {
	if err := ValidateAIProfile(p); err != nil {
		return err
	}
	if err := s.checkAISecret(ctx, p.OwnerUserID, p.APISecretID); err != nil {
		return err
	}
	hdr, err := encodeHeaders(p.Headers)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ai_profiles
		(id, owner_user_id, name, protocol, endpoint_url, model, voice, instructions, headers_json, api_secret_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OwnerUserID, p.Name, string(p.Protocol), p.EndpointURL, p.Model, p.Voice,
		p.Instructions, hdr, nullableString(p.APISecretID), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("storage: create ai profile: %w", err)
	}
	return nil
}

// GetAIProfile returns a profile only when it belongs to ownerUserID.
func (s *Store) GetAIProfile(ctx context.Context, ownerUserID, id string) (AIProfile, error) {
	var p AIProfile
	var proto, hdr string
	var secret sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, owner_user_id, name, protocol, endpoint_url, model, voice, instructions, headers_json, api_secret_id, created_at, updated_at
		FROM ai_profiles WHERE id = ? AND owner_user_id = ?`, id, ownerUserID).
		Scan(&p.ID, &p.OwnerUserID, &p.Name, &proto, &p.EndpointURL, &p.Model, &p.Voice, &p.Instructions, &hdr, &secret, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AIProfile{}, ErrAIProfileNotFound
	}
	if err != nil {
		return AIProfile{}, fmt.Errorf("storage: get ai profile: %w", err)
	}
	if err := json.Unmarshal([]byte(hdr), &p.Headers); err != nil {
		return AIProfile{}, fmt.Errorf("storage: decode headers: %w", err)
	}
	p.Protocol = AIProtocol(proto)
	p.APISecretID = secret.String
	return p, nil
}

// ListAIProfiles returns the owner's profiles, oldest first.
func (s *Store) ListAIProfiles(ctx context.Context, ownerUserID string) ([]AIProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner_user_id, name, protocol, endpoint_url, model, voice, instructions, headers_json, api_secret_id, created_at, updated_at
		FROM ai_profiles WHERE owner_user_id = ? ORDER BY created_at, id`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("storage: list ai profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AIProfile
	for rows.Next() {
		var p AIProfile
		var proto, hdr string
		var secret sql.NullString
		if err := rows.Scan(&p.ID, &p.OwnerUserID, &p.Name, &proto, &p.EndpointURL, &p.Model, &p.Voice, &p.Instructions, &hdr, &secret, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan ai profile: %w", err)
		}
		if err := json.Unmarshal([]byte(hdr), &p.Headers); err != nil {
			return nil, fmt.Errorf("storage: decode headers: %w", err)
		}
		p.Protocol = AIProtocol(proto)
		p.APISecretID = secret.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list ai profiles: %w", err)
	}
	return out, nil
}

// UpdateAIProfile replaces the mutable fields of a profile owned by
// p.OwnerUserID. It returns ErrAIProfileNotFound for another owner's profile.
func (s *Store) UpdateAIProfile(ctx context.Context, p AIProfile) error {
	if err := ValidateAIProfile(p); err != nil {
		return err
	}
	if err := s.checkAISecret(ctx, p.OwnerUserID, p.APISecretID); err != nil {
		return err
	}
	hdr, err := encodeHeaders(p.Headers)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE ai_profiles SET name = ?, protocol = ?, endpoint_url = ?, model = ?, voice = ?,
		instructions = ?, headers_json = ?, api_secret_id = ?, updated_at = ?
		WHERE id = ? AND owner_user_id = ?`,
		p.Name, string(p.Protocol), p.EndpointURL, p.Model, p.Voice, p.Instructions, hdr,
		nullableString(p.APISecretID), p.UpdatedAt, p.ID, p.OwnerUserID)
	if err != nil {
		return fmt.Errorf("storage: update ai profile: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update ai profile: %w", err)
	}
	if n == 0 {
		return ErrAIProfileNotFound
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
