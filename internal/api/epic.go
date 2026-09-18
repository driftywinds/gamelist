package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gamelist/internal/models"
	"gamelist/pkg/api"
)

// ---------------------------------------------------------------------------
// Epic Games OAuth clients.
//
// Epic's library APIs require a user session issued to a client with
// launcher-grade permissions, but only certain clients have the device-code
// grant enabled. The established pattern (used by open-source Epic launchers)
// is:
//  1. device-code login with the Switch activation client
//  2. exchange-code hop to the launcher client (launcherAppClient2)
//  3. refresh the launcher session directly afterwards
//
// These are Epic's own public client credentials (also hardcoded in every
// open-source Epic launcher); this is an unofficial API usage.
// ---------------------------------------------------------------------------

const (
	epicDeviceClientID   = "98f7e42c2e3a4f86a74eb43fbb41ed39" // fortniteNewSwitchGameClient (device-code grant)
	epicDeviceSecret     = "0a2449a2-001a-451e-afec-3e812901c4d7"
	epicLauncherClientID = "34a02cf8f4414e29b15921876da36f9a" // launcherAppClient2 (library permissions)
	epicLauncherSecret   = "daafbccc737745039dffe53d94fc76cf"

	epicUserAgent = "UELauncher/11.0.1-14907503+++Portal+Release-Live Windows/10.0.19041.1.256.64bit"
)

// epicEndpoints bundles every Epic URL used by this client. Defaults point at
// the production hosts; tests replace them with a stub server.
type epicEndpoints struct {
	tokenURL      string // POST: oauth token (all grants)
	deviceAuthURL string // POST: start device-code authorization
	exchangeURL   string // GET:  exchange code for the current session
	verifyURL     string // GET:  session identity (display name etc.)
	// libraryURL returns one page of the user's EGS library records
	// (the modern Epic Games Store library service).
	libraryURL func(cursor string) string
	// catalogURL returns bulk catalog items for one item in a namespace.
	catalogURL func(namespace, itemID string) string
}

func epicDefaultEndpoints() epicEndpoints {
	const libraryHost = "https://library-service.live.use1a.on.epicgames.com"
	return epicEndpoints{
		tokenURL:      "https://account-public-service-prod03.ol.epicgames.com/account/api/oauth/token",
		deviceAuthURL: "https://account-public-service-prod03.ol.epicgames.com/account/api/oauth/deviceAuthorization",
		exchangeURL:   "https://account-public-service-prod03.ol.epicgames.com/account/api/oauth/exchange",
		verifyURL:     "https://account-public-service-prod03.ol.epicgames.com/account/api/oauth/verify",
		libraryURL: func(cursor string) string {
			if cursor == "" {
				return libraryHost + "/library/api/public/items?includeMetadata=true"
			}
			return libraryHost + "/library/api/public/items?includeMetadata=true&cursor=" + url.QueryEscape(cursor)
		},
		catalogURL: func(namespace, itemID string) string {
			return fmt.Sprintf("https://catalog-public-service-prod06.ol.epicgames.com/catalog/api/shared/namespace/%s/bulk/items?id=%s&country=US&locale=en", namespace, url.QueryEscape(itemID))
		},
	}
}

// EpicTokens is a snapshot of an Epic user session.
type EpicTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	AccountID    string
	DisplayName  string
}

// EpicTokenHooks persist Epic sessions between runs (wired to the SQLite
// store_credentials table by the CLI).
type EpicTokenHooks struct {
	Load func() *EpicTokens // nil = nothing stored
	Save func(EpicTokens)
}

// EpicTokenSource manages Epic sign-in state: device-code login, silent
// refresh of the launcher session, and persistence via hooks.
type EpicTokenSource struct {
	DeviceClientID     string
	DeviceClientSecret string

	hooks *EpicTokenHooks
	ep    epicEndpoints
	// pollInterval overrides the deviceAuthorization interval (tests).
	pollInterval time.Duration

	mu  sync.Mutex
	now EpicTokens
}

