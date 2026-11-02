package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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

const (
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	credsFileName = "creds.json"

	// After every client rejects the refresh token, wait this long before
	// asking Google again. A revoked token never recovers on its own, and
	// retrying on every request only deepens whatever triggered the
	// revocation; a fresh creds.json is still adopted immediately.
	authFailureCooldown = 30 * time.Second

	// While refreshes keep failing, re-consult the agy CLI's store at most
	// this often, so a re-login made after the streak began still rescues
	// the running process.
	agyStoreCheckInterval = 30 * time.Second
)

type storedToken struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	Expiry       string `json:"expiry"`
}

type tokenManager struct {
	mu           sync.Mutex
	stateDir     string
	refreshToken string
	accessToken  string
	expiresAt    time.Time
	clientHint   int
	httpClient   *http.Client
	logger       *logger
	credsModTime time.Time
	lastFailure  error
	failedAt     time.Time
	// The agy store is consulted only after a refresh has failed, so an
	// ordinary start never triggers a keychain access prompt.
	lastAgyStoreCheck time.Time
	readAgyStore      func() (storedToken, error)
	tokenURL          string
}

func newTokenManager(stateDir string, log *logger) (*tokenManager, error) {
	tok, err := loadStoredToken(stateDir)
	if err != nil {
		return nil, err
	}
	tm := &tokenManager{
		stateDir:     stateDir,
		refreshToken: tok.RefreshToken,
		accessToken:  tok.AccessToken,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		logger:       log,
		expiresAt:    expiryOf(tok),
		readAgyStore: readAgyStoredToken,
		tokenURL:     tokenEndpoint,
	}
	tm.credsModTime = credsModTime(credsPath(stateDir))
	return tm, nil
}

func credsPath(stateDir string) string {
	return filepath.Join(stateDir, credsFileName)
}

func credsModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// loadStoredToken prefers the proxy's own creds.json and falls back to the agy
// CLI's keyring entry only when the file is missing or unusable. Reading the
// keyring eagerly would prompt for access on every start; a creds.json whose
// token has since gone stale is rescued in Token instead.
func loadStoredToken(stateDir string) (storedToken, error) {
	path := credsPath(stateDir)
	fileTok, fileErr := readCredsFile(path)
	if fileErr == nil {
		return fileTok, nil
	}
	keyTok, keyErr := readAgyStoredToken()
	if keyErr != nil {
		return storedToken{}, fmt.Errorf("no refresh token found; run agy once and log in, or place credentials at %s (creds file: %v; agy store: %v)", path, fileErr, keyErr)
	}
	if err := writeCredsFile(path, keyTok); err != nil {
		return storedToken{}, fmt.Errorf("copying agy credentials to %s: %w", path, err)
	}
	return keyTok, nil
}

