-- OAuth 2.1 authorization server for remote MCP. Access tokens, refresh tokens
-- and authorization codes are opaque random values stored only as SHA-256
-- hashes. No JWTs or ID tokens are issued, so there is no signing key.

CREATE TABLE oauth_clients (
    client_id TEXT PRIMARY KEY CHECK (length(client_id) BETWEEN 1 AND 2048),
    source TEXT NOT NULL CHECK (source IN ('dynamic', 'metadata_document')),
    client_name TEXT NOT NULL DEFAULT '' CHECK (length(client_name) <= 256),
    client_uri TEXT NOT NULL DEFAULT '' CHECK (length(client_uri) <= 2048),
    redirect_uris_json TEXT NOT NULL CHECK (json_valid(redirect_uris_json) AND json_type(redirect_uris_json) = 'array'),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (source = 'dynamic' OR client_id LIKE 'https://%')
);

-- A grant family ties together every code, access token and refresh token
-- issued to one user for one client. Revoking the family revokes them all.
CREATE TABLE oauth_grants (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    client_id TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE RESTRICT,
    resource TEXT NOT NULL CHECK (length(resource) BETWEEN 1 AND 2048),
    scopes TEXT NOT NULL CHECK (length(scopes) BETWEEN 1 AND 512),
    created_at TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at TEXT,
    UNIQUE (id, user_id)
);

CREATE INDEX oauth_grants_user_idx ON oauth_grants (user_id);

CREATE TABLE oauth_auth_requests (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    client_id TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE RESTRICT,
    redirect_uri TEXT NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 2048),
    state TEXT NOT NULL DEFAULT '' CHECK (length(state) <= 1024),
    scopes TEXT NOT NULL CHECK (length(scopes) BETWEEN 1 AND 512),
    resource TEXT NOT NULL CHECK (length(resource) BETWEEN 1 AND 2048),
    code_challenge TEXT NOT NULL CHECK (length(code_challenge) BETWEEN 43 AND 128),
    nonce TEXT NOT NULL DEFAULT '' CHECK (length(nonce) <= 256),
    user_id TEXT REFERENCES users (id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    decided_at TEXT,
    consumed_at TEXT
);

CREATE INDEX oauth_auth_requests_expires_idx ON oauth_auth_requests (expires_at);

CREATE TABLE oauth_codes (
    code_hash TEXT PRIMARY KEY CHECK (length(code_hash) = 64),
    grant_id TEXT NOT NULL REFERENCES oauth_grants (id) ON DELETE RESTRICT,
    auth_request_id TEXT NOT NULL REFERENCES oauth_auth_requests (id) ON DELETE RESTRICT,
    client_id TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE RESTRICT,
    redirect_uri TEXT NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 2048),
    code_challenge TEXT NOT NULL CHECK (length(code_challenge) BETWEEN 43 AND 128),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);

CREATE TABLE oauth_access_tokens (
    token_hash TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    grant_id TEXT NOT NULL REFERENCES oauth_grants (id) ON DELETE RESTRICT,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    client_id TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE RESTRICT,
    audience TEXT NOT NULL CHECK (length(audience) BETWEEN 1 AND 2048),
    scopes TEXT NOT NULL CHECK (length(scopes) BETWEEN 1 AND 512),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT
);

CREATE INDEX oauth_access_tokens_grant_idx ON oauth_access_tokens (grant_id);

CREATE TABLE oauth_refresh_tokens (
    token_hash TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    grant_id TEXT NOT NULL REFERENCES oauth_grants (id) ON DELETE RESTRICT,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    client_id TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE RESTRICT,
    audience TEXT NOT NULL CHECK (length(audience) BETWEEN 1 AND 2048),
    scopes TEXT NOT NULL CHECK (length(scopes) BETWEEN 1 AND 512),
    created_at TEXT NOT NULL,
    absolute_expires_at TEXT NOT NULL,
    rotated_at TEXT,
    revoked_at TEXT
);

CREATE INDEX oauth_refresh_tokens_grant_idx ON oauth_refresh_tokens (grant_id);
