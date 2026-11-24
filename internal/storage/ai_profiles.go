package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// AIProtocol is the explicit AI voice protocol stored on a profile. It is
// never inferred from the model or endpoint.
type AIProtocol string

const (
	AIProtocolRealtime AIProtocol = "realtime_ws"
	AIProtocolLive     AIProtocol = "live_ws"
)

// ErrAIProfileNotFound reports a missing profile or one owned by another user.
var ErrAIProfileNotFound = errors.New("storage: ai profile not found")

// AIProfile is one user-owned AI voice configuration. APISecretID references
// an owner-scoped secret; the secret value is never stored here.
type AIProfile struct {
	ID          string
	OwnerUserID string
	Name        string
	Protocol    AIProtocol
	EndpointURL string
	Model       string
	Voice       string
	APISecretID string
	CreatedAt   string
	UpdatedAt   string
}

// ValidateAIProfile fails closed on an unknown protocol or an endpoint that
// is not a WebSocket URL. The protocol is always explicit.
func ValidateAIProfile(p AIProfile) error {
	switch p.Protocol {
	case AIProtocolRealtime, AIProtocolLive:
	default:
		return fmt.Errorf("storage: unsupported ai protocol %q", p.Protocol)
	}
	u, err := url.Parse(p.EndpointURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("storage: invalid endpoint url")
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("storage: ws endpoints must be loopback; use wss")
		}
	default:
		return fmt.Errorf("storage: endpoint must use wss")
	}
	if strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Voice) == "" {
		return errors.New("storage: model and voice are required")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// CreateAIProfile inserts a profile for its owner after validation.
func (s *Store) CreateAIProfile(ctx context.Context, p AIProfile) error {
	if err := ValidateAIProfile(p); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ai_profiles
		(id, owner_user_id, name, protocol, endpoint_url, model, voice, api_secret_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OwnerUserID, p.Name, string(p.Protocol), p.EndpointURL, p.Model, p.Voice,
		nullableString(p.APISecretID), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("storage: create ai profile: %w", err)
	}
	return nil
}

// GetAIProfile returns a profile only when it belongs to ownerUserID.
func (s *Store) GetAIProfile(ctx context.Context, ownerUserID, id string) (AIProfile, error) {
	var p AIProfile
	var proto string
	var secret sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, owner_user_id, name, protocol, endpoint_url, model, voice, api_secret_id, created_at, updated_at
		FROM ai_profiles WHERE id = ? AND owner_user_id = ?`, id, ownerUserID).
		Scan(&p.ID, &p.OwnerUserID, &p.Name, &proto, &p.EndpointURL, &p.Model, &p.Voice, &secret, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AIProfile{}, ErrAIProfileNotFound
	}
	if err != nil {
		return AIProfile{}, fmt.Errorf("storage: get ai profile: %w", err)
	}
	p.Protocol = AIProtocol(proto)
	p.APISecretID = secret.String
	return p, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
