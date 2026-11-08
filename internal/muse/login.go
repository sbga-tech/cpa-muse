package muse

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Meta OAuth device flow used by Muse Code (DeviceAuthConfig::meta_default).
const (
	defaultAuthURL      = "https://auth.meta.com"
	metaOAuthClientID   = "1031625952748946"
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	defaultPollInterval = 5 * time.Second
	slowDownIncrement   = 5 * time.Second
	// defaultLoginLifetime applies when Meta omits expires_in, matching CPA's native flow.
	defaultLoginLifetime = 15 * time.Minute
	// maxPendingLogins bounds device codes held for unfinished logins.
	maxPendingLogins = 32
)

// deviceLogin is one pending device authorization. The device code stays in
// plugin memory: CPA's poll response cannot update session metadata, so poll
// pacing must live here, and the code never enters CPA's session store.
type deviceLogin struct {
	polling    sync.Mutex // serializes token polls; also guards interval and nextPoll
	deviceCode string
	expiresAt  time.Time
	interval   time.Duration
	nextPoll   time.Time
}

type loginStore struct {
	mu      sync.Mutex
	byState map[string]*deviceLogin
	now     func() time.Time
}

var logins = &loginStore{byState: make(map[string]*deviceLogin), now: time.Now}

func (s *loginStore) add(state string, login *deviceLogin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, pending := range s.byState {
		if !now.Before(pending.expiresAt) {
			delete(s.byState, key)
		}
	}
	if len(s.byState) >= maxPendingLogins {
		return errorf(http.StatusTooManyRequests, "too_many_logins", "too many pending Muse logins; finish or let one expire first")
	}
	s.byState[state] = login
	return nil
}

func (s *loginStore) get(state string) (*deviceLogin, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byState[state], s.now()
}

func (s *loginStore) remove(state string, login *deviceLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byState[state] == login {
		delete(s.byState, state)
	}
}

// StartLogin begins Meta's device authorization. CPA shows the returned URL,
// which embeds the user code, and polls PollLogin with the returned state.
func (p AuthProvider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if req.HTTPClient == nil {
		return pluginapi.AuthLoginStartResponse{}, errorf(http.StatusServiceUnavailable, "http_client_unavailable", "Muse login requires the host HTTP client")
	}
	resp, err := p.postForm(ctx, req.HTTPClient, "/oidc/device/authorization/", url.Values{"client_id": {metaOAuthClientID}})
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, errorf(http.StatusBadGateway, "login_failed", "Meta device authorization request failed: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.AuthLoginStartResponse{}, errorf(http.StatusBadGateway, "login_failed", "Meta device authorization failed (HTTP %d)%s", resp.StatusCode, oauthErrorSuffix(resp.Body))
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(resp.Body, &device); err != nil || strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" {
		return pluginapi.AuthLoginStartResponse{}, errorf(http.StatusBadGateway, "login_failed", "Meta device authorization response is missing device_code or user_code")
	}
	loginURL, err := verificationURL(device.VerificationURIComplete, device.VerificationURI, device.UserCode)
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	state, err := newLoginState()
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	now := logins.now()
	interval := durationOr(device.Interval, defaultPollInterval)
	login := &deviceLogin{
		deviceCode: device.DeviceCode,
		expiresAt:  now.Add(durationOr(device.ExpiresIn, defaultLoginLifetime)),
		interval:   interval,
		nextPoll:   now.Add(interval),
	}
	if err := logins.add(state, login); err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	return pluginapi.AuthLoginStartResponse{Provider: "muse", URL: loginURL, State: state, ExpiresAt: login.expiresAt}, nil
}