// NewEpicTokenSource creates a source with the built-in clients. Empty
// clientID/clientSecret arguments keep the built-ins; non-empty values
// override them (advanced use).
func NewEpicTokenSource(deviceClientID, deviceClientSecret string, hooks *EpicTokenHooks) *EpicTokenSource {
	if deviceClientID == "" {
		deviceClientID = epicDeviceClientID
		deviceClientSecret = epicDeviceSecret
	}
	return &EpicTokenSource{
		DeviceClientID:     deviceClientID,
		DeviceClientSecret: deviceClientSecret,
		hooks:              hooks,
		ep:                 epicDefaultEndpoints(),
	}
}

// Tokens returns a valid launcher session, refreshing it when needed.
func (t *EpicTokenSource) Tokens() (EpicTokens, error) {
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
			if t.fillIdentityLocked() && t.hooks.Save != nil {
				t.hooks.Save(t.now) // persist the backfilled identity
			}
			return t.now, nil
		}
	}

	// Expired: try refreshing the launcher session.
	if t.now.RefreshToken == "" && t.hooks != nil && t.hooks.Load != nil {
		if tok := t.hooks.Load(); tok != nil {
			t.now = *tok
		}
	}
	if t.now.RefreshToken == "" {
		return EpicTokens{}, fmt.Errorf("no Epic session stored - run: gamelist config signin epic")
	}

	sess, err := t.oauthToken(t.launcherBasic(), url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.now.RefreshToken},
		"token_type":    {"eg1"},
	}, "")
	if err != nil {
		return EpicTokens{}, fmt.Errorf("Epic refresh failed (sign in again: gamelist config signin epic): %w", err)
	}
	t.now = sess
	if t.hooks != nil && t.hooks.Save != nil {
		t.hooks.Save(sess)
	}
	return t.now, nil
}

// DeviceLogin runs the interactive device-code flow:
// switch-client session -> exchange code -> launcher session.
// The user authorizes in a browser; this call blocks until done.
func (t *EpicTokenSource) DeviceLogin(openBrowser func(string)) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Anonymous app token for the device client.
	app, err := t.oauthToken(t.deviceBasic(), url.Values{
		"grant_type": {"client_credentials"},
		"token_type": {"eg1"},
	}, "")
	if err != nil {
		return fmt.Errorf("Epic app token: %w", err)
	}

	// 2. Device authorization challenge.
	var da epicDeviceAuthResponse
	if err := t.postForm(t.ep.deviceAuthURL, "", app.AccessToken,
		url.Values{"prompt": {"login"}}, &da); err != nil {
		return fmt.Errorf("Epic device authorization: %w", err)
	}
	if da.DeviceCode == "" {
		return fmt.Errorf("Epic device authorization returned no device code (%s %s)", da.ErrorCode, da.ErrorMessage)
	}

	// 3. Send the user to the browser.
	visit := da.VerificationURIComplete
	if visit == "" {
		visit = da.VerificationURI
	}
	fmt.Printf("\nEpic Games sign-in:\n  1. Open this URL (a browser window should open automatically):\n     %s\n  2. Log in and approve the request.\n", visit)
	if openBrowser != nil {
		openBrowser(visit)
	}
	fmt.Printf("  Waiting for authorization (code %s, valid for ~%d minutes)...\n", da.UserCode, da.ExpiresIn/60)

	// 4. Poll for the user session (switch client).
	interval := time.Duration(da.Interval) * time.Second
	switch {
	case t.pollInterval > 0:
		interval = t.pollInterval
	case interval < 5*time.Second:
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	if da.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for {
		time.Sleep(interval)
		if time.Now().After(deadline) {
			return fmt.Errorf("Epic device code expired before authorization completed")
		}
		sess, perr := t.oauthToken(t.deviceBasic(), url.Values{
			"grant_type":  {"device_code"},
			"device_code": {da.DeviceCode},
			"token_type":  {"eg1"},
		}, "")
		if perr != nil {
			msg := perr.Error()
			switch {
			case strings.Contains(msg, "slow_down"):
				interval += 5 * time.Second
				continue
			case strings.Contains(msg, "expired"):
				return fmt.Errorf("Epic device code expired before authorization completed")
			default:
				continue // pending / transient; keep polling
			}
		}

		// 5. Hop to the launcher client via an exchange code (the switch
		// session cannot access the library endpoints).
		exchange, err := t.exchangeCode(sess.AccessToken)
		if err != nil {
			return fmt.Errorf("Epic exchange code: %w", err)
		}
		launcherSess, err := t.oauthToken(t.launcherBasic(), url.Values{
			"grant_type":    {"exchange_code"},
			"exchange_code": {exchange},
			"token_type":    {"eg1"},
		}, "")
		if err != nil {
			return fmt.Errorf("Epic launcher session: %w", err)
		}
		if launcherSess.AccountID == "" {
			launcherSess.AccountID = sess.AccountID
		}
		t.now = launcherSess
		t.fillIdentityLocked()
		if t.hooks != nil && t.hooks.Save != nil {
			t.hooks.Save(t.now)
		}
		fmt.Printf("  Authorized as %s.\n", t.now.DisplayName)
		return nil
	}
}

