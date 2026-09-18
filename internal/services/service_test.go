package services

import (
	"fmt"
	"path/filepath"
	"testing"

	"gamelist/internal/api"
	"gamelist/internal/database"
	"gamelist/internal/models"
)

func openDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.NewSQLiteDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeStore implements api.StoreAPI without touching the network.
type fakeStore struct {
	key   string
	name  string
	games []models.Game
	err   error
}

func (f *fakeStore) Key() string  { return f.key }
func (f *fakeStore) Name() string { return f.name }
func (f *fakeStore) SignIn() error {
	return nil
}
func (f *fakeStore) GetOwnedGames() ([]models.Game, error) { return f.games, f.err }

func TestFetchAndEnrichMergesCrossStoreOwnership(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db} // IGDB not configured -> enrichment skipped

	steam := &fakeStore{
		key:  models.StoreSteam,
		name: "Steam",
		games: []models.Game{
			{Title: "Halo 3", OwnedStores: []string{models.StoreSteam},
				StoreIDs: map[string]string{models.StoreSteam: "1020"}},
			{Title: "Doom", OwnedStores: []string{models.StoreSteam},
				StoreIDs: map[string]string{models.StoreSteam: "620"}},
		},
	}
	epic := &fakeStore{
		key:  models.StoreEpic,
		name: "Epic Games Store",
		games: []models.Game{
			// Different casing + extra whitespace: must normalize to "halo 3"
			// and merge with the Steam copy.
			{Title: "  HALO   3 ", OwnedStores: []string{models.StoreEpic},
				StoreIDs: map[string]string{models.StoreEpic: "halo-3"}},
		},
	}

	games, err := svc.FetchAndEnrichGames([]api.StoreAPI{steam, epic})
	if err != nil {
		t.Fatalf("FetchAndEnrichGames: %v", err)
	}
	if len(games) != 2 {
		t.Fatalf("games = %d, want 2", len(games))
	}

	var halo *models.Game
	for i := range games {
		if games[i].ID == 0 && normalizeTitle(games[i].Title) == "halo 3" {
			halo = &games[i]
		}
	}
	if halo == nil {
		t.Fatalf("halo 3 not found in %+v", games)
	}
	if len(halo.OwnedStores) != 2 || halo.OwnedStores[0] != models.StoreEpic || halo.OwnedStores[1] != models.StoreSteam {
		t.Fatalf("owned stores = %v, want sorted [epic steam]", halo.OwnedStores)
	}
	if halo.StoreIDs[models.StoreSteam] != "1020" || halo.StoreIDs[models.StoreEpic] != "halo-3" {
		t.Fatalf("store ids = %v", halo.StoreIDs)
	}
}

func TestFetchAndEnrichFailsWhenAllStoresFail(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}

	a := &fakeStore{key: "a", name: "A", err: errFake("boom a")}
	b := &fakeStore{key: "b", name: "B", err: errFake("boom b")}

	if _, err := svc.FetchAndEnrichGames([]api.StoreAPI{a, b}); err == nil {
		t.Fatal("expected error when every store fails")
	}
}

func TestFetchAndEnrichToleratesPartialFailure(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}

	dead := &fakeStore{key: "dead", name: "Dead", err: errFake("offline")}
	live := &fakeStore{key: "live", name: "Live", games: []models.Game{
		{Title: "Doom", OwnedStores: []string{"live"}, StoreIDs: map[string]string{"live": "620"}},
	}}

	games, err := svc.FetchAndEnrichGames([]api.StoreAPI{dead, live})
	if err != nil {
		t.Fatalf("partial failure must not be a hard error: %v", err)
	}
	if len(games) != 1 || games[0].Title != "Doom" {
		t.Fatalf("games = %+v", games)
	}
}

