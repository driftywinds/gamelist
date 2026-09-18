package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gamelist/internal/models"
)

// newGOGStubServer implements the GOG endpoints used by the client.
func newGOGStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/token"):
			if err := req.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			if req.Form.Get("client_id") != gogClientID || req.Form.Get("client_secret") != gogClientSecret {
				t.Errorf("client credentials missing from token request: %v", req.Form)
			}
			switch req.Form.Get("grant_type") {
			case "authorization_code":
				if req.Form.Get("code") != "good-code" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":"invalid_grant","error_description":"bad code"}`)
					return
				}
				if req.Form.Get("redirect_uri") != gogRedirectURI {
					t.Errorf("redirect_uri = %q", req.Form.Get("redirect_uri"))
				}
				fmt.Fprint(w, `{"access_token":"gog-access","refresh_token":"gog-refresh","expires_in":2591999,"token_type":"bearer"}`)
			case "refresh_token":
				if req.Form.Get("refresh_token") != "gog-refresh" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":"invalid_grant","error_description":"bad refresh"}`)
					return
				}
				fmt.Fprint(w, `{"access_token":"gog-access-2","refresh_token":"gog-refresh-2","expires_in":2591999,"token_type":"bearer"}`)
			default:
				t.Errorf("unexpected grant_type %q", req.Form.Get("grant_type"))
			}

		case strings.HasSuffix(req.URL.Path, "/userData.json"):
			if got := req.Header.Get("Authorization"); got != "Bearer gog-access" {
				t.Errorf("userData Authorization = %q", got)
			}
			fmt.Fprint(w, `{"username":"TestGamer","email":"gamer@example.com","userId":12345}`)

		case strings.HasSuffix(req.URL.Path, "/account/getFilteredProducts"):
			if got := req.Header.Get("Authorization"); got != "Bearer gog-access" {
				t.Errorf("owned games Authorization = %q", got)
			}
			switch req.URL.Query().Get("page") {
			case "1":
				// Two games and one DLC (isGame false -> filtered out).
				fmt.Fprint(w, `{"products":[`+
					`{"id":101,"title":"Zeta Game","isGame":true},`+
					`{"id":102,"title":"Alpha Game","isGame":true},`+
					`{"id":103,"title":"Some DLC","isGame":false}],`+
					`"page":1,"totalPages":2,"totalProducts":3}`)
			case "2":
				fmt.Fprint(w, `{"products":[`+
					`{"id":104,"title":"Mid Game","is_game":true}],`+
					`"page":2,"totalPages":2,"totalProducts":3}`)
			default:
				t.Errorf("unexpected page %q", req.URL.Query().Get("page"))
			}

		default:
			t.Errorf("unexpected request path %q", req.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gogTestEndpoints(base string) gogEndpoints {
	return gogEndpoints{
		authorizeURL: func(clientID string) string {
			return base + "/auth?client_id=" + clientID
		},
		tokenURL:    base + "/token",
		userDataURL: base + "/userData.json",
		ownedURL: func(page int) string {
			return fmt.Sprintf("%s/account/getFilteredProducts?page=%d", base, page)
		},
	}
}

func gogHooksWithInput(input string) *GOGTokenHooks {
	return &GOGTokenHooks{
		ReadInput: func() (string, error) { return input, nil },
	}
}

func TestExtractGOGCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://embed.gog.com/on_login_success?origin=client&code=XYZ", "XYZ"},
		{"https://embed.gog.com/on_login_success?origin=client&locale=en-US&code=ABC123", "ABC123"},
		{"  barecode  ", "barecode"},
	}
	for _, tc := range cases {
		if got := extractGOGCode(tc.in); got != tc.want {
			t.Errorf("extractGOGCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGOGSignInExchangesCodeAndFillsUsername(t *testing.T) {
	srv := newGOGStubServer(t)

	var saved []GOGTokens
	hooks := gogHooksWithInput("https://embed.gog.com/on_login_success?origin=client&code=good-code\n")
	hooks.Save = func(tok GOGTokens) { saved = append(saved, tok) }

	g := NewGOGAPI("", "", hooks)
	g.tokens.ep = gogTestEndpoints(srv.URL)

	if err := g.SignIn(); err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	tok, err := g.Tokens()
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if tok.AccessToken != "gog-access" || tok.RefreshToken != "gog-refresh" {
		t.Fatalf("session = %+v", tok)
	}
	// Username comes from userData.json via the freshly exchanged token.
	if tok.Username != "TestGamer" {
		t.Fatalf("username = %q", tok.Username)
	}
	if len(saved) != 1 || saved[0].AccessToken != "gog-access" {
		t.Fatalf("saved sessions = %+v", saved)
	}
}

func TestGOGSignInRejectsBadCode(t *testing.T) {
	srv := newGOGStubServer(t)

	hooks := gogHooksWithInput("https://embed.gog.com/on_login_success?origin=client&code=bad-code\n")
	g := NewGOGAPI("", "", hooks)
	g.tokens.ep = gogTestEndpoints(srv.URL)

	if err := g.SignIn(); err == nil {
		t.Fatal("expected error for a rejected code")
	}
}

func TestGOGTokensRefreshesExpiredSession(t *testing.T) {
	srv := newGOGStubServer(t)

	var saved GOGTokens
	hooks := &GOGTokenHooks{
		Load: func() *GOGTokens {
			return &GOGTokens{
				AccessToken:  "stale-token",
				RefreshToken: "gog-refresh",
				ExpiresAt:    time.Now().Add(-time.Hour),
			}
		},
		Save: func(tok GOGTokens) { saved = tok },
	}
	g := NewGOGAPI("", "", hooks)
	g.tokens.ep = gogTestEndpoints(srv.URL)

	tok, err := g.Tokens()
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if tok.AccessToken != "gog-access-2" {
		t.Fatalf("refreshed token = %q", tok.AccessToken)
	}
	if saved.AccessToken != "gog-access-2" {
		t.Fatalf("refreshed session not persisted: %+v", saved)
	}
}

func TestGOGGetOwnedGamesPaginatesAndFilters(t *testing.T) {
	srv := newGOGStubServer(t)

	g := NewGOGAPI("", "", &GOGTokenHooks{})
	g.tokens.ep = gogTestEndpoints(srv.URL)
	g.tokens.now = GOGTokens{
		AccessToken: "gog-access", RefreshToken: "gog-refresh",
		ExpiresAt: time.Now().Add(time.Hour), Username: "TestGamer",
	}

	games, err := g.GetOwnedGames()
	if err != nil {
		t.Fatalf("GetOwnedGames: %v", err)
	}
	// 3 games across two pages; the isGame:false DLC is filtered out.
	if len(games) != 3 {
		t.Fatalf("games = %d (%+v), want 3", len(games), games)
	}

	// Sorted by title.
	want := []struct{ title, id string }{
		{"Alpha Game", "102"},
		{"Mid Game", "104"},
		{"Zeta Game", "101"},
	}
	for i, w := range want {
		if games[i].Title != w.title || games[i].StoreIDs[models.StoreGOG] != w.id {
			t.Fatalf("games[%d] = %+v, want title %q id %q", i, games[i], w.title, w.id)
		}
		if games[i].OwnedStores[0] != models.StoreGOG {
			t.Fatalf("owned stores = %v", games[i].OwnedStores)
		}
	}
}
