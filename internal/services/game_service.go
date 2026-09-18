package services

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"gamelist/internal/api"
	"gamelist/internal/database"
	"gamelist/internal/models"
)

// igdbCredStoreName caches the auto-managed Twitch token in store_credentials
// so restarts reuse it until it expires.
const igdbCredStoreName = "_igdb"

// SyncMode selects how much IGDB (re-)matching a sync performs.
type SyncMode int

const (
	// SyncModeIncomplete enriches only games that do not already carry an
	// IGDB match in the local database. Fast: previously matched games are
	// skipped entirely.
	SyncModeIncomplete SyncMode = iota
	// SyncModeRefresh re-checks every game against IGDB, overwriting
	// previously stored enrichment data.
	SyncModeRefresh
)

// SyncProgress receives one call per processed game during enrichment
// (done counts from 1). The CLI uses it to print per-game progress.
type SyncProgress func(done, total int, title, result string)

// GameService orchestrates store fetching, IGDB enrichment and persistence.
type GameService struct {
	db             *database.DB
	igdb           *api.IGDBClient
	igdbConfigured bool
}

// NewGameService wires the service. Pass empty clientID/clientSecret when
// IGDB is not configured; enrichment and search then degrade gracefully.
func NewGameService(db *database.DB, igdbClientID, igdbClientSecret string) *GameService {
	s := &GameService{db: db}
	if igdbClientID == "" || igdbClientSecret == "" {
		return s
	}

	ts := api.NewTokenSource(igdbClientID, igdbClientSecret)
	ts.Load = func() (string, time.Time, bool) {
		cred, err := db.GetStoreCredential(igdbCredStoreName)
		if err != nil || cred == nil || cred.AccessToken == "" {
			return "", time.Time{}, false
		}
		return cred.AccessToken, time.Unix(cred.ExpiresAt, 0), true
	}
	ts.Save = func(token string, exp time.Time) {
		if err := db.SaveStoreCredentials(igdbCredStoreName, token, "", exp.Unix(), ""); err != nil {
			log.Printf("Warning: could not cache IGDB token: %v", err)
		}
	}

	s.igdb = api.NewIGDBClient(ts)
	s.igdbConfigured = true
	return s
}

// IGDBConfigured reports whether IGDB enrichment/search can run.
func (s *GameService) IGDBConfigured() bool { return s.igdbConfigured }

// SearchIGDB searches IGDB by title (used by the `search` command).
func (s *GameService) SearchIGDB(query string) ([]models.Game, error) {
	if !s.igdbConfigured {
		return nil, fmt.Errorf("IGDB is not configured: set igdb.clientId and igdb.clientSecret in configs/config.yaml")
	}
	return s.igdb.SearchGames(query)
}

// ---------------------------------------------------------------------------
// Sync phases
// ---------------------------------------------------------------------------

type storeFetch struct {
	key   string
	name  string
	games []models.Game
	err   error
	logID int64
}

// FetchAndMergeGames pulls every store concurrently, records each store's
// outcome in sync_log, and merges cross-store ownership into one entry per
// normalized title. No IGDB calls, no persistence.
// Returns the merged games plus the keys of stores whose fetch SUCCEEDED
// (only those may be pruned later - a failed fetch means unknown, not
// unowned). An error is returned only when ALL stores failed.
func (s *GameService) FetchAndMergeGames(storeAPIs []api.StoreAPI) ([]models.Game, []string, error) {
	results := make([]storeFetch, len(storeAPIs))
	var wg sync.WaitGroup
	for i, st := range storeAPIs {
		wg.Add(1)
		go func(i int, st api.StoreAPI) {
			defer wg.Done()
			res := storeFetch{key: st.Key(), name: st.Name()}
			if s.db != nil {
				if logID, err := s.db.LogSyncStart(st.Key()); err == nil {
					res.logID = logID
				}
			}
			games, err := st.GetOwnedGames()
			res.games, res.err = games, err
			if s.db != nil && res.logID != 0 {
				status, msg := "success", fmt.Sprintf("%d games", len(games))
				if err != nil {
					status, msg = "error", err.Error()
				}
				_ = s.db.LogSyncFinish(res.logID, status, msg)
			}
			results[i] = res
		}(i, st)
	}
	wg.Wait()

	// Merge into one entry per normalized title.
	gameMap := make(map[string]*models.Game)
	var failures []string
	var fetchedStores []string
	for _, r := range results {
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.name, r.err))
			continue
		}
		fetchedStores = append(fetchedStores, r.key)
		for _, g := range r.games {
			key := normalizeTitle(g.Title)
			if key == "" {
				continue
			}
			existing, ok := gameMap[key]
			if !ok {
				clone := g
				gameMap[key] = &clone
				continue
			}
			mergeStoreOwnership(existing, &g)
		}
	}

	if len(failures) > 0 && len(failures) == len(storeAPIs) {
		return nil, nil, fmt.Errorf("all stores failed:\n  %s", strings.Join(failures, "\n  "))
	}
	for _, f := range failures {
		log.Printf("store error: %s", f)
	}

	out := make([]models.Game, 0, len(gameMap))
	for _, g := range gameMap {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, fetchedStores, nil
}

