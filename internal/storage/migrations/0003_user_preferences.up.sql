CREATE TABLE user_preferences (
    user_id TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    theme TEXT NOT NULL CHECK (theme IN ('system', 'light', 'dark')),
    preset TEXT NOT NULL CHECK (preset IN ('console', 'harbor')),
    accent TEXT NOT NULL CHECK (accent IN ('blue', 'teal', 'violet', 'green')),
    density TEXT NOT NULL CHECK (density IN ('comfortable', 'compact')),
    updated_at TEXT NOT NULL
);
