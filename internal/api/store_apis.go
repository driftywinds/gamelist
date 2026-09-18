package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gamelist/internal/models"
	"gamelist/pkg/api"
)

// ErrNotImplemented marks store integrations that cannot list a library yet.
// Each store wraps it with its concrete reason; `sync` reports the message
// per store instead of pretending everything worked.
var ErrNotImplemented = errors.New("store integration not implemented")

// StoreAPI is the contract every game-store integration implements. It is the
// stable surface a future frontend (HTTP wrapper, TUI, ...) can build on.
type StoreAPI interface {
	// Key returns the canonical lowercase store identifier (models.Store*).
	Key() string
	// Name returns a human-readable store name.
	Name() string
	// SignIn validates credentials with a live API call where possible.
	SignIn() error
	// GetOwnedGames returns every game the signed-in user owns.
	GetOwnedGames() ([]models.Game, error)
}

// ---------------------------------------------------------------------------
// Steam (working)
// ---------------------------------------------------------------------------

// SteamAPI implements StoreAPI for Steam via the Steam Web API.
// It needs a Web API key (https://steamcommunity.com/dev/apikey) and the
// account's SteamID64. The profile's "Game details" privacy must be public,
// otherwise Steam silently returns an empty library.
type SteamAPI struct {
	apiKey  string
	steamID string
	client  *api.Client
}

// NewSteamAPI builds a Steam integration.
func NewSteamAPI(apiKey, steamID string) *SteamAPI {
	return &SteamAPI{
		apiKey:  apiKey,
		steamID: steamID,
		// NOTE: Steam Web API auth travels in the query string, so the HTTP
		// client gets NO auth header (the old code leaked the key into a
		// bogus "Authorization: Bearer" header on every request).
		client: api.NewClient("https://api.steampowered.com", nil),
	}
}

func (s *SteamAPI) Key() string  { return models.StoreSteam }
func (s *SteamAPI) Name() string { return "Steam" }

// APIKey and SteamID expose the credentials so AuthService can persist them.
func (s *SteamAPI) APIKey() string  { return s.apiKey }
func (s *SteamAPI) SteamID() string { return s.steamID }

// SignIn validates the key + SteamID64 with a cheap live call.
func (s *SteamAPI) SignIn() error {
	if s.apiKey == "" || s.steamID == "" {
		return fmt.Errorf("steam: both apiKey and steamId must be set in configs/config.yaml")
	}
	var resp struct {
		Response struct {
			Players []struct {
				SteamID     string `json:"steamid"`
				PersonaName string `json:"personaname"`
			} `json:"players"`
		} `json:"response"`
	}
	path := fmt.Sprintf("/ISteamUser/GetPlayerSummaries/v2/?key=%s&steamids=%s&format=json", s.apiKey, s.steamID)
	if err := s.client.Request("GET", path, nil, &resp); err != nil {
		return fmt.Errorf("steam: credential check failed: %w", err)
	}
	if len(resp.Response.Players) == 0 {
		return fmt.Errorf("steam: no player found for steamId %s (wrong SteamID64, or the key does not belong to this account)", s.steamID)
	}
	return nil
}

// GetOwnedGames fetches the owned-games list.
func (s *SteamAPI) GetOwnedGames() ([]models.Game, error) {
	path := fmt.Sprintf(
		"/IPlayerService/GetOwnedGames/v1/?key=%s&steamid=%s&include_appinfo=true&include_played_free_games=true&format=json",
		s.apiKey, s.steamID)

	var resp struct {
		Response struct {
			GameCount int `json:"game_count"`
			Games     []struct {
				AppID int    `json:"appid"`
				Name  string `json:"name"`
			} `json:"games"`
		} `json:"response"`
	}
	if err := s.client.Request("GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("steam get owned games: %w", err)
	}
	if len(resp.Response.Games) == 0 {
		return nil, fmt.Errorf(
			"steam returned 0 games - verify the SteamID64 is correct and the profile's \"Game details\" privacy is set to public")
	}

	games := make([]models.Game, 0, len(resp.Response.Games))
	for _, g := range resp.Response.Games {
		if g.Name == "" {
			continue // unnamed entries (tools/dev apps) cannot be matched
		}
		games = append(games, models.Game{
			Title:       g.Name,
			OwnedStores: []string{models.StoreSteam},
			StoreIDs:    map[string]string{models.StoreSteam: fmt.Sprintf("%d", g.AppID)},
		})
	}
	return games, nil
}

