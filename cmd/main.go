package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gamelist/configs"
	"gamelist/internal/api"
	"gamelist/internal/database"
	"gamelist/internal/models"
	"gamelist/internal/services"
)

func main() {
	args := os.Args[1:]
	configPath, args := extractConfigFlag(args)

	// Resolve the path HERE so the database location and `config set`/`config
	// signin` writes can rely on it.
	configPath = resolveConfigPath(configPath)

	// configs/config.yaml is gitignored (it holds personal credentials), so a
	// fresh clone has none: generate the documented default on first run.
	if created, err := configs.EnsureExists(configPath); err == nil && created {
		log.Printf("Created default config at %s - set it up with: gamelist config signin steam", configPath)
	}

	cfg, err := configs.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	// Keep the database next to the config so the app behaves the same
	// no matter which directory it is launched from.
	dsn := cfg.Database.DataSourceName
	if !filepath.IsAbs(dsn) {
		dsn = filepath.Join(filepath.Dir(configPath), dsn)
	}

	db, err := database.NewSQLiteDB(dsn)
	if err != nil {
		log.Fatalf("Database error: %v", err)
	}
	defer db.Close()

	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "signin":
		cmdSignIn(cfg, db, args[1:])
	case "signout":
		cmdSignOut(db, args[1:])
	case "sync":
		cmdSync(cfg, db, args[1:])
	case "list":
		cmdList(db)
	case "multi":
		cmdMulti(db, args[1:])
	case "search":
		cmdSearch(cfg, db, args[1:])
	case "status":
		cmdStatus(cfg, db)
	case "config":
		cmdConfig(configPath, cfg, db, args[1:])
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// helpers: config location
// ---------------------------------------------------------------------------

// extractConfigFlag pulls --config <path> / --config=<path> out of the args.
func extractConfigFlag(args []string) (string, []string) {
	configPath := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config" && i+1 < len(args):
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			configPath = strings.TrimPrefix(args[i], "--config=")
		default:
			rest = append(rest, args[i])
		}
	}
	return configPath, rest
}

// resolveConfigPath picks the config file: an explicit --config path wins,
// otherwise prefer configs/config.yaml next to the executable, then the
// working directory (the go-run / development case).
func resolveConfigPath(configPath string) string {
	if configPath != "" {
		return configPath
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "configs", "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join("configs", "config.yaml")
}

// ---------------------------------------------------------------------------
// signin / signout
// ---------------------------------------------------------------------------

func cmdSignIn(cfg *configs.Config, db *database.DB, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: gamelist signin <store>")
		fmt.Println("Stores: " + strings.Join(models.AllStoreKeys(), ", "))
		fmt.Println("Tip: 'gamelist config signin <store>' walks you through it interactively.")
		os.Exit(1)
	}
	storeName := args[0]

	storeAPI := buildStoreAPIFromConfig(cfg, db, storeName)
	if storeAPI == nil {
		log.Fatalf("Unknown or disabled store %q - configure it first with: gamelist config signin %s", storeName, storeName)
	}

	svc := services.NewAuthService(db)
	if err := svc.SignIn(storeAPI); err != nil {
		log.Printf("Sign-in failed: %v", err)
		fmt.Fprintln(os.Stderr, "Tip: run 'gamelist config signin "+storeName+"' to set up credentials interactively.")
		os.Exit(1)
	}
	fmt.Printf("Signed in to %s. Credentials stored in the local database.\n", storeAPI.Name())
}

func cmdSignOut(db *database.DB, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: gamelist signout <store>")
		os.Exit(1)
	}
	svc := services.NewAuthService(db)
	if err := svc.SignOut(args[0]); err != nil {
		log.Fatalf("Sign-out failed: %v", err)
	}
	fmt.Printf("Cleared stored credentials for %s.\n", args[0])
}

// ---------------------------------------------------------------------------
// sync
// ---------------------------------------------------------------------------