// AccountID returns the account id of the cached session ("" when signed out).
func (t *EpicTokenSource) AccountID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now.AccountID
}

// fillIdentityLocked backfills the display name (and account id) from the
// verify endpoint when the token response omitted them - some grant types do.
// Callers must hold t.mu. Returns true when anything changed; callers are
// responsible for persisting. Cosmetic failures are ignored.
func (t *EpicTokenSource) fillIdentityLocked() bool {
	if t.now.DisplayName != "" || t.now.AccessToken == "" {
		return false
	}
	data, _, err := t.client().Do("GET", t.ep.verifyURL, nil, map[string]string{
		"Authorization": "Bearer " + t.now.AccessToken,
	})
	if err != nil {
		return false
	}
	var v struct {
		DisplayName string `json:"display_name"`
		AccountID   string `json:"account_id"`
	}
	if json.Unmarshal(data, &v) != nil {
		return false
	}
	changed := false
	if t.now.DisplayName == "" && v.DisplayName != "" {
		t.now.DisplayName = v.DisplayName
		changed = true
	}
	if t.now.AccountID == "" && v.AccountID != "" {
		t.now.AccountID = v.AccountID
		changed = true
	}
	return changed
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

type epicTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in_seconds"`
	AccountID    string `json:"account_id"`
	DisplayName  string `json:"display_name"`
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
	// Some Epic responses use the generic OAuth field name instead.
	ExpiresInGeneric int64 `json:"expires_in"`
}

type epicDeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	ErrorCode               string `json:"errorCode"`
	ErrorMessage            string `json:"errorMessage"`
}

func (t *EpicTokenSource) deviceBasic() string {
	return epicBasicAuth(t.DeviceClientID, t.DeviceClientSecret)
}

func (t *EpicTokenSource) launcherBasic() string {
	return epicBasicAuth(epicLauncherClientID, epicLauncherSecret)
}

func epicBasicAuth(id, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(id + ":" + secret))
}

// oauthToken performs one token-endpoint grant. bearer is optional (used by
// deviceAuthorization); basic carries the client credentials.
func (t *EpicTokenSource) oauthToken(basic string, form url.Values, bearer string) (EpicTokens, error) {
	var tok epicTokenResponse
	if err := t.postForm(t.ep.tokenURL, basic, bearer, form, &tok); err != nil {
		return EpicTokens{}, err
	}
	if tok.AccessToken == "" {
		return EpicTokens{}, fmt.Errorf("%s: %s", tok.ErrorCode, tok.ErrorMessage)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn == 0 {
		expiresIn = tok.ExpiresInGeneric
	}
	return EpicTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
		AccountID:    tok.AccountID,
		DisplayName:  tok.DisplayName,
	}, nil
}