func TestSaveGameAndListRoundTripUsesCanonicalStoreKeys(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}

	igdb := 620
	if err := svc.saveGame(&models.Game{
		ID:          igdb,
		Title:       "Doom",
		Description: "Rip and tear.",
		ReleaseDate: "2016-05-13",
		Developers:  []string{"id Software"},
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "620"},
	}); err != nil {
		t.Fatalf("saveGame: %v", err)
	}

	games, err := svc.ListGamesFromDB()
	if err != nil {
		t.Fatalf("ListGamesFromDB: %v", err)
	}
	if len(games) != 1 {
		t.Fatalf("games = %d, want 1", len(games))
	}
	g := games[0]
	if g.ID != igdb || g.Title != "Doom" || g.ReleaseDate != "2016-05-13" {
		t.Fatalf("game = %+v", g)
	}
	if len(g.Developers) != 1 || g.Developers[0] != "id Software" {
		t.Fatalf("developers = %v", g.Developers)
	}
	// Exactly one canonical store link; the old code also wrote a bogus
	// ("Steam", "Steam") row from display names.
	if len(g.OwnedStores) != 1 || g.OwnedStores[0] != models.StoreSteam {
		t.Fatalf("owned stores = %v", g.OwnedStores)
	}
	if g.StoreIDs[models.StoreSteam] != "620" {
		t.Fatalf("store ids = %v", g.StoreIDs)
	}
}

func TestListGamesFromDBEmpty(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}
	games, err := svc.ListGamesFromDB()
	if err != nil {
		t.Fatalf("ListGamesFromDB: %v", err)
	}
	if games == nil || len(games) != 0 {
		t.Fatalf("want non-nil empty slice, got %+#v", games)
	}
}

func TestMergeByIGDBIDCombinesTitleVariants(t *testing.T) {
	a := &models.Game{ID: 1020, Title: "Halo 3",
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "1020"}}
	b := &models.Game{ID: 1020, Title: "Halo 3 (Xbox 360)",
		OwnedStores: []string{models.StoreEpic},
		StoreIDs:    map[string]string{models.StoreEpic: "halo-3"}}

	out := mergeByIGDBID([]*models.Game{a, b})
	if len(out) != 1 {
		t.Fatalf("merged games = %d, want 1", len(out))
	}
	if len(out[0].OwnedStores) != 2 {
		t.Fatalf("ownership not combined: %v", out[0].OwnedStores)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  DOOM   (2016) ", "doom (2016)"},
		{"Halo\t3", "halo 3"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeTitle(tc.in); got != tc.want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSearchIGDBRequiresConfig(t *testing.T) {
	db := openDB(t)
	svc := NewGameService(db, "", "") // not configured
	if svc.IGDBConfigured() {
		t.Fatal("service should report IGDB as unconfigured")
	}
	if _, err := svc.SearchIGDB("doom"); err == nil {
		t.Fatal("expected helpful error when IGDB is not configured")
	}
}

func TestIGDBMatchedTitles(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}

	if err := svc.saveGame(&models.Game{ID: 620, Title: "Doom",
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "620"}}); err != nil {
		t.Fatalf("saveGame: %v", err)
	}
	if err := svc.saveGame(&models.Game{Title: "Unmatched Game",
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "999"}}); err != nil {
		t.Fatalf("saveGame 2: %v", err)
	}

	matched, err := svc.IGDBMatchedTitles()
	if err != nil {
		t.Fatalf("IGDBMatchedTitles: %v", err)
	}
	if got, ok := matched["doom"]; !ok || got != 620 {
		t.Fatalf(`matched["doom"] = %d, %v; want 620, true`, got, ok)
	}
	if _, ok := matched["unmatched game"]; ok {
		t.Fatal("unmatched game must not appear in the matched map")
	}
}

func TestEnrichAndSaveIncompleteSkipsAlreadyMatched(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db} // IGDB not configured: no network either way

	// A game matched in a previous sync lives in the database with an IGDB ID.
	if err := svc.saveGame(&models.Game{ID: 620, Title: "Doom",
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "620"}}); err != nil {
		t.Fatalf("saveGame: %v", err)
	}

	games := []models.Game{
		// Different casing/spacing: normalizes to "doom" and must be treated
		// as already finished in incomplete mode.
		{Title: "  DOOM  ", OwnedStores: []string{models.StoreSteam},
			StoreIDs: map[string]string{models.StoreSteam: "620"}},
		{Title: "Brand New Game", OwnedStores: []string{models.StoreSteam},
			StoreIDs: map[string]string{models.StoreSteam: "12345"}},
	}

	out, err := svc.EnrichAndSave(games, SyncModeIncomplete, nil, nil)
	if err != nil {
		t.Fatalf("EnrichAndSave: %v", err)
	}

	byTitle := map[string]models.Game{}
	for _, g := range out {
		byTitle[normalizeTitle(g.Title)] = g
	}
	if got, ok := byTitle["doom"]; !ok || got.ID != 620 {
		t.Fatalf(`"doom" ID = %d (present %v); want 620 carried over from the DB`, got.ID, ok)
	}
	if got, ok := byTitle["brand new game"]; !ok || got.ID != 0 {
		t.Fatalf(`"brand new game" ID = %d (present %v); want 0 (enrichment needed)`, got.ID, ok)
	}
}

