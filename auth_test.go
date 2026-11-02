package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *logger {
	return &logger{log: log.New(io.Discard, "", 0)}
}

func TestCredsFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), credsFileName)
	want := storedToken{RefreshToken: "rt", AccessToken: "at", Expiry: "2026-10-28T16:00:00Z"}
	if err := writeCredsFile(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readCredsFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("creds file mode = %o, want 600", perm)
	}
}

func TestAdoptExternalCredsPicksUpNewRefreshToken(t *testing.T) {
	dir := t.TempDir()
	path := credsPath(dir)
	original := storedToken{RefreshToken: "old", Expiry: "2026-09-17T05:23:25Z"}
	if err := writeCredsFile(path, original); err != nil {
		t.Fatalf("write: %v", err)
	}
	tm := &tokenManager{
		stateDir:     dir,
		refreshToken: original.RefreshToken,
		credsModTime: credsModTime(path),
		clientHint:   0,
		logger:       testLogger(),
	}

	replacement := storedToken{RefreshToken: "new", AccessToken: "at2", Expiry: "2026-10-28T16:00:00Z"}
	if err := writeCredsFile(path, replacement); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	tm.adoptExternalCreds()
	if tm.refreshToken != "new" {
		t.Fatalf("refresh token = %q, want new", tm.refreshToken)
	}
	if tm.accessToken != "at2" {
		t.Fatalf("access token = %q, want at2", tm.accessToken)
	}
}

func TestTokenRecoversFromAgyReLoginDuringFailureStreak(t *testing.T) {
	storeToken := "stale"
	consults := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("refresh_token") != "fresh" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 3600})
	}))
	defer server.Close()

	tm := &tokenManager{
		stateDir:     t.TempDir(),
		refreshToken: "stale",
		httpClient:   server.Client(),
		tokenURL:     server.URL,
		logger:       testLogger(),
		readAgyStore: func() (storedToken, error) {
			consults++
			return storedToken{RefreshToken: storeToken}, nil
		},
	}

	if _, err := tm.Token(); err == nil {
		t.Fatal("Token succeeded with a revoked refresh token")
	}
	if consults != 1 {
		t.Fatalf("agy store consulted %d times, want 1 after the first failure", consults)
	}

	if _, err := tm.Token(); err == nil {
		t.Fatal("Token succeeded with a revoked refresh token")
	}
	if consults != 1 {
		t.Fatalf("agy store consulted %d times, want no re-consult within the check interval", consults)
	}

	storeToken = "fresh"
	tm.failedAt = time.Time{}
	tm.lastAgyStoreCheck = time.Now().Add(-agyStoreCheckInterval)
	access, err := tm.Token()
	if err != nil {
		t.Fatalf("Token after agy re-login: %v", err)
	}
	if access != "at" {
		t.Fatalf("access token = %q, want at", access)
	}
	if consults != 2 {
		t.Fatalf("agy store consulted %d times, want 2 after the interval elapsed", consults)
	}
}
