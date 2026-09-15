package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type oauthClient struct {
	id     string
	secret string
}

// OAuth clients embedded in the agy CLI binary (installed-app type).
// The keychain refresh token is issued to the first pair; the second is the
// Antigravity IDE client, kept in case a re-login ever mints tokens for it.
var oauthClients = []oauthClient{
	{
		id:     "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
		secret: "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",
	},
	{
		id:     "884354919052-36trc1jjb3tguiac32ov6cod268c5blh.apps.googleusercontent.com",
		secret: "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",
	},
}

const tokenEndpoint = "https://oauth2.googleapis.com/token"

type storedToken struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	Expiry       string `json:"expiry"`
}

type tokenManager struct {
	mu           sync.Mutex
	refreshToken string
	accessToken  string
	expiresAt    time.Time
	clientHint   int
	httpClient   *http.Client
	logger       *logger
}

func newTokenManager(stateDir string, log *logger) (*tokenManager, error) {
	tok, err := loadStoredToken(stateDir)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token found; run agy once and log in, or place creds at %s", filepath.Join(stateDir, "creds.json"))
	}
	tm := &tokenManager{refreshToken: tok.RefreshToken, accessToken: tok.AccessToken, httpClient: &http.Client{Timeout: 30 * time.Second}, logger: log}
	if t, terr := time.Parse(time.RFC3339, tok.Expiry); terr == nil {
		tm.expiresAt = t
	}
	return tm, nil
}

func loadStoredToken(stateDir string) (storedToken, error) {
	credsPath := filepath.Join(stateDir, "creds.json")
	if data, err := os.ReadFile(credsPath); err == nil {
		var tok storedToken
		if json.Unmarshal(data, &tok) == nil && tok.RefreshToken != "" {
			return tok, nil
		}
	}
	tok, err := readTokenFromKeychain()
	if err != nil {
		return tok, fmt.Errorf("neither %s nor the macOS keychain entry (service=gemini, account=antigravity) could be read: %w", credsPath, err)
	}
	_ = os.MkdirAll(stateDir, 0o700)
	if data, merr := json.Marshal(tok); merr == nil {
		_ = os.WriteFile(credsPath, data, 0o600)
	}
	return tok, nil
}

// readTokenFromKeychain decodes the entry the agy CLI stores via go-keyring:
// "go-keyring-base64:" + base64(JSON), where the JSON wraps a "token" object.
func readTokenFromKeychain() (storedToken, error) {
	if _, err := exec.LookPath("security"); err != nil {
		return storedToken{}, fmt.Errorf("security CLI not found")
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "gemini", "-a", "antigravity", "-w").Output()
	if err != nil {
		return storedToken{}, fmt.Errorf("security find-generic-password failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if strings.HasPrefix(raw, "go-keyring-base64:") {
		decoded, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "go-keyring-base64:"))
		if derr != nil {
			return storedToken{}, fmt.Errorf("decoding keychain blob: %w", derr)
		}
		raw = string(decoded)
	}
	var wrapper struct {
		Token storedToken `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		var flat storedToken
		if json.Unmarshal([]byte(raw), &flat) != nil || flat.RefreshToken == "" {
			return storedToken{}, fmt.Errorf("unrecognized keychain token format: %w", err)
		}
		return flat, nil
	}
	if wrapper.Token.RefreshToken == "" {
		return storedToken{}, fmt.Errorf("keychain entry has no refresh token")
	}
	return wrapper.Token, nil
}

func (tm *tokenManager) Token() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.accessToken != "" && time.Now().Before(tm.expiresAt.Add(-time.Minute)) {
		return tm.accessToken, nil
	}
	order := make([]int, len(oauthClients))
	for i := range order {
		order[i] = i
	}
	// The paired client is usually the first one; try the previously working
	// client before the others to avoid needless failed refresh round-trips.
	if tm.clientHint >= 0 && tm.clientHint < len(order) {
		order[0], order[tm.clientHint] = order[tm.clientHint], order[0]
	}
	var lastErr error
	for _, idx := range order {
		access, expiresIn, rerr := tm.refreshWith(oauthClients[idx])
		if rerr != nil {
			lastErr = rerr
			continue
		}
		tm.clientHint = idx
		tm.accessToken = access
		tm.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
		tm.logger.debugf("access token refreshed via oauth client %d (expires in %ds)", idx, expiresIn)
		return access, nil
	}
	return "", fmt.Errorf("token refresh failed with all known oauth clients: %w", lastErr)
}

func (tm *tokenManager) refreshWith(c oauthClient) (string, int, error) {
	form := url.Values{
		"client_id":     {c.id},
		"client_secret": {c.secret},
		"refresh_token": {tm.refreshToken},
		"grant_type":    {"refresh_token"},
	}
	resp, err := tm.httpClient.PostForm(tokenEndpoint, form)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("oauth client %s: status %d: %s", c.id[:14], resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", 0, err
	}
	if payload.RefreshToken != "" {
		tm.refreshToken = payload.RefreshToken
	}
	return payload.AccessToken, payload.ExpiresIn, nil
}

func newRequestID() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return n.String()
}
