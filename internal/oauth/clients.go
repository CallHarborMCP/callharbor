package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// storeResolver resolves clients registered in storage. It is the resolver
// for every client_id that is not a metadata-document URL.
type storeResolver struct{ store Store }

// ResolveClient returns the stored client, or ErrUnknownClient if none exists.
func (r storeResolver) ResolveClient(ctx context.Context, clientID string) (Client, error) {
	c, err := r.store.GetOAuthClient(ctx, clientID)
	if errors.Is(err, storage.ErrNotFound) {
		return Client{}, ErrUnknownClient
	}
	if err != nil {
		return Client{}, fmt.Errorf("oauth: load client: %w", err)
	}
	var uris []string
	if err := json.Unmarshal([]byte(c.RedirectURIs), &uris); err != nil {
		return Client{}, fmt.Errorf("oauth: stored redirect_uris: %w", err)
	}
	var grants []string
	if err := json.Unmarshal([]byte(c.GrantTypes), &grants); err != nil {
		return Client{}, fmt.Errorf("oauth: stored grant_types: %w", err)
	}
	return Client{
		ID:               c.ClientID,
		Name:             c.ClientName,
		URI:              c.ClientURI,
		RedirectURIs:     uris,
		GrantTypes:       grants,
		MetadataDocument: c.Source == "metadata_document",
	}, nil
}

// compositeResolver sends metadata-document URLs to CIMD and everything else
// to storage. A metadata client is mirrored into oauth_clients when it is
// fetched, not on cache hits, because the authorization request and grant
// rows join that table.
type compositeResolver struct {
	store Store
	cimd  *cimdResolver
	db    ClientResolver
	now   Clock
	// mirrorCap bounds mirrored rows. Tests lower it through newClientResolver.
	mirrorCap int
}

// NewClientResolver is the production resolver: the default SSRF policy and
// system root CAs for metadata-document fetches.
func NewClientResolver(store Store, clock Clock) ClientResolver {
	return newClientResolver(store, clock, cimdOptions{})
}

// newClientResolver is the shared constructor. Tests pass cimdOptions to reach
// a loopback httptest server; production passes the zero value.
func newClientResolver(store Store, clock Clock, opts cimdOptions) ClientResolver {
	return &compositeResolver{
		store:     store,
		cimd:      newCIMDResolver(clock, opts),
		db:        storeResolver{store: store},
		now:       clock,
		mirrorCap: cimdMirrorCap,
	}
}

// ResolveClient implements ClientResolver.
func (r *compositeResolver) ResolveClient(ctx context.Context, clientID string) (Client, error) {
	if !isCIMDClientID(clientID) {
		return r.db.ResolveClient(ctx, clientID)
	}
	c, fetched, err := r.cimd.resolve(ctx, clientID)
	if err != nil {
		return Client{}, ErrUnknownClient
	}
	if fetched {
		if err := r.mirror(ctx, c); err != nil {
			return Client{}, ErrUnknownClient
		}
	}
	return c, nil
}

// cimdMirrorCap bounds mirrored metadata-document rows in oauth_clients.
// Rows are never evicted: grants and requests reference them with ON DELETE
// RESTRICT, so a row in use cannot be removed. A new document beyond the cap
// is refused instead.
const cimdMirrorCap = 10000

// errMetadataClientCapReached is returned when the mirror table is full.
var errMetadataClientCapReached = errors.New("oauth: metadata client capacity reached")

// mirror writes a freshly fetched metadata client into oauth_clients.
func (r *compositeResolver) mirror(ctx context.Context, c Client) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return fmt.Errorf("oauth: encode redirect_uris: %w", err)
	}
	// An empty list stays empty so storage applies the default pair; a nil
	// slice would otherwise encode as JSON null, which the column rejects.
	var grants []byte
	if len(c.GrantTypes) > 0 {
		if grants, err = json.Marshal(c.GrantTypes); err != nil {
			return fmt.Errorf("oauth: encode grant_types: %w", err)
		}
	}
	// Refreshing a known document is always allowed. Only a new row counts
	// against the cap, so a flood of distinct metadata URLs is refused.
	if _, err := r.store.GetOAuthClient(ctx, c.ID); errors.Is(err, storage.ErrNotFound) {
		n, err := r.store.CountMetadataOAuthClients(ctx)
		if err != nil {
			return fmt.Errorf("oauth: count metadata clients: %w", err)
		}
		if n >= r.mirrorCap {
			return errMetadataClientCapReached
		}
	}
	now := formatTime(r.now())
	if err := r.store.UpsertOAuthClient(ctx, storage.OAuthClient{
		ClientID:     c.ID,
		Source:       "metadata_document",
		ClientName:   c.Name,
		ClientURI:    c.URI,
		RedirectURIs: string(uris),
		GrantTypes:   string(grants),

		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("oauth: mirror metadata client: %w", err)
	}
	return nil
}
