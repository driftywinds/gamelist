package configs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `# top comment about IGDB
igdb:
  clientId: ""  # inline comment to preserve
  clientSecret: ""

stores:
  steam:
    enabled: false
    apiKey: ""
    steamId: ""
`

func TestSetYAMLValuesUpdatesAndPreservesComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sampleConfig), 0o600); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	err := SetYAMLValues(path, map[string]string{
		"igdb.clientId":        "my-client-id",
		"stores.steam.id":      "76561198012345678", // unknown section -> created
		"stores.steam.steamId": "76561198012345678",
	})
	// "stores.steam.id" is a valid path shape (created), so no error expected.
	if err != nil {
		t.Fatalf("SetYAMLValues: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Igdb.ClientID != "my-client-id" {
		t.Fatalf("clientId = %q", cfg.Igdb.ClientID)
	}
	if cfg.Stores.Steam.SteamID != "76561198012345678" {
		t.Fatalf("steamId = %q", cfg.Stores.Steam.SteamID)
	}
	// Untouched values stay untouched.
	if cfg.Stores.Steam.Enabled {
		t.Fatal("enabled should still be false")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "# top comment about IGDB") {
		t.Fatalf("head comment lost:\n%s", out)
	}
	if !strings.Contains(out, "# inline comment to preserve") {
		t.Fatalf("inline comment lost:\n%s", out)
	}
}

func TestSetYAMLValuesNormalizesBooleans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sampleConfig), 0o600); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	if err := SetYAMLValues(path, map[string]string{"stores.steam.enabled": "yes"}); err != nil {
		t.Fatalf("SetYAMLValues: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Stores.Steam.Enabled {
		t.Fatal("enabled should be true after setting 'yes'")
	}

	if err := SetYAMLValues(path, map[string]string{"stores.steam.enabled": "0"}); err != nil {
		t.Fatalf("SetYAMLValues 2: %v", err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	if cfg.Stores.Steam.Enabled {
		t.Fatal("enabled should be false after setting '0'")
	}
}

func TestLookupKeyKind(t *testing.T) {
	if got := LookupKeyKind("stores.steam.enabled"); got != "bool" {
		t.Fatalf("kind = %q", got)
	}
	if got := LookupKeyKind("igdb.clientSecret"); got != "string" {
		t.Fatalf("kind = %q", got)
	}
	if got := LookupKeyKind("no.such.key"); got != "" {
		t.Fatalf("unknown key kind = %q", got)
	}
}