func cmdSync(cfg *configs.Config, db *database.DB, args []string) {
	target := ""
	flagRefresh, flagIncomplete := false, false
	for _, a := range args {
		switch a {
		case "--refresh":
			flagRefresh = true
		case "--incomplete":
			flagIncomplete = true
		default:
			if target != "" {
				fmt.Printf("Unexpected argument %q (usage: sync [--refresh|--incomplete] [store])\n", a)
				os.Exit(1)
			}
			target = a
		}
	}
	if flagRefresh && flagIncomplete {
		fmt.Println("Use either --refresh or --incomplete, not both.")
		os.Exit(1)
	}

	auth := services.NewAuthService(db)
	var apis []api.StoreAPI
	for _, key := range models.AllStoreKeys() {
		if target != "" && key != target {
			continue
		}
		if a := buildStoreAPIForSync(cfg, db, auth, key); a != nil {
			apis = append(apis, a)
		}
	}
	if len(apis) == 0 {
		fmt.Println("Nothing to sync: no enabled, signed-in store matched.")
		fmt.Println("Enable a store, then run: gamelist config signin <store>")
		if target != "" {
			os.Exit(1)
		}
		return
	}

	svc := services.NewGameService(db, cfg.Igdb.ClientID, cfg.Igdb.ClientSecret)
	fmt.Printf("Syncing %d store(s)...\n", len(apis))
	games, fetchedStores, err := svc.FetchAndMergeGames(apis)
	if err != nil {
		log.Fatalf("Sync failed: %v", err)
	}
	fmt.Printf("Fetched %d unique game(s).\n", len(games))
	if len(games) == 0 {
		fmt.Println("Nothing to enrich.")
		return
	}

	// Decide how much IGDB work to do: by default ask the user whether to
	// only complete games that have not been matched yet, or refresh all.
	mode := services.SyncModeIncomplete
	switch {
	case flagRefresh:
		mode = services.SyncModeRefresh
		fmt.Println("Mode: refresh all games against IGDB.")
	case flagIncomplete:
		fmt.Println("Mode: complete missing games only.")
	case !svc.IGDBConfigured():
		fmt.Println("Mode: IGDB is not configured - store data will be saved without enrichment.")
	default:
		matchedCount := 0
		if matched, merr := svc.IGDBMatchedTitles(); merr == nil {
			for _, g := range games {
				if _, ok := matched[services.NormalizeTitle(g.Title)]; ok {
					matchedCount++
				}
			}
		}
		if matchedCount == 0 {
			fmt.Println("No games were matched with IGDB in earlier syncs - enriching all.")
		} else {
			mode = promptSyncMode(matchedCount, len(games))
		}
	}

	progress := func(done, total int, title, result string) {
		fmt.Printf("[%4d/%4d] %-45s %s\n", done, total, truncateForDisplay(title, 45), result)
	}

	final, err := svc.EnrichAndSave(games, mode, progress, fetchedStores)
	if err != nil {
		log.Fatalf("Sync failed: %v", err)
	}

	fmt.Printf("\nSync complete: %d unique game(s) stored.\n", len(final))
	for _, g := range final {
		names := make([]string, 0, len(g.OwnedStores))
		for _, k := range g.OwnedStores {
			names = append(names, models.DisplayStoreName(k))
		}
		fmt.Printf("  - %s [%s]\n", g.Title, strings.Join(names, ", "))
	}
}

// buildStoreAPIForSync returns a ready-to-use store API for `sync`.
// config.yaml is always the credential source of truth, so `config set`
// changes take effect immediately; the database row only records that the
// store was signed in and validated before. Stores without a sign-in record
// are signed in (validated) transparently here.
func buildStoreAPIForSync(cfg *configs.Config, db *database.DB, auth *services.AuthService, key string) api.StoreAPI {
	a := buildStoreAPIFromConfig(cfg, db, key)
	if a == nil {
		return nil
	}
	if cred, err := db.GetStoreCredential(key); err == nil && cred != nil {
		return a
	}
	if err := auth.SignIn(a); err != nil {
		log.Printf("%s: auto sign-in failed: %v", models.DisplayStoreName(key), err)
		return nil
	}
	log.Printf("%s: signed in using credentials from config", models.DisplayStoreName(key))
	return a
}

