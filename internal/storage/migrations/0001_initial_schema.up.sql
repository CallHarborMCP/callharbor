CREATE TABLE users (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    username TEXT NOT NULL UNIQUE COLLATE NOCASE CHECK (length(username) BETWEEN 1 AND 128),
    role TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    password_hash TEXT,
    oidc_issuer TEXT,
    oidc_subject TEXT,
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    recording_opt_in INTEGER NOT NULL DEFAULT 0 CHECK (recording_opt_in IN (0, 1)),
    recording_acknowledged_at TEXT,
    recording_retention_days INTEGER NOT NULL DEFAULT 30 CHECK (recording_retention_days BETWEEN 1 AND 3650),
    transcript_retention_days INTEGER NOT NULL DEFAULT 90 CHECK (transcript_retention_days BETWEEN 1 AND 3650),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((oidc_issuer IS NULL) = (oidc_subject IS NULL)),
    CHECK (password_hash IS NOT NULL OR oidc_issuer IS NOT NULL),
    CHECK (recording_opt_in = 0 OR recording_acknowledged_at IS NOT NULL),
    UNIQUE (oidc_issuer, oidc_subject)
);

CREATE TABLE settings (
    key TEXT PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 128),
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE secrets (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    owner_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    purpose TEXT NOT NULL CHECK (purpose IN ('telnyx_api_key', 'telnyx_webhook_public_key', 'ai_api_key')),
    ciphertext BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL CHECK (key_version >= 1),
    created_at TEXT NOT NULL,
    rotated_at TEXT,
    UNIQUE (id, owner_user_id),
    UNIQUE (owner_user_id, name)
);

CREATE TABLE telephony_profiles (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    owner_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    provider TEXT NOT NULL CHECK (provider = 'telnyx'),
    caller_number TEXT NOT NULL CHECK (caller_number GLOB '+[1-9][0-9]*' AND length(caller_number) BETWEEN 8 AND 16),
    connection_id TEXT,
    api_secret_id TEXT NOT NULL,
    webhook_public_key_secret_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (id, owner_user_id),
    UNIQUE (owner_user_id, name),
    FOREIGN KEY (api_secret_id, owner_user_id) REFERENCES secrets (id, owner_user_id) ON DELETE RESTRICT,
    FOREIGN KEY (webhook_public_key_secret_id, owner_user_id) REFERENCES secrets (id, owner_user_id) ON DELETE RESTRICT
);

CREATE TABLE ai_profiles (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    owner_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    protocol TEXT NOT NULL CHECK (protocol IN ('realtime_ws', 'live_ws')),
    endpoint_url TEXT NOT NULL CHECK (length(endpoint_url) BETWEEN 8 AND 2048),
    model TEXT NOT NULL CHECK (length(model) BETWEEN 1 AND 256),
    voice TEXT NOT NULL CHECK (length(voice) BETWEEN 1 AND 128),
    api_secret_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (id, owner_user_id),
    UNIQUE (owner_user_id, name),
    FOREIGN KEY (api_secret_id, owner_user_id) REFERENCES secrets (id, owner_user_id) ON DELETE RESTRICT
);

CREATE TABLE calls (
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
    FOREIGN KEY (telephony_profile_id, owner_user_id) REFERENCES telephony_profiles (id, owner_user_id) ON DELETE RESTRICT,
    FOREIGN KEY (ai_profile_id, owner_user_id) REFERENCES ai_profiles (id, owner_user_id) ON DELETE RESTRICT
);

CREATE TABLE call_briefs (
    call_id TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    content_json TEXT NOT NULL CHECK (json_valid(content_json)),
    content_sha256 TEXT NOT NULL CHECK (length(content_sha256) = 64),
    created_at TEXT NOT NULL,
    FOREIGN KEY (call_id, owner_user_id) REFERENCES calls (id, owner_user_id) ON DELETE RESTRICT
);

CREATE TABLE call_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    call_id TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    kind TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),
    state TEXT CHECK (state IN ('queued', 'initiating', 'ringing', 'active', 'ending', 'ended', 'failed')),
    reason_code TEXT,
    provider TEXT CHECK (provider IN ('telnyx', 'internal', 'realtime_ws', 'live_ws')),
    provider_event_id TEXT,
    correlation_id TEXT,
    payload_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json)),
    occurred_at TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    UNIQUE (call_id, seq),
    FOREIGN KEY (call_id, owner_user_id) REFERENCES calls (id, owner_user_id) ON DELETE RESTRICT
);

CREATE TABLE transcript_turns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    call_id TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    speaker TEXT NOT NULL CHECK (speaker IN ('assistant', 'callee', 'system')),
    finality TEXT NOT NULL CHECK (finality IN ('partial', 'final')),
    started_ms INTEGER NOT NULL CHECK (started_ms >= 0),
    ended_ms INTEGER CHECK (ended_ms IS NULL OR ended_ms >= started_ms),
    text TEXT NOT NULL,
    provider_item_id TEXT,
    recorded_at TEXT NOT NULL,
    UNIQUE (call_id, seq, revision),
    FOREIGN KEY (call_id, owner_user_id) REFERENCES calls (id, owner_user_id) ON DELETE RESTRICT
);

CREATE TABLE recordings (
    id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    call_id TEXT NOT NULL UNIQUE,
    owner_user_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('recording', 'complete', 'partial', 'failed', 'deleted')),
    storage_path TEXT NOT NULL UNIQUE CHECK (storage_path NOT LIKE '/%' AND storage_path NOT LIKE '%..%'),
    byte_count INTEGER NOT NULL DEFAULT 0 CHECK (byte_count >= 0),
    sha256 TEXT CHECK (sha256 IS NULL OR length(sha256) = 64),
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    key_version INTEGER NOT NULL CHECK (key_version >= 1),
    created_at TEXT NOT NULL,
    finalized_at TEXT,
    expires_at TEXT,
    FOREIGN KEY (call_id, owner_user_id) REFERENCES calls (id, owner_user_id) ON DELETE RESTRICT,
    CHECK (status = 'recording' OR finalized_at IS NOT NULL)
);

