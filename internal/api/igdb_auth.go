package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// TokenSource manages the Twitch OAuth client-credentials token that the IGDB
// API requires. Tokens are short-lived (the docs' example is ~65 days); the
// source fetches and refreshes them automatically so users never paste a
// bearer token into config.
//
// Load/Save optionally persist the token between runs (wired to the local
// database by the service layer); fetch is injectable for tests.
type TokenSource struct {
	ClientID     string
	ClientSecret string

	Load func() (token string, expiresAt time.Time, ok bool)
	Save func(token string, expiresAt time.Time)

	fetch func(clientID, clientSecret string) (accessToken string, expiresInSeconds int64, err error)

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTokenSource creates a source backed by the real Twitch token endpoint.
func NewTokenSource(clientID, clientSecret string) *TokenSource {
	return &TokenSource{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		fetch:        fetchTwitchToken,
	}
}

// Token returns a valid access token, refreshing it when missing or expired.
// Tokens are refreshed a minute before actual expiry to avoid edge-of-life
// 401s.
func (t *TokenSource) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if t.token != "" && now.Before(t.expiresAt) {
		return t.token, nil
	}
	if t.Load != nil {
		if tok, exp, ok := t.Load(); ok && tok != "" && now.Before(exp) {
			t.token, t.expiresAt = tok, exp
			return t.token, nil
		}
	}

	tok, expiresIn, err := t.fetch(t.ClientID, t.ClientSecret)
	if err != nil {
		return "", err
	}
	t.token = tok
	t.expiresAt = now.Add(time.Duration(expiresIn-60) * time.Second)
	if t.Save != nil {
		t.Save(t.token, t.expiresAt)
	}
	return t.token, nil
}

// fetchTwitchToken performs the client-credentials grant against Twitch.
// https://api-docs.igdb.com/#account-creation
func fetchTwitchToken(clientID, clientSecret string) (string, int64, error) {
	if clientID == "" || clientSecret == "" {
		return "", 0, fmt.Errorf("twitch token: clientId and clientSecret must be set in configs/config.yaml")
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("grant_type", "client_credentials")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm("https://id.twitch.tv/oauth2/token", form)
	if err != nil {
		return "", 0, fmt.Errorf("twitch token request: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", 0, fmt.Errorf("twitch token decode (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", 0, fmt.Errorf("twitch token refused (status %d): %s %s", resp.StatusCode, body.Error, body.Message)
	}
	return body.AccessToken, body.ExpiresIn, nil
}