// ---------------------------------------------------------------------------
// Epic Games Store (credentials validated; library listing needs user OAuth)
// ---------------------------------------------------------------------------

// EpicGamesAPI implements StoreAPI for the Epic Games Store.
//
// SignIn performs Epic's client-credentials token request (HTTP Basic auth +
// form body), which validates the App credentials but does NOT grant access
// to a user's library: listing owned games requires an end-user OAuth flow
// (authorization-code or device-code) that is not implemented yet.
type EpicGamesAPI struct {
	clientID     string
	clientSecret string
	accessToken  string
	expiresAt    int64
}

// NewEpicGamesAPI builds an Epic Games Store integration.
func NewEpicGamesAPI(clientID, clientSecret string) *EpicGamesAPI {
	return &EpicGamesAPI{clientID: clientID, clientSecret: clientSecret}
}

func (e *EpicGamesAPI) Key() string  { return models.StoreEpic }
func (e *EpicGamesAPI) Name() string { return models.DisplayStoreName(models.StoreEpic) }

// Token returns the cached client-credentials token and its unix expiry.
func (e *EpicGamesAPI) Token() (string, int64) { return e.accessToken, e.expiresAt }
func (e *EpicGamesAPI) ClientID() string       { return e.clientID }
func (e *EpicGamesAPI) ClientSecret() string   { return e.clientSecret }

