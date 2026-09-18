package database

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countGames(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM games`).Scan(&n); err != nil {
		t.Fatalf("count games: %v", err)
	}
	return n
}

func TestMigrationsAreRecordedAndIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := db.runMigrations(); err != nil { // running twice must be safe
		t.Fatalf("second runMigrations: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected at least 2 recorded migrations, got %d", n)
	}
}

func TestUpsertGameDeduplicatesUnmatchedByTitle(t *testing.T) {
	db := openTestDB(t)

	id1, err := db.UpsertGame(&GameRecord{Title: "Doom"})
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	id2, err := db.UpsertGame(&GameRecord{Title: "DOOM"}) // same title, different case, no IGDB id
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("same title must map to one row: %d vs %d", id1, id2)
	}
	if n := countGames(t, db); n != 1 {
		t.Fatalf("games count = %d, want 1 (old code duplicated on every sync)", n)
	}
}

func TestUpsertGameUpdatesByIgdbID(t *testing.T) {
	db := openTestDB(t)
	igdb := 1020

	id1, err := db.UpsertGame(&GameRecord{Title: "Halo 3"})
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	id2, err := db.UpsertGame(&GameRecord{IgdbID: &igdb, Title: "Halo 3", Description: "Finish the fight."})
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same row, got %d vs %d", id1, id2)
	}
	var desc string
	if err := db.QueryRow(`SELECT description FROM games WHERE id = ?`, id1).Scan(&desc); err != nil {
		t.Fatalf("select: %v", err)
	}
	if desc != "Finish the fight." {
		t.Fatalf("description not updated: %q", desc)
	}
}

func TestUpsertGameAttachesIgdbIDToExistingTitleRow(t *testing.T) {
	db := openTestDB(t)

	// Game stored unmatched on an earlier sync...
	id1, err := db.UpsertGame(&GameRecord{Title: "Doom"})
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// ...then matched to IGDB later: must update, not insert.
	igdb := 1020
	id2, err := db.UpsertGame(&GameRecord{IgdbID: &igdb, Title: "Doom"})
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected existing title row to adopt the IGDB id, got %d vs %d", id1, id2)
	}
	var gotIgdb int
	if err := db.QueryRow(`SELECT igdb_id FROM games WHERE id = ?`, id1).Scan(&gotIgdb); err != nil {
		t.Fatalf("select: %v", err)
	}
	if gotIgdb != 1020 {
		t.Fatalf("igdb_id = %d, want 1020", gotIgdb)
	}
}

func TestUpsertGamePreservesFieldsWhenIncomingEmpty(t *testing.T) {
	db := openTestDB(t)

	// A fully enriched game from a previous sync...
	id, err := db.UpsertGame(&GameRecord{
		Title:          "Doom",
		Description:    "Rip and tear.",
		ReleaseDate:    "2016-05-13",
		DevelopersJSON: `["id Software"]`,
		PublishersJSON: `["Bethesda"]`,
		PlatformsJSON:  `["PC (Microsoft Windows)"]`,
	})
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	// ...re-saved with empty enrichment data (incomplete-mode skip) must not
	// wipe the stored IGDB data.
	if _, err := db.UpsertGame(&GameRecord{Title: "Doom"}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	var desc, rd, dev, pub, plat string
	if err := db.QueryRow(`SELECT description, release_date, developers, publishers, platforms FROM games WHERE id = ?`, id).
		Scan(&desc, &rd, &dev, &pub, &plat); err != nil {
		t.Fatalf("select: %v", err)
	}
	if desc != "Rip and tear." || rd != "2016-05-13" {
		t.Fatalf("scalars clobbered: desc=%q rd=%q", desc, rd)
	}
	if dev != `["id Software"]` || pub != `["Bethesda"]` || plat != `["PC (Microsoft Windows)"]` {
		t.Fatalf("lists clobbered: dev=%s pub=%s plat=%s", dev, pub, plat)
	}
}

func TestAddGameStoreLinkIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	id, err := db.UpsertGame(&GameRecord{Title: "Doom"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.AddGameStoreLink(id, "steam", "620"); err != nil {
		t.Fatalf("link 1: %v", err)
	}
	if err := db.AddGameStoreLink(id, "steam", "620"); err != nil {
		t.Fatalf("link 2: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM game_stores WHERE game_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("store links = %d, want 1", n)
	}
}

func TestStoreCredentialRoundTrip(t *testing.T) {
	db := openTestDB(t)

	if cred, _ := db.GetStoreCredential("steam"); cred != nil {
		t.Fatalf("expected no credential before signin, got %+v", cred)
	}

	meta := `{"api_key":"KEY","steam_id":"STEAMID"}`
	if err := db.SaveStoreCredentials("steam", "", "", 0, meta); err != nil {
		t.Fatalf("save: %v", err)
	}
	cred, err := db.GetStoreCredential("steam")
	if err != nil || cred == nil {
		t.Fatalf("get: %v, %+v", err, cred)
	}
	if cred.Meta != meta {
		t.Fatalf("meta = %q", cred.Meta)
	}

	// Upsert overwrites every field.
	if err := db.SaveStoreCredentials("steam", "tok", "", 123456, ""); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	cred, err = db.GetStoreCredential("steam")
	if err != nil || cred == nil {
		t.Fatalf("get 2: %v, %+v", err, cred)
	}
	if cred.AccessToken != "tok" || cred.ExpiresAt != 123456 || cred.Meta != "" {
		t.Fatalf("credential after overwrite: %+v", cred)
	}

	if err := db.ClearStoreCredentials("steam"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cred, _ := db.GetStoreCredential("steam"); cred != nil {
		t.Fatalf("credential still present after signout: %+v", cred)
	}
}

func TestSyncLogRoundTrip(t *testing.T) {
	db := openTestDB(t)
	logID, err := db.LogSyncStart("steam")
	if err != nil {
		t.Fatalf("log start: %v", err)
	}
	if err := db.LogSyncFinish(logID, "error", "steam returned 0 games"); err != nil {
		t.Fatalf("log finish: %v", err)
	}
	var status, msg string
	if err := db.QueryRow(`SELECT status, message FROM sync_log WHERE id = ?`, logID).Scan(&status, &msg); err != nil {
		t.Fatalf("select log: %v", err)
	}
	if status != "error" || msg != "steam returned 0 games" {
		t.Fatalf("log row: status=%q msg=%q", status, msg)
	}
}