// IGDBMatchedTitles returns a map of normalized game titles that already
// carry an IGDB match in the local database (from earlier syncs) to their
// IGDB IDs. Used to detect "already finished" games before enrichment.
func (s *GameService) IGDBMatchedTitles() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT title, igdb_id FROM games WHERE igdb_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("query matched titles: %w", err)
	}
	defer rows.Close()

	matched := make(map[string]int)
	for rows.Next() {
		var title string
		var id int
		if err := rows.Scan(&title, &id); err != nil {
			return nil, fmt.Errorf("scan matched title: %w", err)
		}
		if key := normalizeTitle(title); key != "" {
			matched[key] = id
		}
	}
	return matched, rows.Err()
}

// EnrichAndSave is the second phase of a sync: enrich games with IGDB
// according to mode (reporting per-game progress), persist everything, and
// prune stale ownership links for the stores that were fetched. storeKeys
// must list exactly the stores whose fetch succeeded; pass nil to skip
// pruning entirely.
func (s *GameService) EnrichAndSave(games []models.Game, mode SyncMode, progress SyncProgress, storeKeys []string) ([]models.Game, error) {
	sort.Slice(games, func(i, j int) bool { return games[i].Title < games[j].Title })

	// Titles already matched in earlier syncs (used by incomplete mode).
	matched := map[string]int{}
	if mode == SyncModeIncomplete {
		m, err := s.IGDBMatchedTitles()
		if err != nil {
			return nil, fmt.Errorf("load previously matched titles: %w", err)
		}
		matched = m
	}

	merged := make([]*models.Game, 0, len(games))
	for i := range games {
		g := &games[i]

		// Incomplete mode: skip games that finished IGDB matching earlier.
		if mode == SyncModeIncomplete {
			if id, ok := matched[normalizeTitle(g.Title)]; ok && id > 0 {
				g.ID = id
				s.report(progress, i+1, len(games), g.Title,
					fmt.Sprintf("already matched (IGDB #%d) - skipped", id))
				merged = append(merged, g)
				continue
			}
		}

		if s.igdbConfigured {
			s.enrichViaIGDB(g)
		}
		result := "no IGDB match"
		if g.ID > 0 {
			result = fmt.Sprintf("matched IGDB #%d", g.ID)
		} else if !s.igdbConfigured {
			result = "IGDB not configured"
		}
		s.report(progress, i+1, len(games), g.Title, result)
		merged = append(merged, g)
	}

	// Distinct title variants can resolve to the same IGDB entry; merge those
	// so ownership from both lands on a single row.
	final := mergeByIGDBID(merged)

	out := make([]models.Game, 0, len(final))
	for _, g := range final {
		if err := s.saveGame(g); err != nil {
			log.Printf("Warning: could not save %q: %v", g.Title, err)
		}
		out = append(out, *g)
	}

	// Prune ownership links for successfully-fetched stores: a link whose
	// store_id is no longer in the fetched set is stale (e.g. a refund or a
	// previously-bad match). Games that lost their last link are removed.
	if s.db != nil {
		for _, key := range storeKeys {
			keep := make([]string, 0, len(out))
			seen := make(map[string]bool, len(out))
			for _, g := range out {
				if id, ok := g.StoreIDs[key]; ok {
					if !seen[id] {
						keep = append(keep, id)
						seen[id] = true
					}
					continue
				}
				if containsString(g.OwnedStores, key) && !seen[""] {
					keep = append(keep, "") // ownership-only link
					seen[""] = true
				}
			}
			if err := s.db.PruneStoreLinks(key, keep); err != nil {
				log.Printf("Warning: could not prune stale %s links: %v", key, err)
			}
		}
	}

	var nMatched, nUnmatched int
	for _, g := range merged {
		if g.ID > 0 {
			nMatched++
		} else {
			nUnmatched++
		}
	}
	log.Printf("Enrichment finished: %d game(s) matched with IGDB, %d without a match", nMatched, nUnmatched)
	return out, nil
}

func (s *GameService) report(progress SyncProgress, done, total int, title, result string) {
	if progress != nil {
		progress(done, total, title, result)
		return
	}
	log.Printf("[%d/%d] %s - %s", done, total, title, result)
}

