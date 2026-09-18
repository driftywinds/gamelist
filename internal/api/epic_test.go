package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gamelist/internal/models"
)

// epicStubRoutes counts device_code polls so the stub can answer "pending"
// before succeeding.
type epicStubRoutes struct {
	mu          sync.Mutex
	devicePolls int
}

// newEpicStubServer implements every Epic endpoint used by the client.
func newEpicStubServer(t *testing.T, r *epicStubRoutes) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/account/api/oauth/token"):
			if err := req.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			switch req.Form.Get("grant_type") {
			case "client_credentials":
				fmt.Fprint(w, `{"access_token":"app-token","expires_in_seconds":1800,"token_type":"bearer"}`)
			case "device_code":
				r.mu.Lock()
				r.devicePolls++
				pending := r.devicePolls <= 2 // two "pending" polls, then success
				r.mu.Unlock()
				if pending {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"errorCode":"errors.com.epicgames.oauth.device_code_pending","message":"pending"}`)
					return
				}
				// NOTE: no display_name - mirrors the real device-code response
				// and exercises the verify-endpoint backfill.
				fmt.Fprint(w, `{"access_token":"switch-user-token","refresh_token":"switch-refresh",`+
					`"expires_in_seconds":7200,"account_id":"acc123"}`)
			case "exchange_code":
				fmt.Fprint(w, `{"access_token":"launcher-user-token","refresh_token":"launcher-refresh",`+
					`"expires_in_seconds":7200,"account_id":"acc123"}`)
			case "refresh_token":
				fmt.Fprint(w, `{"access_token":"launcher-user-token-2","refresh_token":"launcher-refresh-2",`+
					`"expires_in_seconds":7200,"account_id":"acc123","display_name":"TestGamer"}`)
			default:
				t.Errorf("unexpected grant_type %q", req.Form.Get("grant_type"))
			}

		case strings.HasSuffix(req.URL.Path, "/account/api/oauth/deviceAuthorization"):
			fmt.Fprint(w, `{"device_code":"dev-code-1","user_code":"ABCD1234",`+
				`"verification_uri":"https://epic.test/activate",`+
				`"verification_uri_complete":"https://epic.test/activate?code=ABCD1234",`+
				`"expires_in":600,"interval":0}`)

		case strings.HasSuffix(req.URL.Path, "/account/api/oauth/exchange"):
			if got := req.Header.Get("Authorization"); got != "Bearer switch-user-token" {
				t.Errorf("exchange Authorization = %q", got)
			}
			fmt.Fprint(w, `{"code":"exchange-code-1","expires_in_seconds":300}`)

		case strings.HasSuffix(req.URL.Path, "/account/api/oauth/verify"):
			if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("verify Authorization = %q", got)
			}
			fmt.Fprint(w, `{"account_id":"acc123","display_name":"TestGamer"}`)

		case strings.HasSuffix(req.URL.Path, "/library/api/public/items"):
			cursor := req.URL.Query().Get("cursor")
			if cursor == "" {
				// Page 1: two games (one with catalog title, one sandbox-only),
				// one DLC record (filtered), one Unreal Marketplace record
				// (filtered). More pages follow.
				fmt.Fprint(w, `{"records":[`+
					`{"appName":"Peony","catalogItemId":"item-1","namespace":"ns1","sandboxName":"The Escapists","recordType":"APPLICATION","platform":["Windows"]},`+
					`{"appName":"Raccoon","catalogItemId":"item-2","namespace":"ns2","sandboxName":"Sandbox Fallback","recordType":"APPLICATION","platform":["Windows","Mac"]},`+
					`{"appName":"SomeDLC","catalogItemId":"item-3","namespace":"ns3","sandboxName":"DLC Thing","recordType":"DLC","platform":["Windows"]},`+
					`{"appName":"UEAsset","catalogItemId":"item-4","namespace":"ue","sandboxName":"Marketplace Thing","recordType":"APPLICATION","platform":["Windows"]}],`+
					`"responseMetadata":{"nextCursor":"page2"}}`)
				return
			}
			// Page 2: one more game, then the cursor ends.
			fmt.Fprint(w, `{"records":[`+
				`{"appName":"ThirdGame","catalogItemId":"item-5","namespace":"ns5","sandboxName":"Third Game","recordType":"APPLICATION","platform":[]}],`+
				`"responseMetadata":{"nextCursor":""}}`)

		case strings.Contains(req.URL.Path, "/catalog/api/shared/namespace/"):
			if got := req.Header.Get("Authorization"); got != "Bearer launcher-user-token" {
				t.Errorf("catalog Authorization = %q", got)
			}
			itemID := req.URL.Query().Get("id")
			titles := map[string]string{"item-1": "The Escapists (Catalog)"}
			if title, ok := titles[itemID]; ok {
				fmt.Fprintf(w, `{"%s":{"id":"%s","title":"%s"}}`, itemID, itemID, title)
				return
			}
			// item-2 has no catalog entry: exercises the sandboxName fallback.
			fmt.Fprintf(w, `{}`)

		default:
			t.Errorf("unexpected request path %q", req.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func epicTestEndpoints(base string) epicEndpoints {
	return epicEndpoints{
		tokenURL:      base + "/account/api/oauth/token",
		deviceAuthURL: base + "/account/api/oauth/deviceAuthorization",
		exchangeURL:   base + "/account/api/oauth/exchange",
		verifyURL:     base + "/account/api/oauth/verify",
		libraryURL: func(cursor string) string {
			if cursor == "" {
				return base + "/library/api/public/items?includeMetadata=true"
			}
			return base + "/library/api/public/items?includeMetadata=true&cursor=" + cursor
		},
		catalogURL: func(namespace, itemID string) string {
			return fmt.Sprintf("%s/catalog/api/shared/namespace/%s/bulk/items?id=%s", base, namespace, itemID)
		},
	}
}

func TestEpicDeviceLoginEndsInLauncherSession(t *testing.T) {
	srv := newEpicStubServer(t, &epicStubRoutes{})

	var saved []EpicTokens
	hooks := &EpicTokenHooks{Save: func(tok EpicTokens) { saved = append(saved, tok) }}
	src := NewEpicTokenSource("", "", hooks)
	src.ep = epicTestEndpoints(srv.URL)
	src.pollInterval = 10 * time.Millisecond

	browserURLs := 0
	if err := src.DeviceLogin(func(string) { browserURLs++ }); err != nil {
		t.Fatalf("DeviceLogin: %v", err)
	}

	tok, err := src.Tokens()
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	// The persisted session must be the LAUNCHER session, not the switch one.
	if tok.AccessToken != "launcher-user-token" || tok.RefreshToken != "launcher-refresh" {
		t.Fatalf("session = %+v", tok)
	}
	// The device-code response carried no display name; verify must backfill it.
	if tok.DisplayName != "TestGamer" {
		t.Fatalf("display name = %q, want backfilled TestGamer", tok.DisplayName)
	}
	if tok.AccountID != "acc123" {
		t.Fatalf("account id = %q", tok.AccountID)
	}
	if len(saved) != 1 || saved[0].AccessToken != "launcher-user-token" {
		t.Fatalf("saved sessions = %+v", saved)
	}
	if browserURLs != 1 {
		t.Fatalf("browser opened %d times, want 1", browserURLs)
	}
}

func TestEpicTokensRefreshesExpiredSession(t *testing.T) {
	srv := newEpicStubServer(t, &epicStubRoutes{})

	var saved EpicTokens
	hooks := &EpicTokenHooks{
		Load: func() *EpicTokens {
			return &EpicTokens{
				AccessToken:  "stale-token",
				RefreshToken: "launcher-refresh",
				ExpiresAt:    time.Now().Add(-time.Hour), // expired
				AccountID:    "acc123",
			}
		},
		Save: func(tok EpicTokens) { saved = tok },
	}
	src := NewEpicTokenSource("", "", hooks)
	src.ep = epicTestEndpoints(srv.URL)

	tok, err := src.Tokens()
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if tok.AccessToken != "launcher-user-token-2" {
		t.Fatalf("refreshed token = %q", tok.AccessToken)
	}
	if saved.AccessToken != "launcher-user-token-2" {
		t.Fatalf("refreshed session not persisted: %+v", saved)
	}
}

func TestEpicTokensFailsWithoutSession(t *testing.T) {
	srv := newEpicStubServer(t, &epicStubRoutes{})
	src := NewEpicTokenSource("", "", &EpicTokenHooks{})
	src.ep = epicTestEndpoints(srv.URL)

	if _, err := src.Tokens(); err == nil {
		t.Fatal("expected error when no session is stored")
	}
}

func TestEpicLibraryPaginationTerminatesOnEmptyPage(t *testing.T) {
	// Regression: Epic keeps returning an ADVANCING nextCursor on the page
	// after the last one (with zero records). The old loop trusted the cursor
	// to become empty and spun forever - sync hung at "Syncing 1 store(s)...".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.URL.Path, "/catalog/api/shared/namespace/") {
			// No catalog data here; titles fall back to sandboxName.
			fmt.Fprint(w, `{}`)
			return
		}
		if req.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"records":[{"appName":"OnlyGame","catalogItemId":"c1","namespace":"ns",`+
				`"sandboxName":"Only Game","recordType":"APPLICATION","platform":["Windows"]}],`+
				`"responseMetadata":{"nextCursor":"offset-100"}}`)
			return
		}
		// Every later page: no records, yet a different (advancing) cursor.
		fmt.Fprint(w, `{"records":[],"responseMetadata":{"nextCursor":"offset-99999"}}`)
	}))
	defer srv.Close()

	apiC := NewEpicGamesAPI("", "", nil)
	apiC.tokens.ep = epicTestEndpoints(srv.URL)
	apiC.tokens.now = EpicTokens{
		AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour), AccountID: "a", DisplayName: "x",
	}

	done := make(chan []models.Game, 1)
	errCh := make(chan error, 1)
	go func() {
		games, err := apiC.GetOwnedGames()
		if err != nil {
			errCh <- err
		} else {
			done <- games
		}
	}()
	select {
	case games := <-done:
		if len(games) != 1 || games[0].Title != "Only Game" {
			t.Fatalf("games = %+v", games)
		}
	case err := <-errCh:
		t.Fatalf("GetOwnedGames: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("pagination did not terminate - infinite cursor loop")
	}
}