func promptSyncMode(matchedCount, total int) services.SyncMode {
	fmt.Printf("\n%d of %d game(s) are already matched with IGDB from earlier syncs.\n", matchedCount, total)
	fmt.Println("  1) Complete only the missing games (fast - recommended)")
	fmt.Println("  2) Refresh all games against IGDB (re-checks everything)")
	fmt.Print("Choose [1/2, default 1]: ")

	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	answer := strings.TrimSpace(line)
	if err != nil && answer == "" {
		fmt.Println("(no interactive input - completing missing games only)")
		return services.SyncModeIncomplete
	}
	switch answer {
	case "2":
		return services.SyncModeRefresh
	default:
		return services.SyncModeIncomplete
	}
}

// ---------------------------------------------------------------------------
// list / search / status
// ---------------------------------------------------------------------------

func cmdList(db *database.DB) {
	svc := services.NewGameService(db, "", "")
	games, err := svc.ListGamesFromDB()
	if err != nil {
		log.Fatalf("Failed to list games: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(games); err != nil {
		log.Fatalf("Failed to encode games: %v", err)
	}
}

// cmdMulti lists games owned on MORE THAN ONE store - the cross-store view.
func cmdMulti(db *database.DB, args []string) {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			fmt.Printf("Unknown option %q (usage: multi [--json])\n", a)
			os.Exit(1)
		}
	}

	svc := services.NewGameService(db, "", "")
	games, err := svc.ListGamesFromDB()
	if err != nil {
		log.Fatalf("Failed to list games: %v", err)
	}

	multi := make([]models.Game, 0)
	for _, g := range games {
		if len(g.OwnedStores) > 1 {
			multi = append(multi, g)
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(multi); err != nil {
			log.Fatalf("Failed to encode games: %v", err)
		}
		return
	}

	if len(multi) == 0 {
		fmt.Println("No games are owned on multiple stores yet.")
		fmt.Println("Ownership merges by game title - make sure every store you own")
		fmt.Println("games on has been synced: gamelist sync")
		return
	}

	fmt.Printf("%d game(s) owned on multiple stores:\n", len(multi))
	for _, g := range multi {
		names := make([]string, 0, len(g.OwnedStores))
		for _, k := range g.OwnedStores {
			names = append(names, models.DisplayStoreName(k))
		}
		fmt.Printf("  - %s  [%s]\n", g.Title, strings.Join(names, ", "))
	}
}

func cmdSearch(cfg *configs.Config, db *database.DB, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: gamelist search <title>")
		os.Exit(1)
	}
	query := strings.Join(args, " ")
	svc := services.NewGameService(db, cfg.Igdb.ClientID, cfg.Igdb.ClientSecret)
	results, err := svc.SearchIGDB(query)
	if err != nil {
		log.Fatalf("Search failed: %v", err)
	}
	if len(results) == 0 {
		fmt.Println("No IGDB results.")
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		log.Fatalf("Failed to encode results: %v", err)
	}
}

func cmdStatus(cfg *configs.Config, db *database.DB) {
	fmt.Println("IGDB enrichment:", igdbStatus(cfg))
	fmt.Println()
	fmt.Printf("%-12s %-9s %s\n", "STORE", "CONFIG", "SIGNED-IN")
	for _, key := range models.AllStoreKeys() {
		cred, err := db.GetStoreCredential(key)
		signedIn := "no"
		if err == nil && cred != nil {
			signedIn = "yes"
			if cred.ExpiresAt > 0 {
				signedIn += fmt.Sprintf(" (token expires %s)", time.Unix(cred.ExpiresAt, 0).Format("2006-01-02 15:04"))
			}
		}
		fmt.Printf("%-12s %-9s %s\n", key, boolLabel(storeEnabled(cfg, key)), signedIn)
	}
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  gamelist config show              see current values")
	fmt.Println(`  gamelist config signin steam      interactive store setup`)
	fmt.Println(`  gamelist search "doom"            test IGDB credentials`)
	fmt.Println("  gamelist sync                     fetch + match + store")
}