func expiryOf(tok storedToken) time.Time {
	parsed, err := time.Parse(time.RFC3339, tok.Expiry)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func readCredsFile(path string) (storedToken, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storedToken{}, err
	}
	var tok storedToken
	if err := json.Unmarshal(data, &tok); err != nil {
		return storedToken{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if tok.RefreshToken == "" {
		return storedToken{}, fmt.Errorf("%s has no refresh token", path)
	}
	return tok, nil
}

func writeCredsFile(path string, tok storedToken) error {
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".creds-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// readAgyStoredToken reads whatever store the agy CLI used. On macOS that is
// the keychain; on Linux it is the Secret Service, and when no session bus was
// reachable the CLI falls back to a file under ~/.gemini. Linux keeps both
// candidates and prefers the fresher, because a CLI that logged in again writes
// the file while an older entry can linger in the keyring.
func readAgyStoredToken() (storedToken, error) {
	if _, err := exec.LookPath("security"); err == nil {
		out, err := exec.Command("security", "find-generic-password", "-s", "gemini", "-a", "antigravity", "-w").Output()
		if err != nil {
			return storedToken{}, fmt.Errorf("security find-generic-password failed: %w", err)
		}
		return parseKeyringBlob(strings.TrimSpace(string(out)))
	}

	var candidates []storedToken
	var errs []error
	if _, err := exec.LookPath("secret-tool"); err == nil {
		if tok, err := readTokenFromSecretService(); err == nil {
			candidates = append(candidates, tok)
		} else {
			errs = append(errs, err)
		}
	}
	if path := agyCLITokenPath(); path != "" {
		if tok, err := readAgyTokenFile(path); err == nil {
			candidates = append(candidates, tok)
		} else {
			errs = append(errs, err)
		}
	}
	if len(candidates) == 0 {
		if len(errs) == 0 {
			return storedToken{}, fmt.Errorf("no agy credential store available (needs macOS security, Linux secret-tool, or %s)", agyCLITokenPath())
		}
		return storedToken{}, fmt.Errorf("could not read agy credentials: %w", errors.Join(errs...))
	}
	return newestToken(candidates), nil
}

func newestToken(tokens []storedToken) storedToken {
	newest := tokens[0]
	for _, tok := range tokens[1:] {
		if expiryOf(tok).After(expiryOf(newest)) {
			newest = tok
		}
	}
	return newest
}

// agyCLITokenPath is where the Linux CLI falls back to when it cannot reach a
// keyring; it is the same "token"-wrapped JSON shape as the keychain entry.
func agyCLITokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

func readAgyTokenFile(path string) (storedToken, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storedToken{}, err
	}
	return parseKeyringBlob(string(data))
}

// readTokenFromSecretService looks the entry up through libsecret. go-keyring
// labels the attributes "service"/"username"; older stores used "account", so
// try both before giving up.
func readTokenFromSecretService() (storedToken, error) {
	lookups := [][]string{
		{"lookup", "service", "gemini", "username", "antigravity"},
		{"lookup", "service", "gemini", "account", "antigravity"},
	}
	lastErr := errors.New("no matching secret-tool attributes")
	for _, args := range lookups {
		out, err := exec.Command("secret-tool", args...).Output()
		if err != nil {
			lastErr = err
			continue
		}
		if strings.TrimSpace(string(out)) == "" {
			lastErr = errors.New("secret-tool returned an empty secret")
			continue
		}
		tok, err := parseKeyringBlob(strings.TrimSpace(string(out)))
		if err == nil {
			return tok, nil
		}
		lastErr = err
	}
	return storedToken{}, fmt.Errorf("secret-tool lookup failed: %w", lastErr)
}

func parseKeyringBlob(raw string) (storedToken, error) {
	if strings.HasPrefix(raw, "go-keyring-base64:") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "go-keyring-base64:"))
		if err != nil {
			return storedToken{}, fmt.Errorf("decoding keychain blob: %w", err)
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
	tm.adoptExternalCreds()
	if tm.lastFailure != nil && time.Since(tm.failedAt) < authFailureCooldown {
		return "", tm.lastFailure
	}

	access, err := tm.refreshAccessToken()
	if err == nil {
		return access, nil
	}
	// creds.json can hold a token that has since been revoked while the CLI's
	// own store is still good; consult it, and again every so often, since a
	// re-login can land in the CLI's store after the failure streak began.
	if time.Since(tm.lastAgyStoreCheck) >= agyStoreCheckInterval {
		tm.lastAgyStoreCheck = time.Now()
		if tm.adoptAgyStoreToken() {
			if access, err = tm.refreshAccessToken(); err == nil {
				return access, nil
			}
		}
	}

	tm.lastFailure = fmt.Errorf("token refresh failed with all known oauth clients: %w", err)
	tm.failedAt = time.Now()
	tm.logger.log.Printf("%v", tm.lastFailure)
	return "", tm.lastFailure
}

func (tm *tokenManager) refreshAccessToken() (string, error) {
	var failures []error
	for _, idx := range tm.clientOrder() {
		access, expiresIn, err := tm.refreshWith(oauthClients[idx])
		if err != nil {
			failures = append(failures, err)
			continue
		}
		tm.clientHint = idx
		tm.accessToken = access
		tm.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
		tm.lastFailure = nil
		tm.logger.debugf("access token refreshed via oauth client %d (expires in %ds)", idx, expiresIn)
		tm.persist()
		return access, nil
	}
	return "", errors.Join(failures...)
}

func (tm *tokenManager) clientOrder() []int {
	order := make([]int, len(oauthClients))
	for i := range order {
		order[i] = i
	}
	// Try the client that last worked first to avoid needless failed
	// refresh round-trips.
	if tm.clientHint >= 0 && tm.clientHint < len(order) {
		order[0], order[tm.clientHint] = order[tm.clientHint], order[0]
	}
	return order
}

// adoptExternalCreds picks up a creds.json replaced while the process runs, so
// re-authenticating does not require a restart.
func (tm *tokenManager) adoptExternalCreds() {
	path := credsPath(tm.stateDir)
	modTime := credsModTime(path)
	if modTime.IsZero() || !modTime.After(tm.credsModTime) {
		return
	}
	tm.credsModTime = modTime
	tok, err := readCredsFile(path)
	if err != nil || tok.RefreshToken == tm.refreshToken {
		return
	}
	tm.refreshToken = tok.RefreshToken
	tm.accessToken = tok.AccessToken
	tm.expiresAt = expiryOf(tok)
	tm.lastFailure = nil
	tm.logger.log.Printf("picked up new credentials from %s", path)
}

// adoptAgyStoreToken swaps in the token the CLI holds when it differs from the
// one in use, reporting whether anything changed.
func (tm *tokenManager) adoptAgyStoreToken() bool {
	read := tm.readAgyStore
	if read == nil {
		read = readAgyStoredToken
	}
	tok, err := read()
	if err != nil || tok.RefreshToken == "" || tok.RefreshToken == tm.refreshToken {
		return false
	}
	tm.refreshToken = tok.RefreshToken
	tm.accessToken = tok.AccessToken
	tm.expiresAt = expiryOf(tok)
	tm.lastFailure = nil
	tm.logger.log.Printf("adopting the refresh token held by the agy CLI")
	tm.persist()
	return true
}

// persist writes the current tokens back, including a rotated refresh token.
// Google replaces the refresh token on occasion and never returns the old one
// to life, so an in-memory-only update used to strand the next restart.
func (tm *tokenManager) persist() {
	path := credsPath(tm.stateDir)
	tok := storedToken{
		RefreshToken: tm.refreshToken,
		AccessToken:  tm.accessToken,
		Expiry:       tm.expiresAt.UTC().Format(time.RFC3339Nano),
	}
	if err := writeCredsFile(path, tok); err != nil {
		tm.logger.log.Printf("warning: could not persist refreshed credentials to %s: %v", path, err)
		return
	}
	tm.credsModTime = credsModTime(path)
}

func (tm *tokenManager) refreshWith(c oauthClient) (string, int, error) {
	form := url.Values{
		"client_id":     {c.id},
		"client_secret": {c.secret},
		"refresh_token": {tm.refreshToken},
		"grant_type":    {"refresh_token"},
	}
	tokenURL := tm.tokenURL
	if tokenURL == "" {
		tokenURL = tokenEndpoint
	}
	resp, err := tm.httpClient.PostForm(tokenURL, form)
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
