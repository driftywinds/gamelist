package configs

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config mirrors configs/config.yaml.
type Config struct {
	Igdb     IgdbConfig     `yaml:"igdb"`
	Database DatabaseConfig `yaml:"database"`
	Stores   StoreConfigs   `yaml:"stores"`
}

// IgdbConfig holds Twitch OAuth credentials for the IGDB API. The app fetches
// and refreshes the short-lived access token itself - users never paste a
// bearer token.
type IgdbConfig struct {
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
}

// DatabaseConfig holds the SQLite database file path.
type DatabaseConfig struct {
	DataSourceName string `yaml:"dataSourceName"`
}

// StoreConfigs holds per-store configuration.
type StoreConfigs struct {
	Steam     SteamConfig     `yaml:"steam"`
	Epic      EpicConfig      `yaml:"epic"`
	Gog       EnabledConfig   `yaml:"gog"`
	Ubisoft   EnabledConfig   `yaml:"ubisoft"`
	Xbox      EnabledConfig   `yaml:"xbox"`
	Battlenet BattleNetConfig `yaml:"battlenet"`
	Dlsite    EnabledConfig   `yaml:"dlsite"`
}

// EnabledConfig is the shape of stores that only support an on/off flag for
// now (their integrations are placeholders awaiting viable APIs).
type EnabledConfig struct {
	Enabled bool `yaml:"enabled"`
}

// SteamConfig holds Steam Web API credentials.
type SteamConfig struct {
	Enabled bool   `yaml:"enabled"`
	APIKey  string `yaml:"apiKey"`  // https://steamcommunity.com/dev/apikey
	SteamID string `yaml:"steamId"` // the account's SteamID64
}

// EpicConfig holds Epic OAuth App credentials.
type EpicConfig struct {
	Enabled      bool   `yaml:"enabled"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
}

// BattleNetConfig holds Blizzard OAuth credentials.
type BattleNetConfig struct {
	Enabled      bool   `yaml:"enabled"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
}

// LoadConfig loads configuration from a YAML file path.
func LoadConfig(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", filePath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Database.DataSourceName == "" {
		cfg.Database.DataSourceName = "gamelist.db"
	}
	return &cfg, nil
}
