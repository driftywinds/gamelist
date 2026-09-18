// Package server exposes the gamelist services as an HTTP JSON API so a
// frontend can drive every CLI action headlessly. It is meant for LOCAL use
// only: the API has no authentication, can read and write your config file,
// and returns sync results inline. Do not expose it to a network.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gamelist/configs"
	"gamelist/internal/api"
	"gamelist/internal/database"
	"gamelist/internal/models"
	"gamelist/internal/services"
)

// syncLockTimeout bounds how long a second sync request waits for a running
// one before giving up with 409 Conflict.
const syncLockTimeout = 30 * time.Second

// Server serves the HTTP JSON API.
type Server struct {
	cfgPath string
	cfg     *configs.Config
	db      *database.DB

	// syncMu guards syncRunning; syncs are exclusive (SQLite writes plus
	// IGDB rate limiting should not interleave).
	syncMu      sync.Mutex
	syncRunning bool
}

// New creates a server. cfgPath is needed because config writes go through
// configs.SetYAMLValues(configPath, ...), like `config set` in the CLI.
func New(cfgPath string, cfg *configs.Config, db *database.DB) *Server {
	return &Server{cfgPath: cfgPath, cfg: cfg, db: db}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/stores", s.handleStores)
	mux.HandleFunc("POST /api/stores/{store}/signin", s.handleSignIn)
	mux.HandleFunc("DELETE /api/stores/{store}/session", s.handleSignOut)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("GET /api/sync/log", s.handleSyncLog)
	mux.HandleFunc("GET /api/games", s.handleGames)
	mux.HandleFunc("GET /api/multi", s.handleMulti)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/config", s.handleConfigShow)
	mux.HandleFunc("PUT /api/config/{key}", s.handleConfigSet)
	return mux
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// resolveStoreArg maps a user-supplied store name (canonical key or display
// name, case-insensitive) to its canonical key.
func resolveStoreArg(arg string) (string, bool) {
	for _, key := range models.AllStoreKeys() {
		if key == strings.ToLower(strings.TrimSpace(arg)) || strings.EqualFold(models.DisplayStoreName(key), arg) {
			return key, true
		}
	}
	return "", false
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	s.writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// decodeJSONBody decodes an optional JSON body; an empty body is not an error.
func decodeJSONBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// storeEnabled mirrors the CLI's buildStoreAPIFromConfig nil-check.
func (s *Server) storeEnabled(key string) bool {
	return s.buildStoreAPI(key) != nil
}

// buildStoreAPI mirrors cmd/main.go's buildStoreAPIFromConfig: config.yaml is
// always the credential source of truth.
func (s *Server) buildStoreAPI(key string) api.StoreAPI {
	switch key {
	case models.StoreSteam:
		v := s.cfg.Stores.Steam
		if !v.Enabled {
			return nil
		}
		return api.NewSteamAPI(v.APIKey, v.SteamID)
	case models.StoreEpic:
		v := s.cfg.Stores.Epic
		if !v.Enabled {
			return nil
		}
		return api.NewEpicGamesAPI(v.ClientID, v.ClientSecret, nil)
	case models.StoreGOG:
		v := s.cfg.Stores.Gog
		if !v.Enabled {
			return nil
		}
		return api.NewGOGAPI(v.ClientID, v.ClientSecret, nil)
	case models.StoreUbisoft:
		v := s.cfg.Stores.Ubisoft
		if !v.Enabled {
			return nil
		}
		return api.NewUbisoftAPI(v.DataPath)
	case models.StoreXbox:
		if !s.cfg.Stores.Xbox.Enabled {
			return nil
		}
		return api.NewXboxAPI()
	case models.StoreBattleNet:
		v := s.cfg.Stores.Battlenet
		if !v.Enabled {
			return nil
		}
		return api.NewBattleNetAPI(v.ClientID, v.ClientSecret)
	case models.StoreDLsite:
		if !s.cfg.Stores.Dlsite.Enabled {
			return nil
		}
		return api.NewDLsiteAPI()
	default:
		return nil
	}
}

// interactiveSignInStores are the stores whose OAuth flows require a browser
// and stdin in the CLI (device-code login / paste-the-redirect-URL). The API
// refuses them with 422 instead of hanging on stdin.
var interactiveSignInStores = []string{models.StoreEpic, models.StoreGOG}

// ---------------------------------------------------------------------------
// GET /api/status  (the `status` command)
// ---------------------------------------------------------------------------

// CredSummary is credential presence without leaking secrets.
type CredSummary struct {
	HasToken   bool  `json:"has_token"`
	HasRefresh bool  `json:"has_refresh_token"`
	ExpiresAt  int64 `json:"expires_at"` // unix seconds, 0 = not applicable
}

// StoreStatus is one store's row of the status view.
type StoreStatus struct {
	Store    string       `json:"store"` // canonical key
	Display  string       `json:"display_name"`
	Enabled  bool         `json:"enabled"`
	SignedIn bool         `json:"signed_in"`
	Cred     *CredSummary `json:"credentials,omitempty"`
}

// StatusPayload is the GET /api/status response.
type StatusPayload struct {
	IGDBConfigured bool          `json:"igdb_configured"`
	Stores         []StoreStatus `json:"stores"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stores := make([]StoreStatus, 0, len(models.AllStoreKeys()))
	for _, key := range models.AllStoreKeys() {
		stores = append(stores, s.storeStatus(key))
	}
	s.writeJSON(w, http.StatusOK, StatusPayload{
		IGDBConfigured: s.cfg.Igdb.ClientID != "" && s.cfg.Igdb.ClientSecret != "",
		Stores:         stores,
	})
}

func (s *Server) storeStatus(key string) StoreStatus {
	ss := StoreStatus{Store: key, Display: models.DisplayStoreName(key), Enabled: s.storeEnabled(key)}
	if cred, err := s.db.GetStoreCredential(key); err == nil && cred != nil {
		ss.SignedIn = true
		ss.Cred = &CredSummary{
			HasToken:   cred.AccessToken != "",
			HasRefresh: cred.RefreshToken != "",
			ExpiresAt:  cred.ExpiresAt,
		}
	}
	return ss
}

// ---------------------------------------------------------------------------
// GET /api/stores
// ---------------------------------------------------------------------------

// handleStores lists the canonical store keys and display names - the
// vocabulary every other endpoint accepts.
func (s *Server) handleStores(w http.ResponseWriter, r *http.Request) {
	keys := models.AllStoreKeys()
	out := make([]StoreStatus, 0, len(keys))
	for _, key := range keys {
		out = append(out, s.storeStatus(key))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /api/stores/{store}/signin  (the `signin <store>` command)
// ---------------------------------------------------------------------------

// handleSignIn validates the store's credentials from config.yaml and stores
// the sign-in record, exactly like `gamelist signin <store>`. Steam,
// Battle.net and Ubisoft validate headlessly; epic and gog require the
// interactive browser flow and are refused with 422 (sign in once via the
// CLI; the stored session then refreshes itself on every API sync).
func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	store := r.PathValue("store")
	key, ok := resolveStoreArg(store)
	if !ok {
		s.writeErr(w, http.StatusBadRequest, "unknown store %q - valid stores: %s", store, strings.Join(models.AllStoreKeys(), ", "))
		return
	}
	if slices.Contains(interactiveSignInStores, key) {
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": models.DisplayStoreName(key) + " uses an interactive browser sign-in that cannot run headlessly",
			"hint":  "run 'gamelist config signin " + key + "' in the CLI once; the stored session refreshes automatically on later API syncs",
		})
		return
	}
	apiC := s.buildStoreAPI(key)
	if apiC == nil {
		s.writeErr(w, http.StatusConflict, "store %s is not enabled - set stores.%s.enabled=true (and its credentials) in configs/config.yaml first", key, key)
		return
	}
	if err := apiC.SignIn(); err != nil {
		if errors.Is(err, api.ErrNotImplemented) {
			s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": err.Error(),
				"hint":  "this store has no viable library API yet",
			})
			return
		}
		s.writeErr(w, http.StatusBadGateway, "sign-in validation failed: %v", err)
		return
	}
	if err := services.NewAuthService(s.db).Persist(apiC); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "credentials validated, but persisting failed: %v", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"store":     key,
		"signed_in": true,
		"note":      "credentials validated and stored",
	})
}

// ---------------------------------------------------------------------------
// DELETE /api/stores/{store}/session  (the `signout <store>` command)
// ---------------------------------------------------------------------------

// handleSignOut clears stored credentials for a store.
func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	store := r.PathValue("store")
	key, ok := resolveStoreArg(store)
	if !ok {
		s.writeErr(w, http.StatusBadRequest, "unknown store %q - valid stores: %s", store, strings.Join(models.AllStoreKeys(), ", "))
		return
	}
	if err := services.NewAuthService(s.db).SignOut(key); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "sign out of %s: %v", key, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"store":     key,
		"signed_in": false,
		"note":      "stored credentials cleared",
	})
}

// ---------------------------------------------------------------------------
// POST /api/sync  (the `sync [store] [--refresh|--incomplete]` command)
// ---------------------------------------------------------------------------

// StoreSyncOutcome is one store's part of a sync response. Failed stores are
// recorded with their error - never silently.
type StoreSyncOutcome struct {
	Store   string `json:"store"`
	Success bool   `json:"success"`
	Games   int    `json:"games,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SyncResult is the POST /api/sync response. Games carries the merged,
// enriched games exactly as `gamelist list` would print them after the sync.
type SyncResult struct {
	TotalGames int                `json:"total_games"`
	Games      []models.Game      `json:"games"`
	Stores     []StoreSyncOutcome `json:"stores"`
	Skipped    int                `json:"skipped_games"`
}

// syncArgs is the optional JSON body of POST /api/sync.
type syncArgs struct {
	// Store limits the sync to one store (canonical key or display name).
	Store string `json:"store,omitempty"`
	// Mode is "incomplete" (default: enrich only games not yet matched) or
	// "refresh" (re-check everything against IGDB).
	Mode string `json:"mode,omitempty"`
}

// handleSync runs one full sync: fetch owned games from every signed-in
// store, merge, enrich via IGDB and persist. It is the headless equivalent
// of `gamelist sync` with the interactive mode prompt resolved by the mode
// argument (default: incomplete). One sync runs at a time; a concurrent
// request waits up to 30s and then answers 409.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var args syncArgs
	if err := decodeJSONBody(r, &args); err != nil {
		s.writeErr(w, http.StatusBadRequest, "decode request body (expected {\"store\": \"...\", \"mode\": \"incomplete|refresh\"}): %v", err)
		return
	}

	target := ""
	if args.Store != "" {
		key, ok := resolveStoreArg(args.Store)
		if !ok {
			s.writeErr(w, http.StatusBadRequest, "unknown store %q - valid stores: %s", args.Store, strings.Join(models.AllStoreKeys(), ", "))
			return
		}
		target = key
	}
	mode := services.SyncModeIncomplete
	switch strings.ToLower(strings.TrimSpace(args.Mode)) {
	case "", "incomplete":
		mode = services.SyncModeIncomplete
	case "refresh":
		mode = services.SyncModeRefresh
	default:
		s.writeErr(w, http.StatusBadRequest, "unknown mode %q - valid modes: incomplete, refresh", args.Mode)
		return
	}

	if !s.tryLockSync() {
		s.writeErr(w, http.StatusConflict, "a sync is already running; try again when it finishes")
		return
	}
	defer s.unlockSync()

	// Build the store list like the CLI: config is the credential source of
	// truth; stores without a sign-in record are validated transparently.
	// Stores that cannot even sign in become explicit failed outcomes.
	auth := services.NewAuthService(s.db)
	var apis []api.StoreAPI
	outcomes := make([]StoreSyncOutcome, 0)
	for _, key := range models.AllStoreKeys() {
		if target != "" && key != target {
			continue
		}
		a := s.buildStoreAPI(key)
		if a == nil {
			continue
		}
		if cred, err := s.db.GetStoreCredential(key); err == nil && cred != nil {
			apis = append(apis, a)
			continue
		}
		if err := auth.SignIn(a); err != nil {
			outcomes = append(outcomes, StoreSyncOutcome{Store: key, Success: false, Error: fmt.Sprintf("auto sign-in failed: %v", err)})
			continue
		}
		apis = append(apis, a)
	}

	if len(apis) == 0 {
		res := SyncResult{Stores: outcomes}
		if res.Stores == nil {
			res.Stores = []StoreSyncOutcome{}
		}
		status := http.StatusConflict // nothing enabled / signed in
		if len(outcomes) > 0 {
			status = http.StatusBadGateway // everything failed to sign in
		}
		s.writeJSON(w, status, res)
		return
	}

	svc := services.NewGameService(s.db, s.cfg.Igdb.ClientID, s.cfg.Igdb.ClientSecret)
	games, fetchedStores, err := svc.FetchAndMergeGames(apis)

	// Per-store fetch results are in sync_log (newest rows are this sync's,
	// since syncs are serialized). Merge them with the sign-in outcomes.
	logByStore := map[string]database.SyncLogEntry{}
	if entries, qerr := s.db.QuerySyncLog(len(apis) + len(outcomes)); qerr == nil {
		for _, e := range entries { // newest first
			if _, seen := logByStore[e.StoreName]; !seen {
				logByStore[e.StoreName] = e
			}
		}
	}
	for _, key := range fetchedStores {
		if e, ok := logByStore[key]; ok {
			o := StoreSyncOutcome{Store: key, Success: e.Status == "success", Games: gamesFromLogMessage(e.Message)}
			if e.Status != "success" {
				o.Error = e.Message
			}
			outcomes = append(outcomes, o)
		}
	}

	if err != nil {
		// Every store failed; report the per-store errors, not just a summary.
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  fmt.Sprintf("sync failed: %v", err),
			"stores": outcomes,
		})
		return
	}

	// skipped = games that incomplete mode will not re-enrich (same
	// computation the CLI uses for its complete-vs-refresh prompt).
	skipped := 0
	if mode == services.SyncModeIncomplete {
		if matched, merr := svc.IGDBMatchedTitles(); merr == nil {
			for _, g := range games {
				if id, ok := matched[services.NormalizeTitle(g.Title)]; ok && id > 0 {
					skipped++
				}
			}
		}
	}

	final, err := svc.EnrichAndSave(games, mode, nil, fetchedStores)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "enrichment failed: %v", err)
		return
	}
	if final == nil {
		final = []models.Game{}
	}

	s.writeJSON(w, http.StatusOK, SyncResult{
		TotalGames: len(final),
		Games:      final,
		Stores:     outcomes,
		Skipped:    skipped,
	})
}