// PollLogin polls Meta's token endpoint at most once per server interval,
// regardless of how often CPA's caller polls, then mints the first API key.
func (p AuthProvider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	state := strings.TrimSpace(req.State)
	login, now := logins.get(state)
	if login == nil {
		return loginFailed("This Muse login is unknown or has expired; start a new login."), nil
	}
	if !now.Before(login.expiresAt) {
		logins.remove(state, login)
		return loginFailed("The login request expired before it was approved."), nil
	}
	if !login.polling.TryLock() {
		return loginPending(), nil
	}
	defer login.polling.Unlock()
	if now.Before(login.nextPoll) {
		return loginPending(), nil
	}
	if req.HTTPClient == nil {
		return pluginapi.AuthLoginPollResponse{}, errorf(http.StatusServiceUnavailable, "http_client_unavailable", "Muse login requires the host HTTP client")
	}
	resp, err := p.postForm(ctx, req.HTTPClient, "/oidc/device/token/", url.Values{
		"grant_type":  {deviceCodeGrantType},
		"device_code": {login.deviceCode},
		"client_id":   {metaOAuthClientID},
	})
	if err != nil {
		// A failed poll is retried at the current pace, as in CPA's native flow.
		login.nextPoll = logins.now().Add(login.interval)
		return loginPending(), nil
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(resp.Body, &token)
	if resp.StatusCode == http.StatusOK {
		logins.remove(state, login)
		if strings.TrimSpace(token.AccessToken) == "" {
			return loginFailed("Meta approved the login but returned no access token."), nil
		}
		return p.completeLogin(ctx, req.HTTPClient, token.AccessToken, token.TokenType, token.ExpiresIn)
	}
	switch token.Error {
	case "authorization_pending", "":
		// An unexpected reply without an error code is retried, as in CPA's native flow.
	case "slow_down":
		login.interval += slowDownIncrement
	case "access_denied":
		logins.remove(state, login)
		return loginFailed("The login request was denied."), nil
	case "expired_token":
		logins.remove(state, login)
		return loginFailed("The login request expired before it was approved."), nil
	default:
		logins.remove(state, login)
		return loginFailed("Meta login failed: " + token.Error), nil
	}
	login.nextPoll = logins.now().Add(login.interval)
	return loginPending(), nil
}

// completeLogin mints the first key like Muse Code does after approval. An
// account Meta refuses a key fails the login; a transient mint failure keeps
// the DCA token so CPA mints on first use through RefreshAuth.
func (p AuthProvider) completeLogin(ctx context.Context, client pluginapi.HostHTTPClient, dcaToken, tokenType string, expiresIn int) (pluginapi.AuthLoginPollResponse, error) {
	dcaToken = strings.TrimSpace(dcaToken)
	minted, errMint := mintAPIKey(ctx, client, firstNonEmpty(p.MintURL, defaultMintURL), dcaToken)
	var mintErr *StatusError
	if errors.As(errMint, &mintErr) && (mintErr.Status == http.StatusUnauthorized || mintErr.Status == http.StatusPaymentRequired || mintErr.Status == http.StatusForbidden) {
		return loginFailed(fmt.Sprintf("Meta signed you in but would not issue a Model API key (HTTP %d). Finish onboarding at https://dev.meta.ai or add a payment method, then log in again.", mintErr.Status)), nil
	}
	now := logins.now().UTC()
	data := map[string]any{"type": "muse", "auth_kind": "oauth", "last_refresh": now.Format(time.RFC3339)}
	if tokenType = strings.TrimSpace(tokenType); tokenType != "" {
		data["token_type"] = tokenType
	}
	if expiresIn > 0 {
		expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
		data["dca_expires_at"], data["dca_expired"] = expiresAt.Unix(), expiresAt.Format(time.RFC3339)
	}
	cred := Credential{DCAToken: dcaToken, BaseURL: DefaultBaseURL}
	message := ""
	if errMint == nil {
		cred.APIKey = strings.TrimSpace(minted.APIKey)
		cred.BaseURL = firstNonEmpty(minted.BaseURL, DefaultBaseURL)
		applyMintedAccount(data, minted)
	} else {
		message = "Signed in; the Model API key will be minted on first use."
	}
	normalizeAuthTokens(data, cred)
	storage, err := json.Marshal(data)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{}, err
	}
	email, _ := data["email"].(string)
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: message, Auth: pluginapi.AuthData{
		Provider:    "muse",
		FileName:    loginFileName(email, dcaToken),
		Label:       firstNonEmpty(email, "Muse"),
		StorageJSON: storage,
		Metadata:    data,
	}}, nil
}

func (p AuthProvider) postForm(ctx context.Context, client pluginapi.HostHTTPClient, path string, form url.Values) (pluginapi.HTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, authHTTPTimeout)
	defer cancel()
	headers := make(http.Header)
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", authFlowUserAgent)
	return client.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     strings.TrimSuffix(firstNonEmpty(p.AuthURL, defaultAuthURL), "/") + path,
		Headers: headers,
		Body:    []byte(form.Encode()),
	})
}

// verificationURL prefers Meta's complete URI. Without one it appends the user
// code as the code parameter, the form Meta's complete URI uses.
func verificationURL(complete, base, userCode string) (string, error) {
	raw := strings.TrimSpace(complete)
	if raw == "" {
		raw = strings.TrimSpace(base)
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", errorf(http.StatusBadGateway, "login_failed", "Meta device authorization response has no usable verification URL")
	}
	if strings.TrimSpace(complete) == "" {
		query := parsed.Query()
		query.Set("code", strings.TrimSpace(userCode))
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// newLoginState returns an opaque state accepted by CPA's OAuth session store.
func newLoginState() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate Muse login state: %w", err)
	}
	return "muse-" + hex.EncodeToString(buf[:]), nil
}

// loginFileName follows CPA's native Meta naming with a muse prefix, so native
// login never overwrites a plugin credential.
func loginFileName(email, dcaToken string) string {
	if clean := strings.TrimSpace(email); clean != "" {
		sanitized := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, clean)
		if len(sanitized) > 120 {
			sanitized = sanitized[:120]
		}
		sum := sha256.Sum256([]byte(clean))
		return fmt.Sprintf("muse-%s-%s.json", sanitized, hex.EncodeToString(sum[:8]))
	}
	sum := sha256.Sum256([]byte(dcaToken))
	return fmt.Sprintf("muse-%s.json", hex.EncodeToString(sum[:8]))
}

// oauthErrorSuffix reports only the OAuth error code, never the response body.
func oauthErrorSuffix(body []byte) string {
	var reply struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &reply) != nil || strings.TrimSpace(reply.Error) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(reply.Error)
}

func durationOr(seconds int, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func loginPending() pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
}

func loginFailed(message string) pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}
}