// SignIn validates the App credentials with a live token request.
func (e *EpicGamesAPI) SignIn() error {
	if e.clientID == "" || e.clientSecret == "" {
		return fmt.Errorf("epic: both clientId and clientSecret must be set in configs/config.yaml")
	}
	creds := base64.StdEncoding.EncodeToString([]byte(e.clientID + ":" + e.clientSecret))
	c := api.NewClient("https://api.epicgames.dev", map[string]string{
		"Authorization": "Basic " + creds,
		"Content-Type":  "application/x-www-form-urlencoded", // OAuth token endpoints reject JSON bodies
	})
	data, _, err := c.Do("POST", "/epic/oauth/v1/token", strings.NewReader("grant_type=client_credentials"), nil)
	if err != nil {
		return fmt.Errorf("epic: token request failed: %w", err)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int64  `json:"expires_in"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("epic: token response decode: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("epic: credentials rejected: %s %s", tok.ErrorCode, tok.ErrorMessage)
	}
	e.accessToken = tok.AccessToken
	e.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	return nil
}

// GetOwnedGames is not implemented: client credentials cannot read a user's
// library; a device-code user flow is required.
func (e *EpicGamesAPI) GetOwnedGames() ([]models.Game, error) {
	return nil, fmt.Errorf("%w: listing an Epic library needs an end-user OAuth token (device-code flow); client credentials alone cannot read it", ErrNotImplemented)
}

// ---------------------------------------------------------------------------
// Battle.net (credentials validated; no unified library API exists)
// ---------------------------------------------------------------------------

// BattleNetAPI implements StoreAPI for Battle.net. SignIn validates Blizzard
// OAuth client credentials. Blizzard's public API exposes per-game account
// data but no unified "owned games" list, so GetOwnedGames is not implemented.
type BattleNetAPI struct {
	clientID     string
	clientSecret string
	accessToken  string
	expiresAt    int64
}

// NewBattleNetAPI builds a Battle.net integration.
func NewBattleNetAPI(clientID, clientSecret string) *BattleNetAPI {
	return &BattleNetAPI{clientID: clientID, clientSecret: clientSecret}
}

func (b *BattleNetAPI) Key() string  { return models.StoreBattleNet }
func (b *BattleNetAPI) Name() string { return models.DisplayStoreName(models.StoreBattleNet) }

// Token returns the cached OAuth token and its unix expiry.
func (b *BattleNetAPI) Token() (string, int64) { return b.accessToken, b.expiresAt }
func (b *BattleNetAPI) ClientID() string       { return b.clientID }
func (b *BattleNetAPI) ClientSecret() string   { return b.clientSecret }

// SignIn validates the credentials with a live token request.
func (b *BattleNetAPI) SignIn() error {
	if b.clientID == "" || b.clientSecret == "" {
		return fmt.Errorf("battlenet: both clientId and clientSecret must be set in configs/config.yaml (https://develop.battle.net/access/clients)")
	}
	c := api.NewClient("https://us.battle.net", nil)
	q := url.Values{}
	q.Set("client_id", b.clientID)
	q.Set("client_secret", b.clientSecret)
	q.Set("grant_type", "client_credentials")
	data, _, err := c.Do("POST", "/oauth/token?"+q.Encode(), nil, nil)
	if err != nil {
		return fmt.Errorf("battlenet: token request failed: %w", err)
	}
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("battlenet: token response decode: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("battlenet: credentials rejected: %s %s", tok.Error, tok.ErrorDescription)
	}
	b.accessToken = tok.AccessToken
	return nil
}

// GetOwnedGames is not implemented: Blizzard exposes per-title account data,
// not a unified library list.
func (b *BattleNetAPI) GetOwnedGames() ([]models.Game, error) {
	return nil, fmt.Errorf("%w: Blizzard's API exposes per-game account data but no unified owned-games list", ErrNotImplemented)
}

// ---------------------------------------------------------------------------
// Placeholder integrations (GOG, Ubisoft Connect, Xbox, DLsite)
// ---------------------------------------------------------------------------

// placeholderStore backs integrations whose APIs are undocumented, closed or
// nonexistent. `sync` and `signin` fail loudly with the reason instead of
// silently returning empty lists.
type placeholderStore struct {
	key         string
	displayName string
	reason      string
}

func (p *placeholderStore) Key() string  { return p.key }
func (p *placeholderStore) Name() string { return p.displayName }

func (p *placeholderStore) SignIn() error {
	return fmt.Errorf("%w: %s - %s", ErrNotImplemented, p.displayName, p.reason)
}

func (p *placeholderStore) GetOwnedGames() ([]models.Game, error) {
	return nil, fmt.Errorf("%w: %s - %s", ErrNotImplemented, p.displayName, p.reason)
}

// NewGOGAPI returns a GOG integration placeholder.
func NewGOGAPI() StoreAPI {
	return &placeholderStore{models.StoreGOG, "GOG",
		"GOG has no documented library API; the Galaxy client's embedded endpoints are unofficial and change without notice"}
}

// NewUbisoftAPI returns a Ubisoft Connect integration placeholder.
func NewUbisoftAPI() StoreAPI {
	return &placeholderStore{models.StoreUbisoft, "Ubisoft Connect",
		"Ubisoft Connect has no public API; library access requires tokens from the closed desktop client"}
}

// NewXboxAPI returns an Xbox integration placeholder.
func NewXboxAPI() StoreAPI {
	return &placeholderStore{models.StoreXbox, "Xbox",
		"Xbox library data sits behind Xbox Live XSTS authentication with no public user-library API"}
}

// NewDLsiteAPI returns a DLsite integration placeholder.
func NewDLsiteAPI() StoreAPI {
	return &placeholderStore{models.StoreDLsite, "DLsite",
		"DLsite has no public purchase/library API; support would require authenticated web scraping"}
}
