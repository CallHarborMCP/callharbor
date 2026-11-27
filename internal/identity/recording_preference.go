package identity

import (
	"context"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// Audit action for recording opt-in changes. The detail carries only the
// before and after state, never a secret.
const auditRecordingPreference = "recording.preference_changed"

// RecordingPreference is the owner's recording setting as the API exposes it.
// AcknowledgedAt is nil while the opt-in has never been acknowledged.
type RecordingPreference struct {
	Enabled        bool    `json:"enabled"`
	AcknowledgedAt *string `json:"acknowledged_at"`
	RetentionDays  int     `json:"retention_days"`
}

// GetRecordingPreference returns the caller's own recording opt-in. It reads
// the principal's user only; there is no user parameter.
func (s *Service) GetRecordingPreference(ctx context.Context, actor *Principal) (RecordingPreference, error) {
	if actor == nil {
		return RecordingPreference{}, ErrUnauthenticated
	}
	p, err := s.store.GetRecordingPreference(ctx, actor.User.ID)
	if err != nil {
		return RecordingPreference{}, err
	}
	return preferenceView(p), nil
}

// SetRecordingPreference changes the caller's own opt-in. Turning recording on
// requires acknowledged=true, the owner's acknowledgement of the consent notice
// shown in the WebGUI. Turning it off never needs an acknowledgement. Every
// change is audited, and the audit detail carries the before and after values.
func (s *Service) SetRecordingPreference(ctx context.Context, actor *Principal, enabled, acknowledged bool) (RecordingPreference, error) {
	if actor == nil {
		return RecordingPreference{}, ErrUnauthenticated
	}
	if enabled && !acknowledged {
		return RecordingPreference{}, ErrInvalidInput
	}
	before, err := s.store.GetRecordingPreference(ctx, actor.User.ID)
	if err != nil {
		return RecordingPreference{}, err
	}
	now := ts(s.now())
	ackAt := ""
	if enabled {
		ackAt = now
	}
	detail := auditDetail(before.Enabled, enabled)
	pref, err := s.store.SetRecordingEnabled(ctx, actor.User.ID, enabled, ackAt, now,
		s.audit(actor, actor.User.ID, auditRecordingPreference, "user", actor.User.ID, "success", detail))
	if err != nil {
		return RecordingPreference{}, err
	}
	return preferenceView(pref), nil
}

// auditDetail is the before and after opt-in as JSON. It holds no secret.
func auditDetail(before, after bool) string {
	return `{"from_enabled":` + boolJSON(before) + `,"to_enabled":` + boolJSON(after) + `}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func preferenceView(p storage.RecordingPreference) RecordingPreference {
	v := RecordingPreference{Enabled: p.Enabled, RetentionDays: p.RetentionDays}
	if p.AcknowledgedAt != "" {
		v.AcknowledgedAt = &p.AcknowledgedAt
	}
	return v
}
