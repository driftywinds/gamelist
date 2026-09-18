package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gamelist/internal/models"
	"gamelist/pkg/api"
)

// ---------------------------------------------------------------------------
// GOG OAuth clients.
//
// GOG has no device-code grant. The established pattern (gogdl, Heroic,
// MiniGalaxy, Playnite) is the authorization-code flow with GOG's own Galaxy
// client credentials and the registered embed redirect: the user logs in via
// the browser, lands on https://embed.gog.com/on_login_success?...&code=...,
// and pastes that URL (or just the code) back into the CLI. These client
// credentials are GOG's own, hardcoded in every open-source GOG tool; this is
// an unofficial API usage.
// ---------------------------------------------------------------------------

const (
	gogClientID     = "46899977096215655"
	gogClientSecret = "9d85c43b1482497dbbce61f6e4aa173a433796eeae2ca8c5f6129f2dc4de46d9"
	gogRedirectURI  = "https://embed.gog.com/on_login_success?origin=client"

	// A locale cookie avoids Cloudflare geo challenges on www.gog.com
	// endpoints (same workaround MiniGalaxy uses).
	gogLocaleCookie = "gog_lc=US_USD_en-US"

	gogUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) GOGGalaxyClient/2.0"
)

// gogEndpoints bundles every GOG URL. Defaults point at production; tests
// replace them with a stub server.
type gogEndpoints struct {
	authorizeURL func(clientID string) string
	tokenURL     string
	userDataURL  string
	ownedURL     func(page int) string
}

func gogDefaultEndpoints() gogEndpoints {
	return gogEndpoints{
		authorizeURL: func(clientID string) string {
			q := url.Values{}
			q.Set("client_id", clientID)
			q.Set("redirect_uri", gogRedirectURI)
			q.Set("response_type", "code")
			q.Set("layout", "client2")
			return "https://auth.gog.com/auth?" + q.Encode()
		},
		tokenURL:    "https://auth.gog.com/token",
		userDataURL: "https://embed.gog.com/userData.json",
		ownedURL: func(page int) string {
			return fmt.Sprintf("https://www.gog.com/account/getFilteredProducts?mediaType=1&sortBy=title&page=%d", page)
		},
	}
}

// GOGTokens is a snapshot of a GOG user session.
type GOGTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Username     string
}

// GOGTokenHooks persist GOG sessions between runs (wired to SQLite by the CLI).
type GOGTokenHooks struct {
	Load      func() *GOGTokens
	Save      func(GOGTokens)
	ReadInput func() (string, error) // interactive code paste; wired to stdin
}

// GOGTokenSource manages GOG sign-in state: authorization-code exchange,
// silent refresh, and persistence via hooks.
type GOGTokenSource struct {
	ClientID     string
	ClientSecret string

	hooks *GOGTokenHooks
	ep    gogEndpoints

	mu  sync.Mutex
	now GOGTokens
}

// NewGOGTokenSource creates a source with the built-in Galaxy client. Empty
// clientID/secret keep the built-ins; non-empty values override them.
func NewGOGTokenSource(clientID, clientSecret string, hooks *GOGTokenHooks) *GOGTokenSource {
	if clientID == "" {
		clientID = gogClientID
		clientSecret = gogClientSecret
	}
	return &GOGTokenSource{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		hooks:        hooks,
		ep:           gogDefaultEndpoints(),
	}
}

// AuthorizeURL returns the browser login URL for the authorization-code flow.
func (t *GOGTokenSource) AuthorizeURL() string {
	return t.ep.authorizeURL(t.ClientID)
}

