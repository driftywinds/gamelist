package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO required on Windows)
)

// DB wraps sql.DB with project-specific helpers. The modernc driver name is
// "sqlite" (mattn/go-sqlite3 used "sqlite3").
type DB struct {
	*sql.DB
}

// StoreCredential mirrors one row of the store_credentials table.
type StoreCredential struct {
	StoreName    string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64  // unix timestamp; 0 = not applicable
	Meta         string // optional JSON blob with store-specific extras
}

// GameRecord bridges the games table and the service layer.
type GameRecord struct {
	ID             int64
	IgdbID         *int // nil = not matched to IGDB
	Title          string
	Description    string
	ReleaseDate    string
	DevelopersJSON string
	PublishersJSON string
	PlatformsJSON  string
}

// NewSQLiteDB opens (or creates) the database at path and runs migrations.
//
// Connection-scoped pragmas (foreign_keys, busy_timeout) are attached to the
// DSN so that EVERY pooled connection gets them; journal_mode=WAL persists in
// the database file itself. Setting PRAGMAs via db.Exec alone would only
// affect whichever single pooled connection happened to run them.
func NewSQLiteDB(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database dir: %w", err)
		}
	}

	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	wrapper := &DB{db}
	if err := wrapper.runMigrations(); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return wrapper, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.DB.Close()
}

// ---------------------------------------------------------------------------
// Migrations (versioned; see schema_migrations)
// ---------------------------------------------------------------------------

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		sql: `CREATE TABLE IF NOT EXISTS games (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				igdb_id      INTEGER,
				title        TEXT NOT NULL,
				description  TEXT,
				release_date TEXT,
				developers   TEXT, -- JSON array of strings
				publishers   TEXT, -- JSON array of strings
				platforms    TEXT, -- JSON array of strings
				created_at   TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE INDEX IF NOT EXISTS idx_games_title ON games(title);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_games_igdb_id ON games(igdb_id) WHERE igdb_id IS NOT NULL;

			CREATE TABLE IF NOT EXISTS game_stores (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				game_id    INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
				store_name TEXT NOT NULL, -- canonical store key (see models.Store*)
				store_id   TEXT NOT NULL, -- store-specific game ID (e.g. Steam appid)
				UNIQUE(game_id, store_name)
			);

			CREATE INDEX IF NOT EXISTS idx_game_stores_game ON game_stores(game_id);
			CREATE INDEX IF NOT EXISTS idx_game_stores_store ON game_stores(store_name);

			CREATE TABLE IF NOT EXISTS store_credentials (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				store_name    TEXT NOT NULL UNIQUE,
				access_token  TEXT,
				refresh_token TEXT,
				expires_at    INTEGER,
				updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE TABLE IF NOT EXISTS sync_log (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				store_name  TEXT NOT NULL,
				status      TEXT NOT NULL DEFAULT 'pending', -- pending | success | error
				message     TEXT,
				started_at  TEXT NOT NULL DEFAULT (datetime('now')),
				finished_at TEXT
			);`,
	},
	{
		// v2 adds a JSON column for store-specific credential extras
		// (e.g. Steam api_key/steam_id are kept here after `signin`).
		version: 2,
		sql:     `ALTER TABLE store_credentials ADD COLUMN meta TEXT;`,
	},
}