// ---------------------------------------------------------------------------
// config command
// ---------------------------------------------------------------------------

func cmdConfig(configPath string, cfg *configs.Config, db *database.DB, args []string) {
	sub := "help"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "show":
		reveal := false
		for _, a := range args[1:] {
			if a == "--reveal" {
				reveal = true
			}
		}
		cmdConfigShow(cfg, reveal)
	case "set":
		cmdConfigSet(configPath, cfg, args[1:])
	case "signin":
		if len(args) < 2 {
			fmt.Println("Usage: gamelist config signin <store>")
			fmt.Println("Stores: " + strings.Join(models.AllStoreKeys(), ", "))
			os.Exit(1)
		}
		cmdConfigSignin(configPath, cfg, db, args[1])
	case "help", "-h", "--help":
		printConfigHelp()
	default:
		fmt.Printf("Unknown config subcommand %q\n\n", sub)
		printConfigHelp()
		os.Exit(1)
	}
}

func cmdConfigShow(cfg *configs.Config, reveal bool) {
	fmt.Println("Current configuration:")
	for _, ek := range configs.EditableKeys() {
		v := configValue(cfg, ek.Key)
		display := v
		switch {
		case v == "":
			display = "(not set)"
		case isSecretKey(ek.Key) && !reveal:
			display = maskSecret(v)
		}
		fmt.Printf("  %-34s %s\n", ek.Key, display)
	}
	if !reveal {
		fmt.Println("\nSecrets are masked. Use 'gamelist config show --reveal' to see them.")
	}
}

func cmdConfigSet(configPath string, cfg *configs.Config, args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: gamelist config set <key> <value>")
		fmt.Println("Run 'gamelist config show' to list editable keys and their current values.")
		os.Exit(1)
	}
	key := args[0]
	value := strings.Join(args[1:], " ")

	kind := configs.LookupKeyKind(key)
	if kind == "" {
		fmt.Printf("Unknown config key %q. Editable keys:\n", key)
		for _, ek := range configs.EditableKeys() {
			fmt.Printf("  %-34s %s\n", ek.Key, ek.Kind)
		}
		os.Exit(1)
	}
	if kind == "bool" {
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			value = "true"
		case "false", "0", "no", "off":
			value = "false"
		default:
			fmt.Printf("%q needs a boolean value (true/false, yes/no, 1/0, on/off).\n", key)
			os.Exit(1)
		}
	}

	_ = cfg // current values are shown by `config show`; writes go straight to the file
	if err := configs.SetYAMLValues(configPath, map[string]string{key: value}); err != nil {
		log.Fatalf("Failed to update config: %v", err)
	}
	fmt.Printf("Updated %s = %s.\n", key, value)

	switch {
	case key == "igdb.clientId" || key == "igdb.clientSecret":
		fmt.Println(`Test with: gamelist search "doom"`)
	case key == "database.dataSourceName":
		fmt.Println("Takes effect on the next run.")
	case strings.HasPrefix(key, "stores."):
		store := strings.Split(key, ".")[1]
		if key == storeKey(store, "enabled") && value == "true" {
			fmt.Printf("Validate and store credentials with: gamelist config signin %s\n", store)
		} else if key != storeKey(store, "enabled") {
			fmt.Printf("Validate with: gamelist config signin %s\n", store)
		}
	}
}

// stdinReader is the single shared stdin reader so interactive flows never
// fight over buffered input.
var stdinReader = bufio.NewReader(os.Stdin)