// Tokens returns a valid session, refreshing it when needed.
func (t *GOGTokenSource) Tokens() (GOGTokens, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if t.now.AccessToken != "" && now.Before(t.now.ExpiresAt.Add(-time.Minute)) {
		return t.now, nil
	}
	if t.hooks != nil && t.hooks.Load != nil {
		if tok := t.hooks.Load(); tok != nil && tok.AccessToken != "" &&
			now.Before(tok.ExpiresAt.Add(-time.Minute)) {
			t.now = *tok
			return t.now, nil
		}
	}

	// Expired: refresh.
	if t.now.RefreshToken == "" && t.hooks != nil && t.hooks.Load != nil {
		if tok := t.hooks.Load(); tok != nil {
			t.now = *tok
		}
	}
	if t.now.RefreshToken == "" {
		return GOGTokens{}, fmt.Errorf("no GOG session stored - run: gamelist config signin gog")
	}

	sess, err := t.tokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.now.RefreshToken},
		"client_id":     {t.ClientID},
		"client_secret": {t.ClientSecret},
	})
	if err != nil {
		return GOGTokens{}, fmt.Errorf("GOG refresh failed (sign in again: gamelist config signin gog): %w", err)
	}
	t.now = sess
	if t.hooks != nil && t.hooks.Save != nil {
		t.hooks.Save(sess)
	}
	return t.now, nil
}

// ExchangeCode swaps an authorization code (pasted from the browser redirect)
// for a full session. Interactive paste input comes from the ReadInput hook.
func (t *GOGTokenSource) SignIn() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	loginURL := t.AuthorizeURL()
	fmt.Printf("\nGOG sign-in:\n  1. Open this URL (a browser window should open automatically):\n     %s\n  2. Log in to GOG.\n", loginURL)
	openBrowser(loginURL)
	fmt.Println("  3. After logging in the browser lands on a page whose URL looks like:")
	fmt.Println("     https://embed.gog.com/on_login_success?origin=client&code=XXXXXXXX")
	fmt.Print("  4. Paste that full URL (or just the code) here: ")

	if t.hooks == nil || t.hooks.ReadInput == nil {
		return fmt.Errorf("no input source configured for the GOG sign-in code")
	}
	line, err := t.hooks.ReadInput()
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("read pasted code: %w", err)
	}
	code := extractGOGCode(line)
	if code == "" {
		return fmt.Errorf("no authorization code found in %q", strings.TrimSpace(line))
	}

	sess, err := t.tokenRequest(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {t.ClientID},
		"client_secret": {t.ClientSecret},
		"redirect_uri":  {gogRedirectURI},
	})
	if err != nil {
		return fmt.Errorf("GOG code exchange failed (nothing was saved): %w", err)
	}
	t.now = sess
	t.fillUsernameLocked()
	if t.hooks != nil && t.hooks.Save != nil {
		t.hooks.Save(t.now)
	}
	fmt.Printf("  Authorized as %s.\n", t.now.Username)
	return nil
}

// extractGOGCode accepts either the full redirect URL or a bare code.
func extractGOGCode(paste string) string {
	paste = strings.TrimSpace(paste)
	if u, err := url.Parse(paste); err == nil {
		if code := u.Query().Get("code"); code != "" {
			return code
		}
	}
	return paste
}

// Snapshot exposes the current session as stored (no refresh; used by
// credential persistence right after sign-in).
func (t *GOGTokenSource) Snapshot() GOGTokens {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.now.AccessToken == "" && t.hooks != nil && t.hooks.Load != nil {
		if tok := t.hooks.Load(); tok != nil {
			t.now = *tok
		}
	}
	return t.now
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

type gogTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// tokenRequest performs one auth.gog.com/token grant. GOG takes the client
// credentials as form fields (no Basic auth header).
func (t *GOGTokenSource) tokenRequest(form url.Values) (GOGTokens, error) {
	extra := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	data, _, err := t.client().Do("POST", t.ep.tokenURL, strings.NewReader(form.Encode()), extra)
	if err != nil {
		return GOGTokens{}, err
	}
	var tok gogTokenResponse
	if err := json.Unmarshal(data, &tok); err != nil {
		return GOGTokens{}, fmt.Errorf("decode response: %w (body: %.200s)", err, string(data))
	}
	if tok.AccessToken == "" {
		return GOGTokens{}, fmt.Errorf("%s: %s", tok.Error, tok.ErrorDesc)
	}
	return GOGTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second),
	}, nil
}

// fillUsernameLocked best-effort fetches the account's username.
func (t *GOGTokenSource) fillUsernameLocked() {
	if t.now.Username != "" || t.now.AccessToken == "" {
		return
	}
	data, _, err := t.client().Do("GET", t.ep.userDataURL, nil, map[string]string{
		"Authorization": "Bearer " + t.now.AccessToken,
	})
	if err != nil {
		return
	}
	var v struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(data, &v) == nil && v.Username != "" {
		t.now.Username = v.Username
	}
}