func gamesFromLogMessage(msg string) int {
	n := 0
	if _, err := fmt.Sscanf(msg, "%d games", &n); err == nil {
		return n
	}
	return 0
}

// tryLockSync acquires the exclusive sync slot, waiting up to
// syncLockTimeout for a running sync to finish.
func (s *Server) tryLockSync() bool {
	deadline := time.Now().Add(syncLockTimeout)
	for {
		s.syncMu.Lock()
		if !s.syncRunning {
			s.syncRunning = true
			s.syncMu.Unlock()
			return true
		}
		s.syncMu.Unlock()
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Server) unlockSync() {
	s.syncMu.Lock()
	s.syncRunning = false
	s.syncMu.Unlock()
}

// ---------------------------------------------------------------------------
// GET /api/sync/log
// ---------------------------------------------------------------------------

// SyncLogJSON is one row of GET /api/sync/log.
type SyncLogJSON struct {
	ID         int64  `json:"id"`
	Store      string `json:"store"`
	Status     string `json:"status"` // pending | success | error
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// handleSyncLog returns recent per-store sync history (newest first) - the
// same rows the CLI writes to the sync_log table.
func (s *Server) handleSyncLog(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.db.QuerySyncLog(limit)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "query sync log: %v", err)
		return
	}
	out := make([]SyncLogJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, SyncLogJSON{
			ID:         e.ID,
			Store:      e.StoreName,
			Status:     e.Status,
			Message:    e.Message,
			StartedAt:  e.StartedAt,
			FinishedAt: e.FinishedAt,
		})
	}
	s.writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /api/games  (the `list` command)