func cmdConfigSignin(configPath string, cfg *configs.Config, db *database.DB, store string) {
	auth := services.NewAuthService(db)

	switch store {
	case models.StoreSteam:
		fmt.Println("Steam sign-in")
		fmt.Println("  Web API key: https://steamcommunity.com/dev/apikey")
		fmt.Println("  SteamID64:   your profile URL or steamid.io")
		key := promptValue(stdinReader, "Steam Web API key", maskSecret(cfg.Stores.Steam.APIKey), cfg.Stores.Steam.APIKey)
		id := promptValue(stdinReader, "SteamID64", cfg.Stores.Steam.SteamID, cfg.Stores.Steam.SteamID)
		if key == "" || id == "" {
			fmt.Println("Both values are required.")
			os.Exit(1)
		}
		apiC := api.NewSteamAPI(key, id)
		if err := apiC.SignIn(); err != nil {
			log.Printf("Steam validation failed: %v", err)
			fmt.Fprintln(os.Stderr, "Nothing was saved - fix the values and try again.")
			os.Exit(1)
		}
		writeConfigUpdates(configPath, map[string]string{
			"stores.steam.enabled": "true",
			"stores.steam.apiKey":  key,
			"stores.steam.steamId": id,
		})
		if err := auth.Persist(apiC); err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println("Signed in to Steam: credentials validated and saved to config + database.")
		fmt.Println("Note: your profile's \"Game details\" privacy must stay public for sync to see your library.")
		fmt.Println("Next: gamelist sync steam")

	case models.StoreEpic:
		fmt.Println("Epic Games Store sign-in (device-code flow)")
		fmt.Println("A browser window will open - log in with your Epic account and approve the request.")
		apiC := api.NewEpicGamesAPI(cfg.Stores.Epic.ClientID, cfg.Stores.Epic.ClientSecret, epicHooks(db))
		if err := auth.SignIn(apiC); err != nil {
			log.Printf("Epic sign-in failed: %v", err)
			os.Exit(1)
		}
		writeConfigUpdates(configPath, map[string]string{"stores.epic.enabled": "true"})
		fmt.Println("Signed in to Epic Games Store: session validated and saved to the local database.")
		fmt.Println("The session refreshes itself automatically on later syncs.")
		fmt.Println("Note: this uses Epic's unofficial launcher APIs (same mechanism as Heroic/Legendary).")
		fmt.Println("Next: gamelist sync epic")

	case models.StoreBattleNet:
		fmt.Println("Battle.net sign-in")
		fmt.Println("  Client credentials: https://develop.battle.net/access/clients")
		id := promptValue(stdinReader, "Client ID", cfg.Stores.Battlenet.ClientID, cfg.Stores.Battlenet.ClientID)
		secret := promptValue(stdinReader, "Client Secret", maskSecret(cfg.Stores.Battlenet.ClientSecret), cfg.Stores.Battlenet.ClientSecret)
		if id == "" || secret == "" {
			fmt.Println("Both values are required.")
			os.Exit(1)
		}
		apiC := api.NewBattleNetAPI(id, secret)
		if err := apiC.SignIn(); err != nil {
			log.Printf("Battle.net validation failed: %v", err)
			fmt.Fprintln(os.Stderr, "Nothing was saved - fix the values and try again.")
			os.Exit(1)
		}
		writeConfigUpdates(configPath, map[string]string{
			"stores.battlenet.enabled":      "true",
			"stores.battlenet.clientId":     id,
			"stores.battlenet.clientSecret": secret,
		})
		if err := auth.Persist(apiC); err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println("Signed in to Battle.net: credentials validated and saved.")
		fmt.Println("Note: Blizzard exposes no unified library API - sync will report it as not implemented.")

	case models.StoreGOG:
		fmt.Println("GOG sign-in (browser login + redirect-URL paste)")
		fmt.Println("The Galaxy client credentials are built in - nothing to configure.")
		apiC := api.NewGOGAPI(cfg.Stores.Gog.ClientID, cfg.Stores.Gog.ClientSecret, gogHooks(db))
		if err := auth.SignIn(apiC); err != nil {
			log.Printf("GOG sign-in failed: %v", err)
			os.Exit(1)
		}
		writeConfigUpdates(configPath, map[string]string{"stores.gog.enabled": "true"})
		fmt.Println("Signed in to GOG: session validated and saved to the local database.")
		fmt.Println("The session refreshes itself automatically on later syncs.")
		fmt.Println("Note: this uses GOG's unofficial account APIs (same mechanism as Heroic/Legendary/MiniGalaxy).")
		fmt.Println("Next: gamelist sync gog")

	case models.StoreUbisoft:
		fmt.Println("Ubisoft Connect sign-in (local client integration)")
		fmt.Println("Your library is read from the locally installed Ubisoft Connect client's")
		fmt.Println("own cache - no password or token is needed here.")
		apiC := api.NewUbisoftAPI(cfg.Stores.Ubisoft.DataPath)
		if err := auth.SignIn(apiC); err != nil {
			log.Printf("Ubisoft sign-in failed: %v", err)
			os.Exit(1)
		}
		writeConfigUpdates(configPath, map[string]string{"stores.ubisoft.enabled": "true"})
		fmt.Println("Signed in to Ubisoft Connect: local client data found and validated.")
		fmt.Println("Keep the Ubisoft Connect client installed and signed in; the library")
		fmt.Println("refreshes from its cache on every sync.")
		fmt.Println("Next: gamelist sync ubisoft")

	case models.StoreXbox, models.StoreDLsite:
		var ph api.StoreAPI
		switch store {
		case models.StoreXbox:
			ph = api.NewXboxAPI()
		case models.StoreDLsite:
			ph = api.NewDLsiteAPI()
		}
		fmt.Printf("Cannot sign in to %s yet:\n", models.DisplayStoreName(store))
		if err := ph.SignIn(); err != nil {
			fmt.Printf("  %v\n", err)
		}
		os.Exit(1)

	default:
		fmt.Printf("Unknown store %q. Stores: %s\n", store, strings.Join(models.AllStoreKeys(), ", "))
		os.Exit(1)
	}
}

