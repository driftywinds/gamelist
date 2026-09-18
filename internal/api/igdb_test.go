package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTokenSource never touches the network.
func stubTokenSource() *TokenSource {
	ts := NewTokenSource("test-cid", "test-secret")
	ts.fetch = func(clientID, clientSecret string) (string, int64, error) {
		return "test-token", 3600, nil
	}
	return ts
}

func newTestIGDBClient(t *testing.T, handler http.HandlerFunc) *IGDBClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewIGDBClientWithBaseURL(srv.URL, stubTokenSource())
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return string(data)
}

func TestGetGameDetailsUsesInvolvedCompaniesAndSplitsRoles(t *testing.T) {
	var gotBody string
	c := newTestIGDBClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id": 1020,
			"name": "Halo 3",
			"summary": "Finish the fight.",
			"first_release_date": 1190678400,
			"involved_companies": [
				{"company": {"name": "Bungie"}, "developer": true, "publisher": false},
				{"company": {"name": "Microsoft Studios"}, "developer": false, "publisher": true},
				{"company": {"name": "Both Roles Inc."}, "developer": true, "publisher": true}
			],
			"platforms": [{"name": "Xbox 360"}, {"name": "PC (Microsoft Windows)"}]
		}]`))
	})

	g, err := c.GetGameDetails(1020)
	if err != nil {
		t.Fatalf("GetGameDetails: %v", err)
	}

	// The v4 games endpoint has no publishers/developers fields; the query
	// must use involved_companies flags instead.
	if !strings.Contains(gotBody, "involved_companies.developer") ||
		!strings.Contains(gotBody, "involved_companies.publisher") {
		t.Fatalf("details query must request involved_companies flags, got: %s", gotBody)
	}
	if strings.Contains(gotBody, "publishers.name") {
		t.Fatalf("details query must not reference the removed v4 publishers field: %s", gotBody)
	}

	if g.Title != "Halo 3" {
		t.Fatalf("title = %q", g.Title)
	}
	if g.ReleaseDate != "2007-09-25" {
		t.Fatalf("release date = %q, want UTC-normalized 2007-09-25", g.ReleaseDate)
	}
	if len(g.Developers) != 2 || g.Developers[0] != "Bungie" || g.Developers[1] != "Both Roles Inc." {
		t.Fatalf("developers = %v", g.Developers)
	}
	if len(g.Publishers) != 2 || g.Publishers[0] != "Microsoft Studios" || g.Publishers[1] != "Both Roles Inc." {
		t.Fatalf("publishers = %v", g.Publishers)
	}
	if len(g.Platforms) != 2 {
		t.Fatalf("platforms = %v", g.Platforms)
	}
}

func TestFindGameByExternalID(t *testing.T) {
	var gotBody string
	c := newTestIGDBClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": 501, "game": 1020, "uid": "620"}]`))
	})

	id, err := c.FindGameByExternalID(ExternalGameSourceSteam, "620")
	if err != nil {
		t.Fatalf("FindGameByExternalID: %v", err)
	}
	if id != 1020 {
		t.Fatalf("game id = %d, want 1020", id)
	}
	if !strings.Contains(gotBody, `external_game_source = 1 & uid = "620"`) {
		t.Fatalf("unexpected external_games query: %s", gotBody)
	}
}

func TestFindGameByExternalIDNoMatchReturnsZero(t *testing.T) {
	c := newTestIGDBClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	id, err := c.FindGameByExternalID(ExternalGameSourceSteam, "999999")
	if err != nil {
		t.Fatalf("FindGameByExternalID: %v", err)
	}
	if id != 0 {
		t.Fatalf("id = %d, want 0 for no match", id)
	}
}

func TestSearchGamesSendsAuthHeaders(t *testing.T) {
	var gotAuth, gotCID string
	c := newTestIGDBClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCID = r.Header.Get("Client-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1942,"name":"Halo: Reach","summary":"...","first_release_date":1285891200}]`))
	})

	games, err := c.SearchGames("Halo")
	if err != nil {
		t.Fatalf("SearchGames: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCID != "test-cid" {
		t.Fatalf("Client-ID = %q", gotCID)
	}
	if len(games) != 1 || games[0].ID != 1942 || games[0].Title != "Halo: Reach" {
		t.Fatalf("games = %+#v", games)
	}
}

func TestEscapeIGDBString(t *testing.T) {
	cases := []struct{ in, want string }{
		{`halo`, `halo`},
		{`say "hi"`, `say \"hi\"`},
		{`back\slash`, `back\\slash`},
		{`both "\`, `both \"\\`},
	}
	for _, tc := range cases {
		if got := escapeIGDBString(tc.in); got != tc.want {
			t.Errorf("escapeIGDBString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIGDBTimestampToDateIsUTC(t *testing.T) {
	// 2007-09-25 00:00:00 UTC. Formatting in local time produced the previous
	// day in UTC-negative timezones; UTC must always yield the same date.
	if got := igdbTimestampToDate(1190678400); got != "2007-09-25" {
		t.Fatalf("got %q, want 2007-09-25 regardless of local timezone", got)
	}
	if got := igdbTimestampToDate(0); got != "" {
		t.Fatalf("zero timestamp should render empty, got %q", got)
	}
}