// ---------------------------------------------------------------------------

// handleGames returns every stored game (full models.Game shape) sorted by
// title, with ownership merged across stores. The optional ?store= filter
// keeps only games owned on that store.
func (s *Server) handleGames(w http.ResponseWriter, r *http.Request) {
	svc := services.NewGameService(s.db, "", "")
	games, err := svc.ListGamesFromDB()
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "list games: %v", err)
		return
	}
	if raw := r.URL.Query().Get("store"); raw != "" {
		key, ok := resolveStoreArg(raw)
		if !ok {
			s.writeErr(w, http.StatusBadRequest, "unknown store %q - valid stores: %s", raw, strings.Join(models.AllStoreKeys(), ", "))
			return
		}
		filtered := make([]models.Game, 0, len(games))
		for _, g := range games {
			if slices.Contains(g.OwnedStores, key) {
				filtered = append(filtered, g)
			}
		}
		games = filtered
	}
	if games == nil {
		games = []models.Game{}
	}
	s.writeJSON(w, http.StatusOK, games)
}

// ---------------------------------------------------------------------------
// GET /api/multi  (the `multi [--json] [store ...]` command)
// ---------------------------------------------------------------------------

// handleMulti returns games owned on more than one store. The optional
// ?stores=steam,epic filter restricts the result to games owned on ALL of
// the listed stores - exactly the CLI's `multi steam epic` semantics.
func (s *Server) handleMulti(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("stores"))
	var filter []string
	if raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, ok := resolveStoreArg(part)
			if !ok {
				s.writeErr(w, http.StatusBadRequest, "unknown store %q - valid stores: %s", part, strings.Join(models.AllStoreKeys(), ", "))
				return
			}
			if !slices.Contains(filter, key) {
				filter = append(filter, key)
			}
		}
	}

	svc := services.NewGameService(s.db, "", "")
	games, err := svc.ListGamesFromDB()
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "list games: %v", err)
		return
	}

	multi := make([]models.Game, 0)
	for _, g := range games {
		if len(g.OwnedStores) <= 1 {
			continue
		}
		all := true
		for _, key := range filter {
			if !slices.Contains(g.OwnedStores, key) {
				all = false
				break
			}
		}
		if all {
			multi = append(multi, g)
		}
	}
	s.writeJSON(w, http.StatusOK, multi)
}