func TestEpicGetOwnedGamesUsesLibraryService(t *testing.T) {
	srv := newEpicStubServer(t, &epicStubRoutes{})

	apiC := NewEpicGamesAPI("", "", nil)
	apiC.tokens.ep = epicTestEndpoints(srv.URL)
	// Seed a valid launcher session directly.
	apiC.tokens.now = EpicTokens{
		AccessToken: "launcher-user-token", RefreshToken: "launcher-refresh",
		ExpiresAt: time.Now().Add(time.Hour), AccountID: "acc123", DisplayName: "TestGamer",
	}

	games, err := apiC.GetOwnedGames()
	if err != nil {
		t.Fatalf("GetOwnedGames: %v", err)
	}
	// Library has 5 records; DLC and Unreal Marketplace records are filtered,
	// leaving 3 games across two paginated pages.
	if len(games) != 3 {
		t.Fatalf("games = %d (%+v), want 3", len(games), games)
	}

	byTitle := map[string]models.Game{}
	for _, g := range games {
		byTitle[g.Title] = g
	}
	// item-1 resolves via the catalog service.
	if g, ok := byTitle["The Escapists (Catalog)"]; !ok || g.StoreIDs[models.StoreEpic] != "Peony" {
		t.Fatalf("catalog-titled game missing or wrong: %+v", g)
	}
	// item-2 has no catalog entry: falls back to the record's sandboxName.
	if g, ok := byTitle["Sandbox Fallback"]; !ok || g.StoreIDs[models.StoreEpic] != "Raccoon" {
		t.Fatalf("sandbox-fallback game missing or wrong: %+v", g)
	}
	// item-5 has neither catalog title nor sandbox name beyond the game name;
	// falls back to the app name via the record's sandboxName field.
	if g, ok := byTitle["Third Game"]; !ok || g.StoreIDs[models.StoreEpic] != "ThirdGame" {
		t.Fatalf("app-name-fallback game missing or wrong: %+v", g)
	}
	for _, g := range games {
		if g.OwnedStores[0] != models.StoreEpic {
			t.Fatalf("owned stores = %v", g.OwnedStores)
		}
	}
}
