package api

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"gamelist/internal/models"
	"gamelist/pkg/api"
)

const igdbBaseURL = "https://api.igdb.com/v4"

// ExternalGameSourceSteam is the IGDB external_game_sources ID for Steam.
// It lets us match a Steam appid to an IGDB game exactly instead of relying
// on fuzzy title search.
const ExternalGameSourceSteam = 1

// IGDBClient talks to the IGDB API v4 with an auto-managed Twitch OAuth token.
type IGDBClient struct {
	client *api.Client
	tokens *TokenSource

	// IGDB rate limit: 4 requests/second, max 8 open requests. We throttle
	// to one request every minDelay.
	mu       sync.Mutex
	lastReq  time.Time
	minDelay time.Duration
}

// NewIGDBClient creates a client pointed at the real IGDB API.
func NewIGDBClient(tokens *TokenSource) *IGDBClient {
	return NewIGDBClientWithBaseURL(igdbBaseURL, tokens)
}

// NewIGDBClientWithBaseURL lets tests point the client at a stub server.
func NewIGDBClientWithBaseURL(baseURL string, tokens *TokenSource) *IGDBClient {
	return &IGDBClient{
		client:   api.NewClient(baseURL, nil),
		tokens:   tokens,
		minDelay: 250 * time.Millisecond,
	}
}

// query throttles, attaches auth headers, and runs one Apicalypse query.
func (c *IGDBClient) query(endpoint, body string, dst any) error {
	tok, err := c.tokens.Token()
	if err != nil {
		return fmt.Errorf("IGDB auth: %w", err)
	}

	c.mu.Lock()
	if wait := c.minDelay - time.Since(c.lastReq); wait > 0 {
		time.Sleep(wait)
	}
	c.lastReq = time.Now()
	c.mu.Unlock()

	c.client.SetHeader("Client-ID", c.tokens.ClientID)
	c.client.SetHeader("Authorization", "Bearer "+tok)
	return c.client.Request("POST", endpoint, strings.NewReader(body), dst)
}

// SearchGames searches IGDB by title, returning up to 10 matches.
func (c *IGDBClient) SearchGames(query string) ([]models.Game, error) {
	body := fmt.Sprintf(
		`search "%s"; fields name,id,first_release_date,summary; limit 10;`,
		escapeIGDBString(query))
	var results []igdbGameResult
	if err := c.query("/games", body, &results); err != nil {
		return nil, fmt.Errorf("IGDB search %q: %w", query, err)
	}
	games := make([]models.Game, 0, len(results))
	for _, r := range results {
		games = append(games, models.Game{
			ID:          r.ID,
			Title:       r.Name,
			Description: r.Summary,
			ReleaseDate: igdbTimestampToDate(r.FirstReleaseDate),
		})
	}
	return games, nil
}

// FindGameByExternalID resolves an IGDB game ID from a store-specific ID
// (currently Steam appids) via the external_games endpoint. Returns 0 when
// nothing matches.
func (c *IGDBClient) FindGameByExternalID(source int, uid string) (int, error) {
	body := fmt.Sprintf(
		`fields game; where external_game_source = %d & uid = "%s"; limit 1;`,
		source, escapeIGDBString(uid))
	var results []struct {
		Game int `json:"game"`
	}
	if err := c.query("/external_games", body, &results); err != nil {
		return 0, fmt.Errorf("IGDB external_games lookup (source %d, uid %q): %w", source, uid, err)
	}
	if len(results) == 0 {
		return 0, nil
	}
	return results[0].Game, nil
}

// GetGameDetails fetches full details for one IGDB game ID.
//
// NOTE: the v4 games endpoint has no dedicated "developers"/"publishers"
// fields (they were removed in the v3->v4 migration); developer and publisher
// flags live on involved_companies, so we request those instead.
func (c *IGDBClient) GetGameDetails(igdbID int) (*models.Game, error) {
	body := fmt.Sprintf(
		`fields name,summary,first_release_date,involved_companies.company.name,`+
			`involved_companies.developer,involved_companies.publisher,platforms.name; where id = %d;`,
		igdbID)

	var results []igdbGameDetailResult
	if err := c.query("/games", body, &results); err != nil {
		return nil, fmt.Errorf("IGDB details for ID %d: %w", igdbID, err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no IGDB game with ID %d", igdbID)
	}

	d := results[0]
	game := &models.Game{
		ID:          d.ID,
		Title:       d.Name,
		Description: d.Summary,
		ReleaseDate: igdbTimestampToDate(d.FirstReleaseDate),
	}
	for _, ic := range d.InvolvedCompanies {
		if ic.Developer {
			game.Developers = append(game.Developers, ic.Company.Name)
		}
		if ic.Publisher {
			game.Publishers = append(game.Publishers, ic.Company.Name)
		}
	}
	for _, p := range d.Platforms {
		game.Platforms = append(game.Platforms, p.Name)
	}
	return game, nil
}

// --- IGDB response types ---

type igdbGameResult struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	Summary          string `json:"summary"`
	FirstReleaseDate int64  `json:"first_release_date"`
}

type igdbGameDetailResult struct {
	ID                int                   `json:"id"`
	Name              string                `json:"name"`
	Summary           string                `json:"summary"`
	FirstReleaseDate  int64                 `json:"first_release_date"`
	InvolvedCompanies []igdbInvolvedCompany `json:"involved_companies"`
	Platforms         []igdbPlatform        `json:"platforms"`
}

type igdbCompany struct {
	Name string `json:"name"`
}

type igdbInvolvedCompany struct {
	Company   igdbCompany `json:"company"`
	Developer bool        `json:"developer"`
	Publisher bool        `json:"publisher"`
}

type igdbPlatform struct {
	Name string `json:"name"`
}

// --- helpers ---

// igdbTimestampToDate renders a UTC unix timestamp as YYYY-MM-DD. IGDB
// timestamps are UTC midnight; formatting in local time shifted the date a
// day early in UTC-negative timezones.
func igdbTimestampToDate(ts int64) string {
	if ts == 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}

func escapeIGDBString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