func TestEnrichAndSaveRefreshRestartsEverything(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}

	if err := svc.saveGame(&models.Game{ID: 620, Title: "Doom",
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "620"}}); err != nil {
		t.Fatalf("saveGame: %v", err)
	}

	games := []models.Game{
		{Title: "Doom", OwnedStores: []string{models.StoreSteam},
			StoreIDs: map[string]string{models.StoreSteam: "620"}},
	}
	out, err := svc.EnrichAndSave(games, SyncModeRefresh, nil, nil)
	if err != nil {
		t.Fatalf("EnrichAndSave: %v", err)
	}
	// Refresh mode must NOT stamp the old ID in; enrichment (here: disabled)
	// would re-derive it. The upsert still lands on the same row via title.
	if len(out) != 1 {
		t.Fatalf("out = %d games, want 1", len(out))
	}
}

func TestEnrichAndSaveReportsProgressPerGame(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db}

	games := []models.Game{
		{Title: "Game B", OwnedStores: []string{models.StoreSteam},
			StoreIDs: map[string]string{models.StoreSteam: "2"}},
		{Title: "Game A", OwnedStores: []string{models.StoreSteam},
			StoreIDs: map[string]string{models.StoreSteam: "1"}},
	}

	var reports []string
	progress := func(done, total int, title, result string) {
		reports = append(reports, fmt.Sprintf("%d/%d:%s", done, total, title))
	}
	if _, err := svc.EnrichAndSave(games, SyncModeRefresh, progress, nil); err != nil {
		t.Fatalf("EnrichAndSave: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("progress calls = %d (%v), want 2", len(reports), reports)
	}
	// Titles are sorted before enrichment, so Game A is processed first.
	if reports[0] != "1/2:Game A" || reports[1] != "2/2:Game B" {
		t.Fatalf("reports = %v", reports)
	}
}

func TestEnrichAndSavePrunesStaleLinksAndOrphans(t *testing.T) {
	db := openDB(t)
	svc := &GameService{db: db} // IGDB not configured

	// A bogus entry from a previous bad sync, plus a legit Steam game.
	if err := svc.saveGame(&models.Game{Title: "Old Bogus",
		OwnedStores: []string{models.StoreEpic},
		StoreIDs:    map[string]string{models.StoreEpic: "bogus-app"}}); err != nil {
		t.Fatalf("saveGame bogus: %v", err)
	}
	if err := svc.saveGame(&models.Game{Title: "Doom",
		OwnedStores: []string{models.StoreSteam},
		StoreIDs:    map[string]string{models.StoreSteam: "620"}}); err != nil {
		t.Fatalf("saveGame doom: %v", err)
	}

	// New sync: Epic now reports one real game; Doom's store was not fetched,
	// so its links must be untouched.
	games := []models.Game{
		{Title: "The Escapists", OwnedStores: []string{models.StoreEpic},
			StoreIDs: map[string]string{models.StoreEpic: "Peony"}},
	}
	out, err := svc.EnrichAndSave(games, SyncModeRefresh, nil, []string{models.StoreEpic})
	if err != nil {
		t.Fatalf("EnrichAndSave: %v", err)
	}

	// Only the current Epic game remains (Doom's link survives, but Doom was
	// not fetched this round and cannot be IGDB-enriched here; it stays).
	titles := map[string]bool{}
	for _, g := range out {
		titles[g.Title] = true
	}
	if !titles["The Escapists"] || titles["Old Bogus"] {
		t.Fatalf("unexpected result set: %v", titles)
	}

	listed, err := svc.ListGamesFromDB()
	if err != nil {
		t.Fatalf("ListGamesFromDB: %v", err)
	}
	listedTitles := map[string]bool{}
	for _, g := range listed {
		listedTitles[g.Title] = true
	}
	if listedTitles["Old Bogus"] {
		t.Fatal("stale bogus game was not pruned from the database")
	}
	if !listedTitles["Doom"] {
		t.Fatal("Doom must survive an Epic-only prune")
	}
}

// errFake is a tiny error type to keep imports minimal.
type errFake string

func (e errFake) Error() string { return string(e) }
