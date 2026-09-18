package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestDecodesJSONAndSendsHeaders(t *testing.T) {
	var gotPath, gotAuth, gotCID, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCID = r.Header.Get("Client-ID")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Halo"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, map[string]string{
		"Authorization": "Bearer test-token",
		"Client-ID":     "test-cid",
	})

	var dst struct {
		Name string `json:"name"`
	}
	if err := c.Request("POST", "/games", strings.NewReader(`fields name;`), &dst); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if dst.Name != "Halo" {
		t.Fatalf("decoded %+#v", dst)
	}
	if gotPath != "/games" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if gotCID != "test-cid" {
		t.Fatalf("Client-ID header = %q", gotCID)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q, want default application/json", gotCT)
	}
}

func TestRequestReturnsAPIErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	err := c.Request("GET", "/x", nil, &struct{}{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Code != http.StatusForbidden {
		t.Fatalf("code = %d", apiErr.Code)
	}
	if apiErr.Message == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestDoKeepsPersistentContentTypeOverride(t *testing.T) {
	// Form-encoded callers (Epic/Battle.net token endpoints) must be able to
	// override the JSON default that Request applies.
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if _, _, err := c.Do("POST", "/token", strings.NewReader("grant_type=client_credentials"), nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q, want form override", gotCT)
	}
}

func TestSetHeaderOverridesPersistently(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, map[string]string{"Authorization": "Bearer old"})
	c.SetHeader("Authorization", "Bearer new")
	if _, _, err := c.Do("GET", "/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer new" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}
