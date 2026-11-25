-- Telnyx telephony profile connectivity and owner-granted use.
--
-- connectivity_status is the last read-only validation result. It starts as
-- 'unknown' and is never reported as healthy unless a validation succeeded.
-- connectivity_reason is a fixed machine code from the validation taxonomy,
-- never provider text. connectivity_checked_at is the time of that check.

ALTER TABLE telephony_profiles ADD COLUMN connectivity_status TEXT NOT NULL DEFAULT 'unknown'
    CHECK (connectivity_status IN ('unknown', 'verified', 'failed'));
ALTER TABLE telephony_profiles ADD COLUMN connectivity_reason TEXT NOT NULL DEFAULT ''
    CHECK (length(connectivity_reason) <= 64);
ALTER TABLE telephony_profiles ADD COLUMN connectivity_checked_at TEXT;

-- A grant lets another user use a profile for calls. It never exposes the
-- profile's secrets. Only an admin who owns the profile may create, list or
-- remove grants. Deleting the profile removes its grants.
CREATE TABLE telephony_profile_grants (
    profile_id TEXT NOT NULL REFERENCES telephony_profiles (id) ON DELETE CASCADE,
    grantee_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    granted_by_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (profile_id, grantee_user_id)
);

-- Grant lookups by grantee list every profile a user can use.
CREATE INDEX telephony_profile_grants_grantee_idx ON telephony_profile_grants (grantee_user_id);