// FetchAndEnrichGames is the single-call convenience API: fetch, merge,
// enrich everything (refresh mode) and save. The CLI uses the split phases
// so it can ask the user how much to refresh first.
func (s *GameService) FetchAndEnrichGames(storeAPIs []api.StoreAPI) ([]models.Game, error) {
	games, keys, err := s.FetchAndMergeGames(storeAPIs)
	if err != nil {
		return nil, err
	}
	return s.EnrichAndSave(games, SyncModeRefresh, nil, keys)
}

// ---------------------------------------------------------------------------
// enrichment + persistence
// ---------------------------------------------------------------------------

// enrichViaIGDB matches a game on IGDB, preferring an exact store-ID match
// (Steam appid via external_games) over fuzzy title search. No-ops when the
// game already carries an IGDB ID.
func (s *GameService) enrichViaIGDB(g *models.Game) {
	if s.igdb == nil || g.ID != 0 {
		return
	}

	if appID := g.StoreIDs[models.StoreSteam]; appID != "" {
		id, err := s.igdb.FindGameByExternalID(api.ExternalGameSourceSteam, appID)
		if err != nil {
			log.Printf("IGDB external-id match for %q (appid %s): %v", g.Title, appID, err)
		} else if id > 0 {
			s.applyIGDBDetails(g, id)
			return
		}
	}

	// GOG: IGDB indexes GOG product ids (external_game_source 5), so exact
	// matching works here too.
	if gogID := g.StoreIDs[models.StoreGOG]; gogID != "" {
		id, err := s.igdb.FindGameByExternalID(api.ExternalGameSourceGOG, gogID)
		if err != nil {
			log.Printf("IGDB external-id match for %q (gog id %s): %v", g.Title, gogID, err)
		} else if id > 0 {
			s.applyIGDBDetails(g, id)
			return
		}
	}

	results, err := s.igdb.SearchGames(g.Title)
	if err != nil {
		log.Printf("IGDB search for %q failed: %v", g.Title, err)
		return
	}
	if len(results) == 0 {
		return // genuinely unmatched; stored with store data only
	}
	s.applyIGDBDetails(g, results[0].ID)
}

func (s *GameService) applyIGDBDetails(g *models.Game, igdbID int) {
	details, err := s.igdb.GetGameDetails(igdbID)
	if err != nil {
		log.Printf("IGDB details for %q (IGDB ID %d): %v", g.Title, igdbID, err)
		g.ID = igdbID // keep the match even when the detail fetch fails
		return
	}
	g.ID = details.ID
	g.Description = details.Description
	g.ReleaseDate = details.ReleaseDate
	g.Developers = details.Developers
	g.Publishers = details.Publishers
	g.Platforms = details.Platforms
}

// saveGame persists one game plus its canonical store links. Empty enrichment
// fields are written as empty strings so the database keeps whatever a
// previous sync stored (upsert preserves them).
func (s *GameService) saveGame(g *models.Game) error {
	var igdbID *int
	if g.ID > 0 {
		igdbID = &g.ID
	}

	devJSON, err := marshalList(g.Developers)
	if err != nil {
		return fmt.Errorf("encode developers: %w", err)
	}
	pubJSON, err := marshalList(g.Publishers)
	if err != nil {
		return fmt.Errorf("encode publishers: %w", err)
	}
	platJSON, err := marshalList(g.Platforms)
	if err != nil {
		return fmt.Errorf("encode platforms: %w", err)
	}

	rowID, err := s.db.UpsertGame(&database.GameRecord{
		IgdbID:         igdbID,
		Title:          g.Title,
		Description:    g.Description,
		ReleaseDate:    g.ReleaseDate,
		DevelopersJSON: devJSON,
		PublishersJSON: pubJSON,
		PlatformsJSON:  platJSON,
	})
	if err != nil {
		return err
	}

	// Store links use canonical keys only (models.Store*); the old code also
	// wrote a second "Steam"/"Steam" row from display names.
	for storeKey, storeID := range g.StoreIDs {
		if err := s.db.AddGameStoreLink(rowID, storeKey, storeID); err != nil {
			log.Printf("Warning: could not link %q to %s: %v", g.Title, storeKey, err)
		}
	}
	for _, storeKey := range g.OwnedStores {
		if _, ok := g.StoreIDs[storeKey]; !ok {
			if err := s.db.AddGameStoreLink(rowID, storeKey, ""); err != nil {
				log.Printf("Warning: could not link %q to %s: %v", g.Title, storeKey, err)
			}
		}
	}
	return nil
}

