-- Registered grant types per client. Existing rows and clients registered
-- without a list get the default: authorization_code and refresh_token.
ALTER TABLE oauth_clients ADD COLUMN grant_types_json TEXT NOT NULL DEFAULT '["authorization_code","refresh_token"]'
    CHECK (json_valid(grant_types_json) AND json_type(grant_types_json) = 'array');