func (t *GOGTokenSource) client() *api.Client {
	return api.NewClient("", map[string]string{
		"User-Agent": gogUserAgent,
		"Cookie":     gogLocaleCookie,
	})
}

// ---------------------------------------------------------------------------
// GOGAPI - owned games
// ---------------------------------------------------------------------------

// GOGAPI implements StoreAPI for GOG. The owned list comes from the account
// getFilteredProducts endpoint (the same data the GOG library page shows),
// which accepts the OAuth bearer token.
type GOGAPI struct {
	tokens *GOGTokenSource
}

// NewGOGAPI builds a GOG integration. clientID/secret override the built-in
// Galaxy client when non-empty; hooks persist sessions and provide paste input.
func NewGOGAPI(clientID, clientSecret string, hooks *GOGTokenHooks) *GOGAPI {
	return &GOGAPI{tokens: NewGOGTokenSource(clientID, clientSecret, hooks)}
}

func (g *GOGAPI) Key() string  { return models.StoreGOG }
func (g *GOGAPI) Name() string { return models.DisplayStoreName(models.StoreGOG) }

// SignIn runs the interactive authorization-code login and stores the session.
func (g *GOGAPI) SignIn() error {
	return g.tokens.SignIn()
}

// Tokens exposes the current session, refreshing it when needed.
func (g *GOGAPI) Tokens() (GOGTokens, error) { return g.tokens.Tokens() }

// Snapshot exposes the stored session without refreshing (for persistence
// right after sign-in).
func (g *GOGAPI) Snapshot() GOGTokens { return g.tokens.Snapshot() }

// GetOwnedGames returns every game (mediaType=1) in the signed-in account.
func (g *GOGAPI) GetOwnedGames() ([]models.Game, error) {
	tok, err := g.tokens.Tokens()
	if err != nil {
		return nil, fmt.Errorf("gog get owned games: %w", err)
	}

	type gogProduct struct {
		ID            int64  `json:"id"`
		Title         string `json:"title"`
		IsGame        *bool  `json:"isGame"`
		IsGameSnake   *bool  `json:"is_game"`
		IsInstallable *bool  `json:"isInstallable"`
	}
	type gogOwnedPage struct {
		Products    []gogProduct `json:"products"`
		Page        int          `json:"page"`
		TotalPages  int          `json:"totalPages"`
		TotalPagesS int          `json:"total_pages"`
	}

	seen := make(map[int64]bool)
	games := make([]models.Game, 0)
	totalPages := 1
	for page := 1; page <= 200 && page <= totalPages; page++ {
		data, _, err := g.httpClient().Do("GET", g.tokens.ep.ownedURL(page), nil,
			map[string]string{"Authorization": "Bearer " + tok.AccessToken})
		if err != nil {
			return nil, fmt.Errorf("gog owned games (page %d): %w", page, err)
		}
		var result gogOwnedPage
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("gog owned games decode: %w (body: %.200s)", err, string(data))
		}
		for _, p := range result.Products {
			// Skip only explicit non-games (DLC etc.); absent flags mean the
			// field naming changed - keep the product rather than dropping it.
			if (p.IsGame != nil && !*p.IsGame) || (p.IsGameSnake != nil && !*p.IsGameSnake) {
				continue
			}
			if p.ID == 0 || p.Title == "" || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			games = append(games, models.Game{
				Title:       p.Title,
				OwnedStores: []string{models.StoreGOG},
				StoreIDs:    map[string]string{models.StoreGOG: strconv.FormatInt(p.ID, 10)},
			})
		}
		log.Printf("gog library: page %d - %d product(s) (%d games so far)", page, len(result.Products), len(games))

		totalPages = result.TotalPages
		if totalPages == 0 {
			totalPages = result.TotalPagesS
		}
		if len(result.Products) == 0 {
			break
		}
	}

	sort.Slice(games, func(i, j int) bool { return games[i].Title < games[j].Title })
	return games, nil
}

func (g *GOGAPI) httpClient() *api.Client {
	return api.NewClient("", map[string]string{
		"User-Agent": gogUserAgent,
		"Cookie":     gogLocaleCookie,
	})
}