// ---------------------------------------------------------------------------
// GET /api/search  (the `search <title>` command)
// ---------------------------------------------------------------------------

// handleSearch queries IGDB by title. Requires IGDB credentials in
// config.yaml (503 otherwise); upstream failures answer 502.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		s.writeErr(w, http.StatusBadRequest, "missing query parameter q")
		return
	}
	if s.cfg.Igdb.ClientID == "" || s.cfg.Igdb.ClientSecret == "" {
		s.writeErr(w, http.StatusServiceUnavailable, "IGDB is not configured: set igdb.clientId and igdb.clientSecret in configs/config.yaml")
		return
	}
	svc := services.NewGameService(s.db, s.cfg.Igdb.ClientID, s.cfg.Igdb.ClientSecret)
	results, err := svc.SearchIGDB(query)
	if err != nil {
		s.writeErr(w, http.StatusBadGateway, "search failed: %v", err)
		return
	}
	if results == nil {
		results = []models.Game{}
	}
	s.writeJSON(w, http.StatusOK, results)
}

// ---------------------------------------------------------------------------
// GET /api/config  (the `config show [--reveal]` command)
// ---------------------------------------------------------------------------

// ConfigValue is one row of GET /api/config.
type ConfigValue struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"` // bool | string
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

// handleConfigShow lists every editable config key with its current value.
// Secret values are masked unless ?reveal=true (the caller is local anyway,
// but this keeps response shapes identical to `config show`).
func (s *Server) handleConfigShow(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "true"
	out := make([]ConfigValue, 0)
	for _, ek := range configs.EditableKeys() {
		v := s.configValue(ek.Key)
		row := ConfigValue{Key: ek.Key, Kind: ek.Kind, Value: v, Secret: isSecretConfigKey(ek.Key)}
		if row.Secret && !reveal {
			row.Value = maskSecretValue(v)
		}
		if row.Value == "" {
			row.Value = "(not set)"
		}
		out = append(out, row)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// configValue mirrors cmd/main.go's configValue.
func (s *Server) configValue(key string) string {
	switch key {
	case "igdb.clientId":
		return s.cfg.Igdb.ClientID
	case "igdb.clientSecret":
		return s.cfg.Igdb.ClientSecret
	case "database.dataSourceName":
		return s.cfg.Database.DataSourceName
	case "stores.steam.enabled":
		return strconv.FormatBool(s.cfg.Stores.Steam.Enabled)
	case "stores.steam.apiKey":
		return s.cfg.Stores.Steam.APIKey
	case "stores.steam.steamId":
		return s.cfg.Stores.Steam.SteamID
	case "stores.epic.enabled":
		return strconv.FormatBool(s.cfg.Stores.Epic.Enabled)
	case "stores.epic.clientId":
		return s.cfg.Stores.Epic.ClientID
	case "stores.epic.clientSecret":
		return s.cfg.Stores.Epic.ClientSecret
	case "stores.gog.enabled":
		return strconv.FormatBool(s.cfg.Stores.Gog.Enabled)
	case "stores.gog.clientId":
		return s.cfg.Stores.Gog.ClientID
	case "stores.gog.clientSecret":
		return s.cfg.Stores.Gog.ClientSecret
	case "stores.ubisoft.enabled":
		return strconv.FormatBool(s.cfg.Stores.Ubisoft.Enabled)
	case "stores.ubisoft.dataPath":
		return s.cfg.Stores.Ubisoft.DataPath
	case "stores.xbox.enabled":
		return strconv.FormatBool(s.cfg.Stores.Xbox.Enabled)
	case "stores.battlenet.enabled":
		return strconv.FormatBool(s.cfg.Stores.Battlenet.Enabled)
	case "stores.battlenet.clientId":
		return s.cfg.Stores.Battlenet.ClientID
	case "stores.battlenet.clientSecret":
		return s.cfg.Stores.Battlenet.ClientSecret
	case "stores.dlsite.enabled":
		return strconv.FormatBool(s.cfg.Stores.Dlsite.Enabled)
	}
	return ""
}

func isSecretConfigKey(key string) bool {
	switch key {
	case "igdb.clientSecret", "stores.steam.apiKey", "stores.epic.clientSecret",
		"stores.gog.clientSecret", "stores.battlenet.clientSecret":
		return true
	}
	return false
}

func maskSecretValue(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "********"
	}
	return v[:4] + "..." + v[len(v)-4:]
}

// ---------------------------------------------------------------------------
// PUT /api/config/{key}  (the `config set <key> <value>` command)
// ---------------------------------------------------------------------------

// configSetArgs is the body of PUT /api/config/{key}.
type configSetArgs struct {
	Value string `json:"value"`
}

// handleConfigSet writes one value to configs/config.yaml (comment- and
// order-preserving, like `config set`) and reloads the in-memory config so
// later requests use it immediately. It deliberately refuses
// database.dataSourceName to avoid invalidating the already-open DB handle.
func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	kind := configs.LookupKeyKind(key)
	if kind == "" {
		valid := make([]string, 0, 32)
		for _, ek := range configs.EditableKeys() {
			valid = append(valid, ek.Key)
		}
		s.writeErr(w, http.StatusBadRequest, "unknown config key %q - valid keys: %s", key, strings.Join(valid, ", "))
		return
	}
	if key == "database.dataSourceName" {
		s.writeErr(w, http.StatusBadRequest, "database.dataSourceName cannot be changed while the server is running (restart with a new --config instead)")
		return
	}
	var args configSetArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		s.writeErr(w, http.StatusBadRequest, "decode request body (expected {\"value\": \"...\"}): %v", err)
		return
	}
	value := args.Value
	if kind == "bool" {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "yes", "on":
			value = "true"
		case "false", "0", "no", "off":
			value = "false"
		default:
			s.writeErr(w, http.StatusBadRequest, "%q needs a boolean value (true/false, yes/no, 1/0, on/off)", key)
			return
		}
	}
	if err := configs.SetYAMLValues(s.cfgPath, map[string]string{key: value}); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "write config: %v", err)
		return
	}
	if err := s.reloadConfig(); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "config written, but reloading failed: %v", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"key":   key,
		"value": value,
		"note":  "config written and reloaded; takes effect immediately",
	})
}

// reloadConfig re-reads config.yaml into the server's in-memory config.
func (s *Server) reloadConfig() error {
	cfg, err := configs.LoadConfig(s.cfgPath)
	if err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}