CREATE TABLE provider_webhook_receipts (
    provider TEXT NOT NULL CHECK (provider IN ('telnyx')),
    provider_event_id TEXT NOT NULL CHECK (length(provider_event_id) BETWEEN 1 AND 256),
    call_id TEXT,
    received_at TEXT NOT NULL,
    PRIMARY KEY (provider, provider_event_id)
);

CREATE TABLE retention_purge_markers (
    scope TEXT NOT NULL CHECK (scope IN ('call', 'audit')),
    key TEXT NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
    cutoff_at TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (scope, key),
    CHECK ((scope = 'audit') = (cutoff_at IS NOT NULL))
);

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at TEXT NOT NULL,
    actor_user_id TEXT REFERENCES users (id) ON DELETE RESTRICT,
    actor_role TEXT CHECK (actor_role IN ('admin', 'user', 'system')),
    target_user_id TEXT REFERENCES users (id) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
    resource_type TEXT NOT NULL CHECK (length(resource_type) BETWEEN 1 AND 64),
    resource_id TEXT,
    outcome TEXT NOT NULL CHECK (outcome IN ('success', 'denied', 'failed')),
    detail_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(detail_json))
);

CREATE INDEX calls_owner_created_idx ON calls (owner_user_id, created_at DESC);
CREATE INDEX calls_owner_status_idx ON calls (owner_user_id, status);
CREATE INDEX telephony_profiles_owner_idx ON telephony_profiles (owner_user_id);
CREATE INDEX ai_profiles_owner_idx ON ai_profiles (owner_user_id);
CREATE INDEX secrets_owner_idx ON secrets (owner_user_id);
CREATE INDEX call_events_owner_occurred_idx ON call_events (owner_user_id, occurred_at);
CREATE UNIQUE INDEX call_events_provider_event_idx ON call_events (call_id, provider_event_id) WHERE provider_event_id IS NOT NULL;
CREATE INDEX transcript_turns_owner_call_idx ON transcript_turns (owner_user_id, call_id, seq);
CREATE INDEX recordings_owner_expires_idx ON recordings (owner_user_id, expires_at);
CREATE INDEX provider_webhook_receipts_received_idx ON provider_webhook_receipts (received_at);
CREATE INDEX audit_events_target_occurred_idx ON audit_events (target_user_id, occurred_at);
CREATE INDEX audit_events_actor_occurred_idx ON audit_events (actor_user_id, occurred_at);
CREATE INDEX audit_events_occurred_idx ON audit_events (occurred_at);

CREATE TRIGGER call_briefs_immutable_update
BEFORE UPDATE ON call_briefs
BEGIN
    SELECT RAISE(ABORT, 'call briefs are immutable');
END;

CREATE TRIGGER call_briefs_guarded_delete
BEFORE DELETE ON call_briefs
WHEN NOT EXISTS (SELECT 1 FROM retention_purge_markers WHERE scope = 'call' AND key = OLD.call_id)
BEGIN
    SELECT RAISE(ABORT, 'call briefs may only be deleted by retention purge');
END;

CREATE TRIGGER call_events_immutable_update
BEFORE UPDATE ON call_events
BEGIN
    SELECT RAISE(ABORT, 'call events are append-only');
END;

CREATE TRIGGER call_events_guarded_delete
BEFORE DELETE ON call_events
WHEN NOT EXISTS (SELECT 1 FROM retention_purge_markers WHERE scope = 'call' AND key = OLD.call_id)
BEGIN
    SELECT RAISE(ABORT, 'call events may only be deleted by retention purge');
END;

CREATE TRIGGER transcript_turns_immutable_update
BEFORE UPDATE ON transcript_turns
BEGIN
    SELECT RAISE(ABORT, 'transcript revisions are immutable');
END;

CREATE TRIGGER transcript_turns_guarded_delete
BEFORE DELETE ON transcript_turns
WHEN NOT EXISTS (SELECT 1 FROM retention_purge_markers WHERE scope = 'call' AND key = OLD.call_id)
BEGIN
    SELECT RAISE(ABORT, 'transcript turns may only be deleted by retention purge');
END;

CREATE TRIGGER recordings_guarded_delete
BEFORE DELETE ON recordings
WHEN NOT EXISTS (SELECT 1 FROM retention_purge_markers WHERE scope = 'call' AND key = OLD.call_id)
BEGIN
    SELECT RAISE(ABORT, 'recordings may only be deleted by retention purge');
END;

CREATE TRIGGER calls_guarded_delete
BEFORE DELETE ON calls
WHEN NOT EXISTS (SELECT 1 FROM retention_purge_markers WHERE scope = 'call' AND key = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'calls may only be deleted by retention purge');
END;

CREATE TRIGGER audit_events_immutable_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;

CREATE TRIGGER audit_events_guarded_delete
BEFORE DELETE ON audit_events
WHEN NOT EXISTS (
    SELECT 1 FROM retention_purge_markers m
    WHERE m.scope = 'audit' AND OLD.occurred_at < m.cutoff_at
)
BEGIN
    SELECT RAISE(ABORT, 'audit events may only be deleted by retention purge');
END;

CREATE TRIGGER retention_purge_markers_immutable_update
BEFORE UPDATE ON retention_purge_markers
BEGIN
    SELECT RAISE(ABORT, 'retention purge markers are immutable');
END;