func (db *DB) runMigrations() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate schema_migrations: %w", err)
	}
	rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			return fmt.Errorf("migration v%d: %w", m.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, m.version); err != nil {
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Game CRUD helpers
// ---------------------------------------------------------------------------

// UpsertGame inserts a game or updates the existing row. Matching order:
//  1. by IGDB ID (when known),
//  2. by title (case-insensitive) so games without an IGDB match do not
//     duplicate on every sync,
//  3. insert as new.
//
// Returns the row id.
func (db *DB) UpsertGame(g *GameRecord) (int64, error) {
	if g.IgdbID != nil && *g.IgdbID > 0 {
		var id int64
		err := db.QueryRow(`SELECT id FROM games WHERE igdb_id = ?`, *g.IgdbID).Scan(&id)
		switch {
		case err == nil:
			// Empty incoming fields preserve whatever a previous sync stored,
			// so incomplete-mode saves never wipe IGDB enrichment data.
			_, uerr := db.Exec(`UPDATE games
				SET title=?,
				    description=CASE WHEN ? = '' THEN description ELSE ? END,
				    release_date=CASE WHEN ? = '' THEN release_date ELSE ? END,
				    developers=CASE WHEN ? = '' THEN developers ELSE ? END,
				    publishers=CASE WHEN ? = '' THEN publishers ELSE ? END,
				    platforms=CASE WHEN ? = '' THEN platforms ELSE ? END,
				    updated_at=datetime('now')
				WHERE id=?`,
				g.Title,
				g.Description, g.Description,
				g.ReleaseDate, g.ReleaseDate,
				g.DevelopersJSON, g.DevelopersJSON,
				g.PublishersJSON, g.PublishersJSON,
				g.PlatformsJSON, g.PlatformsJSON,
				id)
			return id, uerr
		case !errors.Is(err, sql.ErrNoRows):
			return 0, fmt.Errorf("find game by igdb_id: %w", err)
		}
		// Not found by IGDB ID: fall through so a previously-unmatched title
		// row adopts this IGDB ID instead of a duplicate row appearing.
	}

	var id int64
	err := db.QueryRow(`SELECT id FROM games WHERE lower(title) = lower(?)`, g.Title).Scan(&id)
	switch {
	case err == nil:
		_, uerr := db.Exec(`UPDATE games
			SET igdb_id=COALESCE(?, igdb_id),
			    title=?,
			    description=CASE WHEN ? = '' THEN description ELSE ? END,
			    release_date=CASE WHEN ? = '' THEN release_date ELSE ? END,
			    developers=CASE WHEN ? = '' THEN developers ELSE ? END,
			    publishers=CASE WHEN ? = '' THEN publishers ELSE ? END,
			    platforms=CASE WHEN ? = '' THEN platforms ELSE ? END,
			    updated_at=datetime('now')
			WHERE id=?`,
			g.IgdbID, g.Title,
			g.Description, g.Description,
			g.ReleaseDate, g.ReleaseDate,
			g.DevelopersJSON, g.DevelopersJSON,
			g.PublishersJSON, g.PublishersJSON,
			g.PlatformsJSON, g.PlatformsJSON,
			id)
		return id, uerr
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("find game by title: %w", err)
	}

	res, err := db.Exec(`INSERT INTO games (igdb_id, title, description, release_date, developers, publishers, platforms)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.IgdbID, g.Title, g.Description, g.ReleaseDate,
		g.DevelopersJSON, g.PublishersJSON, g.PlatformsJSON)
	if err != nil {
		return 0, fmt.Errorf("insert game: %w", err)
	}
	return res.LastInsertId()
}

// AddGameStoreLink links a game row to a canonical store key (idempotent).
func (db *DB) AddGameStoreLink(gameRowID int64, storeName, storeID string) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO game_stores (game_id, store_name, store_id) VALUES (?, ?, ?)`,
		gameRowID, storeName, storeID)
	return err
}

// PruneStoreLinks removes a store's ownership links that were NOT part of the
// latest successful fetch (keep lists the store_ids to keep; an empty-string
// entry keeps ownership-only links that carry no store id), then deletes
// games that lost their last link. Only call this for stores whose fetch
// succeeded - a failed fetch means unknown, not unowned.
func (db *DB) PruneStoreLinks(storeName string, keep []string) error {
	keepSet := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepSet[id] = true
	}

	rows, err := db.Query(`SELECT id, store_id FROM game_stores WHERE store_name = ?`, storeName)
	if err != nil {
		return fmt.Errorf("list %s links: %w", storeName, err)
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var storeID string
		if err := rows.Scan(&id, &storeID); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s link: %w", storeName, err)
		}
		if !keepSet[storeID] {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate %s links: %w", storeName, err)
	}
	rows.Close()

	for _, id := range stale {
		if _, err := db.Exec(`DELETE FROM game_stores WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete stale %s link: %w", storeName, err)
		}
	}

	// Games that lost their last ownership link are dead data.
	if _, err := db.Exec(`DELETE FROM games WHERE id NOT IN (SELECT DISTINCT game_id FROM game_stores)`); err != nil {
		return fmt.Errorf("delete orphaned games: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store credential helpers
// ---------------------------------------------------------------------------

// SaveStoreCredentials upserts credentials for a store. meta is optional JSON
// (pass "" when unused).
func (db *DB) SaveStoreCredentials(storeName, accessToken, refreshToken string, expiresAt int64, meta string) error {
	_, err := db.Exec(`INSERT INTO store_credentials (store_name, access_token, refresh_token, expires_at, meta)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(store_name) DO UPDATE SET
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			meta=excluded.meta,
			updated_at=datetime('now')`,
		storeName, accessToken, refreshToken, expiresAt, meta)
	return err
}

// GetStoreCredential returns the stored credentials, or (nil, nil) when the
// store has never been signed in.
func (db *DB) GetStoreCredential(storeName string) (*StoreCredential, error) {
	var c StoreCredential
	err := db.QueryRow(`SELECT store_name, COALESCE(access_token,''), COALESCE(refresh_token,''),
		COALESCE(expires_at,0), COALESCE(meta,'')
		FROM store_credentials WHERE store_name = ?`, storeName).
		Scan(&c.StoreName, &c.AccessToken, &c.RefreshToken, &c.ExpiresAt, &c.Meta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ClearStoreCredentials removes stored credentials for a store.
func (db *DB) ClearStoreCredentials(storeName string) error {
	_, err := db.Exec(`DELETE FROM store_credentials WHERE store_name = ?`, storeName)
	return err
}

// ---------------------------------------------------------------------------
// Sync log helpers
// ---------------------------------------------------------------------------

// LogSyncStart records the beginning of a sync operation and returns its id.
func (db *DB) LogSyncStart(storeName string) (int64, error) {
	res, err := db.Exec(`INSERT INTO sync_log (store_name, status) VALUES (?, 'pending')`, storeName)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LogSyncFinish marks a sync log entry as success or error.
func (db *DB) LogSyncFinish(logID int64, status, message string) error {
	_, err := db.Exec(`UPDATE sync_log SET status=?, message=?, finished_at=datetime('now') WHERE id=?`,
		status, message, logID)
	return err
}
