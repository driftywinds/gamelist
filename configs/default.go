package configs

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultConfigYAML is written when no config file exists yet (e.g. on a
// fresh clone - configs/config.yaml is gitignored because it holds personal
// API credentials). It matches the documented default setup.
const defaultConfigYAML = `# ---------------------------------------------------------------------------
# IGDB (game metadata + matching) - free for non-commercial use.
# 1. Register an app at https://dev.twitch.tv/console/apps/create
#    (requires a free Twitch account with 2FA; type "Confidential").
# 2. Paste the Client ID and Client Secret below, or run:
#      gamelist config set igdb.clientId <your-client-id>
#      gamelist config set igdb.clientSecret <your-client-secret>
# The app fetches and refreshes the OAuth token itself - never paste a
# bearer token here.
# ---------------------------------------------------------------------------
igdb:
  clientId: ""
  clientSecret: ""

# ---------------------------------------------------------------------------
# SQLite database file. Relative paths resolve next to this config file,
# wherever the exe is run from.
# ---------------------------------------------------------------------------
database:
  dataSourceName: "gamelist.db"

# ---------------------------------------------------------------------------
# Store integrations. Set them up with:
#      gamelist config signin <store>
# Status:
#   steam     WORKING       needs a Web API key + your SteamID64; the Steam
#                           profile's "Game details" privacy must be public
#   epic      WORKING       sign in with: gamelist config signin epic (device-
#                           code login in your browser; unofficial launcher API)
#   gog       WORKING       sign in with: gamelist config signin gog (browser
#                           login + paste the redirect URL; Galaxy client is
#                           built in)
#   ubisoft   WORKING       reads the library from the locally installed
#                           Ubisoft Connect client's cache - no password or
#                           token needed; the client must stay installed
#   battlenet partial       credentials are validated; Blizzard exposes no
#                           unified library API
#   xbox / dlsite
#             placeholders  these stores have no usable public library API
# ---------------------------------------------------------------------------
stores:
  steam:
    enabled: false
    apiKey: ""     # https://steamcommunity.com/dev/apikey
    steamId: ""    # your SteamID64, e.g. 76561198012345678
  epic:
    enabled: false
    # The built-in device-login client works out of the box - the two keys
    # below are OPTIONAL overrides for advanced use (another Epic auth client
    # that has the device_code grant enabled).
    clientId: ""
    clientSecret: ""
  gog:
    enabled: false
    # The built-in Galaxy client credentials work out of the box - the two
    # keys below are OPTIONAL overrides for advanced use.
    clientId: ""
    clientSecret: ""
  ubisoft:
    enabled: false
    # Leave dataPath empty to use the standard client location
    # (%LocalAppData%\Ubisoft Game Launcher). Override only for non-standard
    # installs.
    dataPath: ""
  ubisoft:
    enabled: false
  xbox:
    enabled: false
  battlenet:
    enabled: false
    clientId: ""   # https://develop.battle.net/access/clients
    clientSecret: ""
  dlsite:
    enabled: false
`

// EnsureExists creates a default config file at configPath when none exists,
// so a fresh clone works immediately (`gamelist config set ...` needs a file
// to edit). Returns true when a new file was created.
func EnsureExists(configPath string) (bool, error) {
	if _, err := os.Stat(configPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat config: %w", err)
	}

	if dir := filepath.Dir(configPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("create config dir: %w", err)
		}
	}
	if err := os.WriteFile(configPath, []byte(defaultConfigYAML), 0o600); err != nil {
		return false, fmt.Errorf("create default config: %w", err)
	}
	return true, nil
}
