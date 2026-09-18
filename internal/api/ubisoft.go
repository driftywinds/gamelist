package api

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gamelist/internal/models"
)

// ---------------------------------------------------------------------------
// Ubisoft Connect (local client integration).
//
// Ubisoft retired the legacy UbiServices gateway (verified live: every
// documented endpoint, including /v3/profiles/session, 404s at the gateway),
// and there is no public OAuth flow. The proven remaining path - used by
// Playnite - is reading the locally installed Ubisoft Connect client's own
// cache:
//
//   %LocalAppData%\Ubisoft Game Launcher\cache\ownership\<profileId>
//       binary blob containing the owned app_ids (hex GUIDs prefixed with $)
//   %LocalAppData%\Ubisoft Game Launcher\cache\configuration\configurations
//       a readable concatenation of per-product YAML-ish configs, each with
//       name:, app_id: and start_game: fields
//
// Owned games = owned app_ids that map to a launchable (start_game) product.
// The data is as fresh as the last time the Ubisoft Connect client ran.
// ---------------------------------------------------------------------------

var (
	ubisoftOwnedIDRe = regexp.MustCompile(`\$([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	ubisoftAppIDRe   = regexp.MustCompile(`(?m)^\s*app_id:\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	ubisoftNameRe    = regexp.MustCompile(`(?m)^\s{2}name:\s*"([^"]+)"`)
	ubisoftVersionRe = regexp.MustCompile(`(?m)^version: .*$`)
)

// UbisoftDefaultDataPath returns the default Ubisoft Connect data directory.
func UbisoftDefaultDataPath() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return filepath.Join(".", "Ubisoft Game Launcher")
	}
	return filepath.Join(local, "Ubisoft Game Launcher")
}

// UbisoftAPI implements StoreAPI from the local Ubisoft Connect client data.
type UbisoftAPI struct {
	dataPath string // e.g. C:\Users\<u>\AppData\Local\Ubisoft Game Launcher
}

// NewUbisoftAPI builds a Ubisoft integration. An empty dataPath uses the
// default per-user location.
func NewUbisoftAPI(dataPath string) *UbisoftAPI {
	if strings.TrimSpace(dataPath) == "" {
		dataPath = UbisoftDefaultDataPath()
	}
	return &UbisoftAPI{dataPath: dataPath}
}

func (u *UbisoftAPI) Key() string  { return models.StoreUbisoft }
func (u *UbisoftAPI) Name() string { return models.DisplayStoreName(models.StoreUbisoft) }

// SignIn validates that the local Ubisoft Connect data exists and is usable.
// There is no remote authentication: the client itself owns the login.
func (u *UbisoftAPI) SignIn() error {
	ownership, err := u.ownershipFiles()
	if err != nil {
		return err
	}
	if len(ownership) == 0 {
		return fmt.Errorf(
			"no Ubisoft Connect ownership data found under %s - install Ubisoft Connect, sign in once so it syncs your library, then retry",
			u.dataPath)
	}
	return nil
}

// GetOwnedGames returns the account's launchable games from the local cache.
func (u *UbisoftAPI) GetOwnedGames() ([]models.Game, error) {
	ownershipFiles, err := u.ownershipFiles()
	if err != nil {
		return nil, err
	}
	if len(ownershipFiles) == 0 {
		return nil, fmt.Errorf("no Ubisoft Connect ownership data found under %s - sign in once with the Ubisoft Connect client first", u.dataPath)
	}

	var owned []string
	for _, f := range ownershipFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read ownership cache %s: %w", f, err)
		}
		owned = append(owned, ubisoftExtractOwnedAppIDs(data)...)
	}
	if len(owned) == 0 {
		return nil, fmt.Errorf("Ubisoft ownership cache contains no app ids - run the Ubisoft Connect client once and retry")
	}

	configData, err := os.ReadFile(filepath.Join(u.dataPath, "cache", "configuration", "configurations"))
	if err != nil {
		return nil, fmt.Errorf("read Ubisoft configurations cache: %w", err)
	}
	nameByAppID := ubisoftParseConfigurations(configData)

	// Map owned app_ids to product names; several app_ids can belong to the
	// same product (per-platform/per-DLC variants), so deduplicate by name.
	byName := make(map[string]*models.Game)
	unmapped := 0
	for _, appID := range owned {
		name, ok := nameByAppID[appID]
		if !ok || strings.TrimSpace(name) == "" {
			unmapped++
			continue
		}
		if _, exists := byName[name]; exists {
			continue // product already recorded via another app id
		}
		byName[name] = &models.Game{
			Title:       name,
			OwnedStores: []string{models.StoreUbisoft},
			StoreIDs:    map[string]string{models.StoreUbisoft: appID},
		}
	}
	if unmapped > 0 {
		log.Printf("ubisoft: %d owned app id(s) have no local product config (never installed/launched?) - skipped", unmapped)
	}

	games := make([]models.Game, 0, len(byName))
	for _, g := range byName {
		games = append(games, *g)
	}
	sort.Slice(games, func(i, j int) bool { return games[i].Title < games[j].Title })
	return games, nil
}

// ownershipFiles lists cache\ownership\* files (one per signed-in profile).
func (u *UbisoftAPI) ownershipFiles() ([]string, error) {
	dir := filepath.Join(u.dataPath, "cache", "ownership")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read ownership cache dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

// ubisoftExtractOwnedAppIDs pulls owned app ids out of the binary ownership
// cache: hex GUIDs preceded by a $ marker byte.
func ubisoftExtractOwnedAppIDs(data []byte) []string {
	matches := ubisoftOwnedIDRe.FindAllStringSubmatch(string(data), -1)
	ids := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	return ids
}

// ubisoftParseConfigurations parses the configurations cache: a concatenation
// of per-product YAML-ish documents, each starting with a `version:` line.
// Returns app_id -> product name, but only for launchable products
// (documents containing a start_game: section) so DLC/Ulc configs are skipped.
func ubisoftParseConfigurations(data []byte) map[string]string {
	text := string(data)
	segments := ubisoftVersionRe.Split(text, -1)

	// The version markers are consumed by Split; re-attach nothing - segments
	// after the first correspond to products in order.
	nameByAppID := make(map[string]string)
	for _, seg := range segments {
		nameMatch := ubisoftNameRe.FindStringSubmatch(seg)
		if nameMatch == nil {
			continue
		}
		name := strings.TrimSpace(nameMatch[1])
		if name == "" || !strings.Contains(seg, "start_game:") {
			continue // DLC / ULC / non-launchable product configs
		}
		for _, m := range ubisoftAppIDRe.FindAllStringSubmatch(seg, -1) {
			if _, exists := nameByAppID[m[1]]; !exists {
				nameByAppID[m[1]] = name
			}
		}
	}
	return nameByAppID
}
