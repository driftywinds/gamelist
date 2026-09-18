package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamelist/internal/models"
)

// Synthetic Ubisoft Connect data mirroring the real formats:
//
//	ownership/<profileId>: binary blob with $-prefixed owned app_ids
//	cache/configuration/configurations: concatenated product configs
const ubisoftTestOwnership = "\x00\x01\x00\x00" +
	"$20a72d33-4b3e-47df-b88c-bbd632ed57df" + "\x91\x02" +
	"$9c4a1757-422b-458f-b4d2-5e623c911ba6" + "\x92\x03" +
	"$b0b0b0b0-b0b0-b0b0-b0b0-b0b0b0b0b0b0" + "\x93\x04" + // owned, no config
	"$15a42aaf-f5cc-47df-bbb3-f59768ac6eed" + "\x94\x05" +
	"WF6L-CHRM-ANC7-BMPN" // activation-code noise

const ubisoftTestConfigurations = `#----------------------------------------------------------
# ASSASSIN'S CREED 2
#----------------------------------------------------------
version: 2.0
root: .
  name: "Assassin's Creed II"
  background_image: aaa.jpg
  start_game:
    executables:
      - path: 
          relative: AC2.exe
    crash_reporting:
      space_id: 97ef669a-c028-4c25-b5ff-7335aa5d806c
      app_id: 20a72d33-4b3e-47df-b88c-bbd632ed57df
#----------------------------------------------------------
# FAR CRY 3
#----------------------------------------------------------
version: 2.0
root: .
  name: "Far Cry® 3"
  start_game:
    executables:
      - path: 
          relative: FC3.exe
    crash_reporting:
      app_id: 15a42aaf-f5cc-47df-bbb3-f59768ac6eed
#----------------------------------------------------------
# SOME DLC (no start_game -> must be filtered)
#----------------------------------------------------------
version: 2.0
root: .
  name: "Some DLC Pack"
  app_id: b0b0b0b0-b0b0-b0b0-b0b0-b0b0b0b0b0b0
#----------------------------------------------------------
# RAYMAN ORIGINS (owned app id inside a non-start_game segment? no - launchable)
#----------------------------------------------------------
version: 2.0
root: .
  name: "Rayman Origins"
  start_game:
    executables:
      - path: 
          relative: RaymanOrigins.exe
  space_id: c8237ba1-f3a7-4a93-acb6-a23044c4f0cf
  app_id: 9c4a1757-422b-458f-b4d2-5e623c911ba6
`

func newUbisoftTestData(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	ownershipDir := filepath.Join(root, "cache", "ownership")
	configDir := filepath.Join(root, "cache", "configuration")
	if err := os.MkdirAll(ownershipDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownershipDir, "72f261df-d26a-453c-9c7d-f2e202f605e3"), []byte(ubisoftTestOwnership), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "configurations"), []byte(ubisoftTestConfigurations), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestUbisoftSignInRequiresLocalData(t *testing.T) {
	empty := t.TempDir()
	api := NewUbisoftAPI(empty)
	err := api.SignIn()
	if err == nil {
		t.Fatal("expected error when no Ubisoft client data exists")
	}
	if !strings.Contains(err.Error(), "Ubisoft Connect") {
		t.Fatalf("error should mention Ubisoft Connect: %v", err)
	}

	root := newUbisoftTestData(t)
	api2 := NewUbisoftAPI(root)
	if err := api2.SignIn(); err != nil {
		t.Fatalf("SignIn with local data: %v", err)
	}
}

func TestUbisoftGetOwnedGamesMapsAndFilters(t *testing.T) {
	root := newUbisoftTestData(t)
	g := NewUbisoftAPI(root)

	games, err := g.GetOwnedGames()
	if err != nil {
		t.Fatalf("GetOwnedGames: %v", err)
	}

	// Owned app ids: AC2 (config, launchable), Rayman Origins (config,
	// launchable), Far Cry 3 (config, launchable), b0b0... (config exists but
	// no start_game -> filtered as DLC).
	if len(games) != 3 {
		t.Fatalf("games = %d (%+v), want 3", len(games), games)
	}

	byTitle := map[string]models.Game{}
	for _, g := range games {
		byTitle[g.Title] = g
	}
	if g, ok := byTitle["Assassin's Creed II"]; !ok || g.StoreIDs[models.StoreUbisoft] != "20a72d33-4b3e-47df-b88c-bbd632ed57df" {
		t.Fatalf("AC2 missing or wrong: %+v", g)
	}
	if g, ok := byTitle["Far Cry® 3"]; !ok {
		t.Fatalf("Far Cry® 3 missing: %+v", g)
	}
	if g, ok := byTitle["Rayman Origins"]; !ok {
		t.Fatalf("Rayman Origins missing: %+v", g)
	}
	if _, ok := byTitle["Some DLC Pack"]; ok {
		t.Fatal("DLC without start_game must be filtered")
	}
	for _, g := range games {
		if g.OwnedStores[0] != models.StoreUbisoft {
			t.Fatalf("owned stores = %v", g.OwnedStores)
		}
	}
}

func TestUbisoftGetOwnedGamesNoData(t *testing.T) {
	api := NewUbisoftAPI(t.TempDir())
	if _, err := api.GetOwnedGames(); err == nil {
		t.Fatal("expected error when local data is missing")
	}
}

func TestUbisoftExtractOwnedAppIDs(t *testing.T) {
	data := []byte("junk$abc12345-1234-1234-1234-123456789abc junk $abc12345-1234-1234-1234-123456789abc\x00\x01$def45678-1234-1234-1234-123456789abc")
	ids := ubisoftExtractOwnedAppIDs(data)
	if len(ids) != 2 || ids[0] != "abc12345-1234-1234-1234-123456789abc" || ids[1] != "def45678-1234-1234-1234-123456789abc" {
		t.Fatalf("ids = %v", ids)
	}
}
