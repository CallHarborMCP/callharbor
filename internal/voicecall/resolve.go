// Package voicecall wires one AI call path end to end: an owner's AI profile,
// its encrypted API key, the protocol dialer, the voice engine and the Telnyx
// media bridge. It is the only place these layers meet; each stays independent
// of the others.
package voicecall

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// ErrNoAPIKey reports an AI profile that has no ai_api_key secret bound. A
// call is never started without a key, and a key is never sent in clear.
var ErrNoAPIKey = errors.New("voicecall: ai profile has no api key")

// ProfileStore loads an owner's AI profile.
type ProfileStore interface {
	GetAIProfile(ctx context.Context, ownerUserID, id string) (storage.AIProfile, error)
}

// SecretResolver returns the plaintext of an owner's secret for one purpose.
type SecretResolver interface {
	Resolve(ctx context.Context, owner, id, purpose string) ([]byte, error)
}

// Resolver maps (owner, profile id) to a realtime.Profile. The API key is
// taken from the owner's ai_api_key secret. Protocol is carried through
// unchanged; the resolver never picks a protocol itself.
type Resolver struct {
	profiles ProfileStore
	secrets  SecretResolver
	format   audio.Format
	client   *http.Client
}

// NewResolver builds a resolver. format is the provider wire format. policy
// governs which resolved addresses provider dials may reach; the default
// policy refuses private and link-local addresses, so an owner cannot point a
// profile at an internal host. The HTTP client is built here from the policy,
// so an unchecked client cannot be injected.
func NewResolver(profiles ProfileStore, sec SecretResolver, format audio.Format, policy realtime.EndpointPolicy) (*Resolver, error) {
	if profiles == nil || sec == nil {
		return nil, errors.New("voicecall: resolver requires profile and secret stores")
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	return &Resolver{profiles: profiles, secrets: sec, format: format, client: policy.HTTPClient()}, nil
}

// Resolve returns the provider profile for ownerUserID's AI profile profileID.
// It fails with storage.ErrAIProfileNotFound for another owner's profile and
// with ErrNoAPIKey when no key is bound.
func (r *Resolver) Resolve(ctx context.Context, ownerUserID, profileID string) (realtime.Profile, error) {
	p, err := r.profiles.GetAIProfile(ctx, ownerUserID, profileID)
	if err != nil {
		return realtime.Profile{}, err
	}
	if p.APISecretID == "" {
		return realtime.Profile{}, ErrNoAPIKey
	}
	key, err := r.secrets.Resolve(ctx, ownerUserID, p.APISecretID, secrets.PurposeAIAPIKey)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return realtime.Profile{}, ErrNoAPIKey
		}
		return realtime.Profile{}, fmt.Errorf("voicecall: resolve api key: %w", err)
	}
	return realtime.Profile{
		Protocol:     realtime.Protocol(p.Protocol),
		Endpoint:     p.EndpointURL,
		APIKey:       string(key),
		Model:        p.Model,
		Voice:        p.Voice,
		Instructions: p.Instructions,
		Headers:      p.Headers,
		Format:       r.format,
		HTTPClient:   r.client,
	}, nil
}