func writeConfigUpdates(configPath string, updates map[string]string) {
	if err := configs.SetYAMLValues(configPath, updates); err != nil {
		log.Fatalf("Credentials validated, but writing %s failed: %v", configPath, err)
	}
}

func printConfigHelp() {
	fmt.Print(`gamelist config - manage configuration without editing config.yaml

Usage:
  gamelist config show [--reveal]     Print current values (secrets masked unless --reveal)
  gamelist config set <key> <value>   Set one value, e.g.:
                                        gamelist config set igdb.clientId myclientid
                                        gamelist config set stores.steam.enabled true
  gamelist config signin <store>      Interactive sign-in: prompts for credentials,
                                      validates them live, saves to config + database
  gamelist config help                This help

Editable keys:
  igdb.clientId, igdb.clientSecret
  database.dataSourceName
  stores.steam.enabled, stores.steam.apiKey, stores.steam.steamId
  stores.epic.enabled, stores.epic.clientId, stores.epic.clientSecret
  stores.gog.enabled, stores.gog.clientId, stores.gog.clientSecret
  stores.ubisoft.enabled, stores.ubisoft.dataPath
  stores.battlenet.enabled, stores.battlenet.clientId, stores.battlenet.clientSecret
  stores.ubisoft.enabled, stores.xbox.enabled, stores.dlsite.enabled
`)
}

