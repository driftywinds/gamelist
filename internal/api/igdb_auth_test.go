package api

import (
	"errors"
	"testing"
	"time"
)

func TestTokenSourceCachesUntilExpiry(t *testing.T) {
	calls := 0
	ts := NewTokenSource("id", "secret")
	ts.fetch = func(clientID, clientSecret string) (string, int64, error) {
		calls++
		return "tok", 3600, nil
	}

	for i := 0; i < 3; i++ {
		tok, err := ts.Token()
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if tok != "tok" {
			t.Fatalf("token = %q", tok)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 fetch for repeated calls, got %d", calls)
	}
}

func TestTokenSourceRefreshesExpiredToken(t *testing.T) {
	tokens := []string{"tok1", "tok2"}
	calls := 0
	ts := NewTokenSource("id", "secret")
	ts.fetch = func(clientID, clientSecret string) (string, int64, error) {
		tok := tokens[calls]
		calls++
		// 30s lifetime minus the 60s safety margin = already expired on the
		// next call.
		return tok, 30, nil
	}

	tok1, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	tok2, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok1 != "tok1" || tok2 != "tok2" {
		t.Fatalf("tokens = %q, %q", tok1, tok2)
	}
	if calls != 2 {
		t.Fatalf("expected refetch of expired token, fetches = %d", calls)
	}
}

func TestTokenSourceUsesLoadSaveHooks(t *testing.T) {
	loads, saves, fetches := 0, 0, 0
	ts := NewTokenSource("id", "secret")
	ts.fetch = func(clientID, clientSecret string) (string, int64, error) {
		fetches++
		return "", 0, errors.New("network must not be touched")
	}
	ts.Load = func() (string, time.Time, bool) {
		loads++
		return "cached-token", time.Now().Add(time.Hour), true
	}
	ts.Save = func(token string, expiresAt time.Time) {
		saves++
	}

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "cached-token" {
		t.Fatalf("token = %q", tok)
	}
	if loads != 1 || saves != 0 || fetches != 0 {
		t.Fatalf("loads=%d saves=%d fetches=%d", loads, saves, fetches)
	}
}

func TestTokenSourceSavesAfterFetch(t *testing.T) {
	var savedToken string
	var savedExpiry time.Time
	ts := NewTokenSource("id", "secret")
	ts.fetch = func(clientID, clientSecret string) (string, int64, error) {
		return "fresh", 3600, nil
	}
	ts.Save = func(token string, expiresAt time.Time) {
		savedToken, savedExpiry = token, expiresAt
	}

	if _, err := ts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if savedToken != "fresh" {
		t.Fatalf("saved token = %q", savedToken)
	}
	if !savedExpiry.After(time.Now().Add(time.Hour - 2*time.Minute)) {
		t.Fatalf("saved expiry %v should be ~59m out (1h minus 60s safety margin)", savedExpiry)
	}
}
