-- Reference schema for Game List Manager.
-- Applied automatically by internal/database/sqlite.go (versioned migrations
-- tracked in schema_migrations). This file exists for humans and DB tools.

-- v1 -------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS games (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    igdb_id      INTEGER,                 -- matched IGDB game ID (NULL if unmatched)
    title        TEXT NOT NULL,
    description  TEXT,
    release_date TEXT,                    -- YYYY-MM-DD (UTC)
    developers   TEXT,                    -- JSON array of strings
    publishers   TEXT,                    -- JSON array of strings
    platforms    TEXT,                    -- JSON array of strings
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_games_title ON games(title);
CREATE UNIQUE INDEX IF NOT EXISTS idx_games_igdb_id ON games(igdb_id) WHERE igdb_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS game_stores (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    game_id    INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    store_name TEXT NOT NULL,             -- canonical store key (models.Store*)
    store_id   TEXT NOT NULL,             -- store-specific game ID (e.g. Steam appid)
    UNIQUE(game_id, store_name)
);

CREATE INDEX IF NOT EXISTS idx_game_stores_game ON game_stores(game_id);
CREATE INDEX IF NOT EXISTS idx_game_stores_store ON game_stores(store_name);

CREATE TABLE IF NOT EXISTS store_credentials (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    store_name    TEXT NOT NULL UNIQUE,   -- canonical store key, or "_igdb" for the cached Twitch token
    access_token  TEXT,
    refresh_token TEXT,
    expires_at    INTEGER,                -- unix timestamp; 0 = not applicable
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS sync_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    store_name  TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',  -- pending | success | error
    message     TEXT,
    started_at  TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at TEXT
);

-- v2 -------------------------------------------------------------------------

ALTER TABLE store_credentials ADD COLUMN meta TEXT;  -- JSON extras (e.g. Steam api_key/steam_id)