func configValue(cfg *configs.Config, key string) string {
	switch key {
	case "igdb.clientId":
		return cfg.Igdb.ClientID
	case "igdb.clientSecret":
		return cfg.Igdb.ClientSecret
	case "database.dataSourceName":
		return cfg.Database.DataSourceName
	case "stores.steam.enabled":
		return strconv.FormatBool(cfg.Stores.Steam.Enabled)
	case "stores.steam.apiKey":
		return cfg.Stores.Steam.APIKey
	case "stores.steam.steamId":
		return cfg.Stores.Steam.SteamID
	case "stores.epic.enabled":
		return strconv.FormatBool(cfg.Stores.Epic.Enabled)
	case "stores.epic.clientId":
		return cfg.Stores.Epic.ClientID
	case "stores.epic.clientSecret":
		return cfg.Stores.Epic.ClientSecret
	case "stores.gog.enabled":
		return strconv.FormatBool(cfg.Stores.Gog.Enabled)
	case "stores.gog.clientId":
		return cfg.Stores.Gog.ClientID
	case "stores.gog.clientSecret":
		return cfg.Stores.Gog.ClientSecret
	case "stores.ubisoft.enabled":
		return strconv.FormatBool(cfg.Stores.Ubisoft.Enabled)
	case "stores.ubisoft.dataPath":
		return cfg.Stores.Ubisoft.DataPath
	case "stores.xbox.enabled":
		return strconv.FormatBool(cfg.Stores.Xbox.Enabled)
	case "stores.battlenet.enabled":
		return strconv.FormatBool(cfg.Stores.Battlenet.Enabled)
	case "stores.battlenet.clientId":
		return cfg.Stores.Battlenet.ClientID
	case "stores.battlenet.clientSecret":
		return cfg.Stores.Battlenet.ClientSecret
	case "stores.dlsite.enabled":
		return strconv.FormatBool(cfg.Stores.Dlsite.Enabled)
	}
	return ""
}