// ListGamesFromDB loads every game with its store links (two queries total,
// no N+1) in a stable, title-sorted order.
func (s *GameService) ListGamesFromDB() ([]models.Game, error) {
	rows, err := s.db.Query(`SELECT id, igdb_id, title, description, release_date, developers, publishers, platforms
		FROM games ORDER BY lower(title)`)
	if err != nil {
		return nil, fmt.Errorf("query games: %w", err)
	}
	defer rows.Close()

	byRowID := map[int64]*models.Game{}
	var order []int64
	for rows.Next() {
		var rec database.GameRecord
		if err := rows.Scan(&rec.ID, &rec.IgdbID, &rec.Title, &rec.Description, &rec.ReleaseDate,
			&rec.DevelopersJSON, &rec.PublishersJSON, &rec.PlatformsJSON); err != nil {
			return nil, fmt.Errorf("scan game row: %w", err)
		}

		g := &models.Game{
			Title:       rec.Title,
			Description: rec.Description,
			ReleaseDate: rec.ReleaseDate,
			OwnedStores: []string{},
		}
		if rec.IgdbID != nil {
			g.ID = *rec.IgdbID
		}
		if err := decodeJSONStrings(rec.DevelopersJSON, &g.Developers); err != nil {
			return nil, fmt.Errorf("decode developers for %q: %w", rec.Title, err)
		}
		if err := decodeJSONStrings(rec.PublishersJSON, &g.Publishers); err != nil {
			return nil, fmt.Errorf("decode publishers for %q: %w", rec.Title, err)
		}
		if err := decodeJSONStrings(rec.PlatformsJSON, &g.Platforms); err != nil {
			return nil, fmt.Errorf("decode platforms for %q: %w", rec.Title, err)
		}
		byRowID[rec.ID] = g
		order = append(order, rec.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate games: %w", err)
	}

	linkRows, err := s.db.Query(`SELECT game_id, store_name, store_id FROM game_stores ORDER BY store_name`)
	if err != nil {
		return nil, fmt.Errorf("query store links: %w", err)
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var gameID int64
		var storeKey, storeID string
		if err := linkRows.Scan(&gameID, &storeKey, &storeID); err != nil {
			return nil, fmt.Errorf("scan store link: %w", err)
		}
		g, ok := byRowID[gameID]
		if !ok {
			continue
		}
		if g.StoreIDs == nil {
			g.StoreIDs = map[string]string{}
		}
		g.StoreIDs[storeKey] = storeID
		if !containsString(g.OwnedStores, storeKey) {
			g.OwnedStores = append(g.OwnedStores, storeKey)
		}
	}
	if err := linkRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate store links: %w", err)
	}

	games := make([]models.Game, 0, len(order))
	for _, id := range order {
		games = append(games, *byRowID[id])
	}
	return games, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mergeStoreOwnership unions store keys and store IDs from src into dst.
func mergeStoreOwnership(dst, src *models.Game) {
	if dst.OwnedStores == nil {
		dst.OwnedStores = []string{}
	}
	for _, st := range src.OwnedStores {
		if !containsString(dst.OwnedStores, st) {
			dst.OwnedStores = append(dst.OwnedStores, st)
		}
	}
	sort.Strings(dst.OwnedStores)
	if dst.StoreIDs == nil {
		dst.StoreIDs = map[string]string{}
	}
	for k, v := range src.StoreIDs {
		dst.StoreIDs[k] = v
	}
}

// mergeByIGDBID collapses games whose titles differ but that resolved to the
// same IGDB entry, combining their store ownership.
func mergeByIGDBID(games []*models.Game) []*models.Game {
	byID := map[int]*models.Game{}
	out := make([]*models.Game, 0, len(games))
	for _, g := range games {
		if g.ID <= 0 {
			out = append(out, g)
			continue
		}
		if prev, ok := byID[g.ID]; ok {
			mergeStoreOwnership(prev, g)
			continue
		}
		byID[g.ID] = g
		out = append(out, g)
	}
	return out
}

func decodeJSONStrings(raw string, dst *[]string) error {
	if raw == "" || raw == "null" {
		return nil
	}
	var v []string
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return err
	}
	*dst = v
	return nil
}

// marshalList encodes a string list; empty lists become "" so the upsert
// keeps whatever the database already stores.
func marshalList(list []string) (string, error) {
	if len(list) == 0 {
		return "", nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// normalizeTitle lowercases and collapses whitespace for cross-store
// matching, and strips trademark symbols that Ubisoft-style names carry
// ("Far Cry® 3" matches "Far Cry 3").
func normalizeTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '®', '™', '©':
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// NormalizeTitle is the exported form of the cross-store title matcher, used
// by the CLI to interpret IGDBMatchedTitles results.
func NormalizeTitle(s string) string {
	return normalizeTitle(s)
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
