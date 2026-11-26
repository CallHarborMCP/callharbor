-- callharbor:foreign_keys=off
-- Owner-granted telephony profiles can carry calls placed by the grantee. The
-- call row is owned by the grantee, so the composite foreign key to
-- telephony_profiles(id, owner_user_id) cannot hold those rows. The profile
-- reference becomes a plain reference to telephony_profiles(id). Access is
-- checked by the telephony profile resolver before any call row is written.
-- A profile still cannot be deleted while a call references it.
--
-- SQLite cannot alter a foreign key in place, so calls is rebuilt with the
-- procedure from https://www.sqlite.org/lang_altertable.html. The runner turns
-- foreign key enforcement off for this script and verifies with
-- foreign_key_check before commit. Every other constraint, index, and trigger
-- on calls is recreated. Child tables keep referencing calls(id, owner_user_id).

CREATE TABLE calls_new (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    owner_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    telephony_profile_id TEXT NOT NULL,
    ai_profile_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    to_number TEXT NOT NULL CHECK (to_number GLOB '+[1-9][0-9]*' AND length(to_number) BETWEEN 8 AND 16),
    status TEXT NOT NULL CHECK (status IN ('queued', 'initiating', 'ringing', 'active', 'ending', 'ended', 'failed')),
    outcome TEXT CHECK (outcome IN ('success', 'partial', 'failed', 'unknown')),
    outcome_reason_code TEXT,
    provider_call_control_id TEXT UNIQUE,
    record_requested INTEGER NOT NULL DEFAULT 0 CHECK (record_requested IN (0, 1)),
    record_effective INTEGER NOT NULL DEFAULT 0 CHECK (record_effective IN (0, 1)),
    max_duration_seconds INTEGER NOT NULL CHECK (max_duration_seconds BETWEEN 1 AND 7200),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    ended_at TEXT,
    UNIQUE (id, owner_user_id),
    UNIQUE (owner_user_id, idempotency_key),
    CHECK (record_effective = 0 OR record_requested = 1),
    CHECK ((status IN ('ended', 'failed')) = (ended_at IS NOT NULL)),
    CHECK (outcome IS NULL OR status IN ('ended', 'failed')),
    FOREIGN KEY (telephony_profile_id) REFERENCES telephony_profiles (id) ON DELETE RESTRICT,
    FOREIGN KEY (ai_profile_id, owner_user_id) REFERENCES ai_profiles (id, owner_user_id) ON DELETE RESTRICT
);

INSERT INTO calls_new (
    id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number,
    status, outcome, outcome_reason_code, provider_call_control_id, record_requested,
    record_effective, max_duration_seconds, created_at, updated_at, ended_at
)
SELECT
    id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number,
    status, outcome, outcome_reason_code, provider_call_control_id, record_requested,
    record_effective, max_duration_seconds, created_at, updated_at, ended_at
FROM calls;

DROP TABLE calls;
ALTER TABLE calls_new RENAME TO calls;

CREATE INDEX calls_owner_created_idx ON calls (owner_user_id, created_at DESC);
CREATE INDEX calls_owner_status_idx ON calls (owner_user_id, status);

CREATE TRIGGER calls_guarded_delete
BEFORE DELETE ON calls
WHEN NOT EXISTS (SELECT 1 FROM retention_purge_markers WHERE scope = 'call' AND key = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'calls may only be deleted by retention purge');
END;

-- The composite foreign key used to stop a call from naming another tenant's
-- profile. This trigger keeps that guarantee for the relaxed key: a call's
-- telephony profile must be owned by the call's owner or granted to the call's
-- owner. The access rule (active owner, active admin for grants) is enforced by
-- the telephony profile resolver before any write.
CREATE TRIGGER calls_telephony_access_insert
BEFORE INSERT ON calls
WHEN NOT EXISTS (SELECT 1 FROM telephony_profiles p WHERE p.id = NEW.telephony_profile_id AND p.owner_user_id = NEW.owner_user_id)
    AND NOT EXISTS (SELECT 1 FROM telephony_profile_grants g WHERE g.profile_id = NEW.telephony_profile_id AND g.grantee_user_id = NEW.owner_user_id)
BEGIN
    SELECT RAISE(ABORT, 'call telephony profile is not owned or granted to the call owner');
END;

CREATE TRIGGER calls_telephony_access_update
BEFORE UPDATE OF telephony_profile_id, owner_user_id ON calls
WHEN NOT EXISTS (SELECT 1 FROM telephony_profiles p WHERE p.id = NEW.telephony_profile_id AND p.owner_user_id = NEW.owner_user_id)
    AND NOT EXISTS (SELECT 1 FROM telephony_profile_grants g WHERE g.profile_id = NEW.telephony_profile_id AND g.grantee_user_id = NEW.owner_user_id)
BEGIN
    SELECT RAISE(ABORT, 'call telephony profile is not owned or granted to the call owner');
END;