// postForm POSTs a urlencoded form with optional Basic/Bearer auth and
// decodes the JSON response. Non-2xx responses surface Epic's errorCode.
func (t *EpicTokenSource) postForm(rawURL, basic, bearer string, form url.Values, dst any) error {
	extra := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if basic != "" {
		extra["Authorization"] = "Basic " + basic
	}
	if bearer != "" {
		extra["Authorization"] = "Bearer " + bearer
	}
	data, _, err := t.client().Do("POST", rawURL, strings.NewReader(form.Encode()), extra)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode response: %w (body: %.200s)", err, string(data))
	}
	return nil
}

func (t *EpicTokenSource) client() *api.Client {
	return api.NewClient("", map[string]string{"User-Agent": epicUserAgent})
}

// exchangeCode mints an exchange code for the current user session.
func (t *EpicTokenSource) exchangeCode(accessToken string) (string, error) {
	data, _, err := t.client().Do("GET", t.ep.exchangeURL, nil, map[string]string{
		"Authorization": "Bearer " + accessToken,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Code         string `json:"code"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if resp.Code == "" {
		return "", fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMessage)
	}
	return resp.Code, nil
}

// openBrowser best-effort opens a URL in the system browser.
func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	_ = cmd.Start()
}

// ---------------------------------------------------------------------------
// EpicGamesAPI - owned games
// ---------------------------------------------------------------------------

// EpicGamesAPI implements StoreAPI for the Epic Games Store using the
// device-code user session. Owned games = public launchable assets that the
// account holds an entitlement for; titles come from the catalog service.
type EpicGamesAPI struct {
	tokens *EpicTokenSource
}

// NewEpicGamesAPI builds an Epic integration. deviceClientID/secret override
// the built-in device-login client when non-empty; hooks persist sessions.
func NewEpicGamesAPI(deviceClientID, deviceClientSecret string, hooks *EpicTokenHooks) *EpicGamesAPI {
	return &EpicGamesAPI{tokens: NewEpicTokenSource(deviceClientID, deviceClientSecret, hooks)}
}

func (e *EpicGamesAPI) Key() string  { return models.StoreEpic }
func (e *EpicGamesAPI) Name() string { return models.DisplayStoreName(models.StoreEpic) }

// SignIn runs the interactive device-code login and stores the session.
func (e *EpicGamesAPI) SignIn() error {
	return e.tokens.DeviceLogin(openBrowser)
}

// Tokens exposes the current session snapshot (for credential persistence).
func (e *EpicGamesAPI) Tokens() (EpicTokens, error) { return e.tokens.Tokens() }

// GetOwnedGames returns every game in the signed-in account's EGS library.
//
// Source of truth is the modern EGS library service (the same data the Epic
// Games Launcher's Library tab shows). The legacy entitlements endpoint
// returns nothing for accounts whose purchases live in the new system, and
// the launcher assets endpoint only covers Unreal Marketplace apps - neither
// is usable for owned games.
func (e *EpicGamesAPI) GetOwnedGames() ([]models.Game, error) {
	tok, err := e.tokens.Tokens()
	if err != nil {
		return nil, fmt.Errorf("epic get owned games: %w", err)
	}

	// 1. Library records (cursor-paginated, ~100 per page).
	//
	// NOTE: Epic keeps returning a non-empty nextCursor (an advancing offset)
	// even on the page AFTER the last one, which contains zero records. The
	// loop therefore terminates on an empty page, a repeated cursor, or a
	// hard page cap - trusting the cursor alone spins forever.
	type libraryRecord struct {
		AppName       string   `json:"appName"`
		CatalogItemID string   `json:"catalogItemId"`
		Namespace     string   `json:"namespace"`
		SandboxName   string   `json:"sandboxName"`
		RecordType    string   `json:"recordType"`
		Platform      []string `json:"platform"`
	}
	var records []libraryRecord
	cursor := ""
	seenCursors := map[string]bool{"": true}
	for pages := 0; pages < 200; pages++ {
		data, _, err := e.httpClient().Do("GET", e.tokens.ep.libraryURL(cursor), nil,
			map[string]string{"Authorization": "Bearer " + tok.AccessToken})
		if err != nil {
			return nil, fmt.Errorf("epic library (cursor %q): %w", cursor, err)
		}
		var page struct {
			Records          []libraryRecord `json:"records"`
			ResponseMetadata struct {
				NextCursor string `json:"nextCursor"`
			} `json:"responseMetadata"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("epic library decode: %w", err)
		}
		records = append(records, page.Records...)
		log.Printf("epic library: %d record(s) fetched so far", len(records))
		next := page.ResponseMetadata.NextCursor
		if next == "" || seenCursors[next] || len(page.Records) == 0 {
			break
		}
		seenCursors[next] = true
		cursor = next
	}

	// 2. Keep games only: APPLICATION records for Windows, skipping the
	// Unreal Engine namespace (engine + Marketplace items).
	type ownedApp struct {
		appName, namespace, itemID, sandboxName string
	}
	var apps []ownedApp
	seen := make(map[string]bool) // one entry per app name
	for _, r := range records {
		if r.RecordType != "APPLICATION" || r.AppName == "" || r.CatalogItemID == "" {
			continue
		}
		if r.Namespace == "ue" {
			continue // Unreal Engine / Marketplace
		}
		if len(r.Platform) > 0 && !slices.Contains(r.Platform, "Windows") {
			continue
		}
		if seen[r.AppName] {
			continue
		}
		seen[r.AppName] = true
		apps = append(apps, ownedApp{r.AppName, r.Namespace, r.CatalogItemID, r.SandboxName})
	}
	log.Printf("epic library: %d game record(s) after filtering", len(apps))

	// 3. Resolve display titles from the catalog service (concurrently);
	// sandboxName is the fallback when the catalog has no title.
	titles := make([]string, len(apps))
	var wg sync.WaitGroup
	var completed atomic.Int32
	sem := make(chan struct{}, 8)
	for i, app := range apps {
		wg.Add(1)
		go func(i int, app ownedApp) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, _, err := e.httpClient().Do("GET", e.tokens.ep.catalogURL(app.namespace, app.itemID), nil,
				map[string]string{"Authorization": "Bearer " + tok.AccessToken})
			if err != nil {
				log.Printf("epic catalog lookup for %s: %v", app.appName, err)
			} else {
				var items map[string]struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(data, &items); err != nil {
					log.Printf("epic catalog decode for %s: %v", app.appName, err)
				} else if item, ok := items[app.itemID]; ok {
					titles[i] = item.Title
				}
			}
			if n := completed.Add(1); n%25 == 0 || int(n) == len(apps) {
				log.Printf("epic catalog: %d/%d title(s) resolved", n, len(apps))
			}
		}(i, app)
	}
	wg.Wait()

	games := make([]models.Game, 0, len(apps))
	for i, app := range apps {
		title := titles[i]
		if title == "" {
			title = app.sandboxName
		}
		if title == "" {
			title = app.appName // last resort: the internal app name
		}
		games = append(games, models.Game{
			Title:       title,
			OwnedStores: []string{models.StoreEpic},
			StoreIDs:    map[string]string{models.StoreEpic: app.appName},
		})
	}
	sort.Slice(games, func(i, j int) bool { return games[i].Title < games[j].Title })
	return games, nil
}

func (e *EpicGamesAPI) httpClient() *api.Client {
	return api.NewClient("", map[string]string{"User-Agent": epicUserAgent})
}