func isSecretKey(key string) bool {
	switch key {
	case "igdb.clientSecret", "stores.steam.apiKey", "stores.epic.clientSecret",
		"stores.gog.clientSecret", "stores.battlenet.clientSecret":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// store construction
// ---------------------------------------------------------------------------

func buildStoreAPIFromConfig(cfg *configs.Config, db *database.DB, storeName string) api.StoreAPI {
	switch storeName {
	case models.StoreSteam:
		s := cfg.Stores.Steam
		if !s.Enabled {
			return nil
		}
		return api.NewSteamAPI(s.APIKey, s.SteamID)
	case models.StoreEpic:
		s := cfg.Stores.Epic
		if !s.Enabled {
			return nil
		}
		return api.NewEpicGamesAPI(s.ClientID, s.ClientSecret, epicHooks(db))
	case models.StoreGOG:
		s := cfg.Stores.Gog
		if !s.Enabled {
			return nil
		}
		return api.NewGOGAPI(s.ClientID, s.ClientSecret, gogHooks(db))
	case models.StoreUbisoft:
		s := cfg.Stores.Ubisoft
		if !s.Enabled {
			return nil
		}
		return api.NewUbisoftAPI(s.DataPath)
	case models.StoreXbox:
		if !cfg.Stores.Xbox.Enabled {
			return nil
		}
		return api.NewXboxAPI()
	case models.StoreBattleNet:
		s := cfg.Stores.Battlenet
		if !s.Enabled {
			return nil
		}
		return api.NewBattleNetAPI(s.ClientID, s.ClientSecret)
	case models.StoreDLsite:
		if !cfg.Stores.Dlsite.Enabled {
			return nil
		}
		return api.NewDLsiteAPI()
	default:
		return nil
	}
}

// gogHooks wires GOG session persistence to the local database and stdin for
// the paste-the-redirect-URL step of the authorization-code flow.
func gogHooks(db *database.DB) *api.GOGTokenHooks {
	return &api.GOGTokenHooks{
		Load: func() *api.GOGTokens {
			cred, err := db.GetStoreCredential(models.StoreGOG)
			if err != nil || cred == nil || cred.AccessToken == "" {
				return nil
			}
			tok := &api.GOGTokens{
				AccessToken:  cred.AccessToken,
				RefreshToken: cred.RefreshToken,
				ExpiresAt:    time.Unix(cred.ExpiresAt, 0),
			}
			if cred.Meta != "" {
				var meta struct {
					Username string `json:"username"`
				}
				if err := json.Unmarshal([]byte(cred.Meta), &meta); err == nil {
					tok.Username = meta.Username
				}
			}
			return tok
		},
		Save: func(tok api.GOGTokens) {
			meta, _ := json.Marshal(map[string]string{
				"username": tok.Username,
			})
			if err := db.SaveStoreCredentials(models.StoreGOG, tok.AccessToken, tok.RefreshToken, tok.ExpiresAt.Unix(), string(meta)); err != nil {
				log.Printf("Warning: could not store GOG session: %v", err)
			}
		},
		ReadInput: func() (string, error) {
			return stdinReader.ReadString('\n')
		},
	}
}

func storeEnabled(cfg *configs.Config, key string) bool {
	return buildStoreAPIFromConfig(cfg, nil, key) != nil
}

// epicHooks wires Epic session persistence to the local database: the
// launcher access/refresh tokens live in store_credentials.
func epicHooks(db *database.DB) *api.EpicTokenHooks {
	if db == nil {
		return nil
	}
	return &api.EpicTokenHooks{
		Load: func() *api.EpicTokens {
			cred, err := db.GetStoreCredential(models.StoreEpic)
			if err != nil || cred == nil || cred.AccessToken == "" {
				return nil
			}
			tok := &api.EpicTokens{
				AccessToken:  cred.AccessToken,
				RefreshToken: cred.RefreshToken,
				ExpiresAt:    time.Unix(cred.ExpiresAt, 0),
			}
			if cred.Meta != "" {
				var meta struct {
					AccountID   string `json:"account_id"`
					DisplayName string `json:"display_name"`
				}
				if err := json.Unmarshal([]byte(cred.Meta), &meta); err == nil {
					tok.AccountID = meta.AccountID
					tok.DisplayName = meta.DisplayName
				}
			}
			return tok
		},
		Save: func(tok api.EpicTokens) {
			meta, _ := json.Marshal(map[string]string{
				"account_id":   tok.AccountID,
				"display_name": tok.DisplayName,
			})
			if err := db.SaveStoreCredentials(models.StoreEpic, tok.AccessToken, tok.RefreshToken, tok.ExpiresAt.Unix(), string(meta)); err != nil {
				log.Printf("Warning: could not store Epic session: %v", err)
			}
		},
	}
}

func igdbStatus(cfg *configs.Config) string {
	if cfg.Igdb.ClientID != "" && cfg.Igdb.ClientSecret != "" {
		return "configured (token is fetched and refreshed automatically)"
	}
	return "NOT configured - set igdb.clientId + igdb.clientSecret in configs/config.yaml"
}

func boolLabel(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// promptValue prints label with an optional (masked) current value; an empty
// answer keeps the current value.
func promptValue(r *bufio.Reader, label, displayCurrent, fullCurrent string) string {
	if displayCurrent != "" {
		fmt.Printf("%s [current: %s - Enter to keep]: ", label, displayCurrent)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Println("\nInput unavailable.")
		os.Exit(1)
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return fullCurrent
	}
	return v
}

func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "********"
	}
	return v[:4] + "..." + v[len(v)-4:]
}

func truncateForDisplay(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func storeKey(store, field string) string {
	return "stores." + store + "." + field
}

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

func printUsage() {
	fmt.Print(`Game List Manager - local multi-store game library

Usage: gamelist [--config <path>] <command> [args]

Commands:
  signin <store>    Validate and store credentials for a store
  signout <store>   Remove stored credentials for a store
  sync [store]      Fetch owned games, match IGDB, store locally
                    [--refresh | --incomplete] skip the completion prompt
  list              Print all stored games as JSON
  multi [--json]    List games owned on more than one store
  search <title>    Search IGDB directly (tests your IGDB credentials)
  status            Show which stores are enabled and signed in
  config            Manage configuration without editing config.yaml
    config show [--reveal]     Print current values (secrets masked)
    config set <key> <value>   Set one value
    config signin <store>      Interactive sign-in: prompts, validates, saves
    config help                List editable keys

Stores: steam, epic, gog, ubisoft, xbox, battlenet, dlsite
`)
}
