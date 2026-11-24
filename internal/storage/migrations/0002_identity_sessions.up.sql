CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    csrf_token TEXT NOT NULL CHECK (length(csrf_token) BETWEEN 32 AND 128),
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT
);

CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE login_throttle (
    key TEXT PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 256),
    failures INTEGER NOT NULL CHECK (failures >= 0),
    window_started_at TEXT NOT NULL,
    locked_until TEXT,
    updated_at TEXT NOT NULL
);
