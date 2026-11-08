package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	defaultMintURL = "https://api.meta.ai/muse-code/key"
	// authFlowUserAgent is Muse Code's authflow client identity, used for device
	// login and key minting independently of the inference client version.
	authFlowUserAgent = "muse-build/0.1.0"
	// authHTTPTimeout bounds each credential-acquisition request like CPA's native
	// Meta login and mint. Refresh mints are not tied to a downstream request, so
	// nothing else cancels them.
	authHTTPTimeout = 30 * time.Second
)

// AuthProvider translates Meta credentials to host-managed Muse credentials:
// device login, auth-file parsing, and key minting on refresh. CPA owns refresh
// scheduling, persistence, and login sessions.
type AuthProvider struct{ MintURL, AuthURL string }

var _ pluginapi.AuthProvider = AuthProvider{}

func (AuthProvider) Identifier() string { return "muse" }

func (AuthProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var data map[string]any
	if err := json.Unmarshal(req.RawJSON, &data); err != nil {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("decode Muse auth: %w", err)
	}
	// CPA dispatches auth files by type: type meta stays with the native executor.
	kind, _ := data["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(kind), "muse") {
		return pluginapi.AuthParseResponse{}, nil
	}
	cred := credentialFromJSON(req.RawJSON)
	if cred.APIKey == "" && cred.DCAToken == "" {
		return pluginapi.AuthParseResponse{Handled: true}, errorf(http.StatusUnauthorized, "auth_unavailable", "Muse auth requires api_key, access_token, or dca_token")
	}
	data["type"] = "muse"
	normalizeAuthTokens(data, cred)
	storage, err := json.Marshal(data)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	attributes := make(map[string]string)
	if raw, ok := data["attributes"].(map[string]any); ok {
		for key, value := range raw {
			if value, ok := value.(string); ok {
				attributes[key] = value
			}
		}
	}
	// CPA reserves this attribute for native OpenAI compatibility executors.
	delete(attributes, "base_url")
	for _, key := range []string{"priority", "weight", "session_affinity"} {
		if value, ok := data[key]; ok {
			attributes[key] = fmt.Sprint(value)
		}
	}
	get := func(key string) string { value, _ := data[key].(string); return strings.TrimSpace(value) }
	disabled, _ := data["disabled"].(bool)
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: "muse", FileName: req.FileName, Label: firstNonEmpty(get("label"), get("email"), get("name")),
		Prefix: get("prefix"), ProxyURL: get("proxy_url"), Disabled: disabled,
		StorageJSON: storage, Metadata: data, Attributes: attributes,
	}}, nil
}

func normalizeAuthTokens(data map[string]any, cred Credential) {
	if cred.APIKey != "" {
		data["api_key"], data["access_token"] = cred.APIKey, cred.APIKey
	}
	if cred.DCAToken != "" {
		data["dca_token"] = cred.DCAToken
		// CPA's generic unauthorized-refresh gate recognizes refresh_token.
		data["refresh_token"] = cred.DCAToken
	}
	if cred.BaseURL != "" {
		data["base_url"] = cred.BaseURL
	}
}

func (p AuthProvider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	if strings.TrimSpace(req.AuthID) == "" || !strings.EqualFold(strings.TrimSpace(req.AuthProvider), "muse") {
		return pluginapi.AuthRefreshResponse{}, errorf(http.StatusUnauthorized, "auth_unavailable", "Muse refresh requires a host-selected muse credential")
	}
	if req.HTTPClient == nil {
		return pluginapi.AuthRefreshResponse{}, errorf(http.StatusServiceUnavailable, "http_client_unavailable", "Muse refresh requires the host HTTP client")
	}
	if err := perAuthProxyError(req.StorageJSON, req.Metadata); err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	data := make(map[string]any)
	if len(req.StorageJSON) > 0 {
		if err := json.Unmarshal(req.StorageJSON, &data); err != nil {
			return pluginapi.AuthRefreshResponse{}, fmt.Errorf("decode Muse auth storage: %w", err)
		}
		if data == nil {
			return pluginapi.AuthRefreshResponse{}, fmt.Errorf("Muse auth storage must be a JSON object")
		}
	}
	for key, value := range req.Metadata {
		data[key] = value
	}
	cred := materializeCredential(req.StorageJSON, req.Metadata, req.Attributes)
	if cred.DCAToken == "" {
		return pluginapi.AuthRefreshResponse{}, &StatusError{Status: http.StatusUnauthorized, Code: "auth_unavailable", Message: "selected Muse credential has no DCA token to refresh", Scope: ScopeCredential}
	}
	minted, err := mintAPIKey(ctx, req.HTTPClient, firstNonEmpty(p.MintURL, defaultMintURL), cred.DCAToken)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	cred.APIKey = strings.TrimSpace(minted.APIKey)
	cred.BaseURL = firstNonEmpty(minted.BaseURL, cred.BaseURL)
	data["type"] = "muse"
	normalizeAuthTokens(data, cred)
	data["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	applyMintedAccount(data, minted)
	delete(data, "expired")
	attributes := make(map[string]string, len(req.Attributes))
	for key, value := range req.Attributes {
		attributes[key] = value
	}
	delete(attributes, "base_url")
	for _, key := range []string{"api_key", "api-key", "access_token"} {
		if _, ok := attributes[key]; ok {
			attributes[key] = cred.APIKey
		}
	}
	for _, key := range []string{"base-url", "api_base_url"} {
		if _, ok := attributes[key]; ok {
			attributes[key] = cred.BaseURL
		}
	}
	storage, err := json.Marshal(data)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	get := func(key string) string { value, _ := data[key].(string); return value }
	disabled, _ := data["disabled"].(bool)
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
		Provider: "muse", ID: req.AuthID, Label: firstNonEmpty(get("label"), get("email"), get("name")),
		Prefix: get("prefix"), ProxyURL: get("proxy_url"), Disabled: disabled,
		StorageJSON: storage, Metadata: data, Attributes: attributes,
	}}, nil
}

type mintedKey struct {
	APIKey           string `json:"api_key"`
	BaseURL          string `json:"base_url"`
	UserEmail        string `json:"user_email"`
	UserFullName     string `json:"user_full_name"`
	SubsTierName     string `json:"subs_tier_name"`
	SubsTierID       string `json:"subs_tier_id"`
	IsSubsActive     bool   `json:"is_subs_active"`
	HasPaymentMethod bool   `json:"has_payment_method"`
}

// applyMintedAccount records the account and subscription fields a mint returns.
func applyMintedAccount(data map[string]any, minted mintedKey) {
	data["is_subs_active"], data["has_payment_method"] = minted.IsSubsActive, minted.HasPaymentMethod
	for key, value := range map[string]string{"email": minted.UserEmail, "name": minted.UserFullName, "subs_tier_name": minted.SubsTierName, "subs_tier_id": minted.SubsTierID} {
		if value != "" {
			data[key] = value
		}
	}
}

func mintAPIKey(ctx context.Context, client pluginapi.HostHTTPClient, mintURL, dcaToken string) (mintedKey, error) {
	ctx, cancel := context.WithTimeout(ctx, authHTTPTimeout)
	defer cancel()
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+dcaToken)
	headers.Set("User-Agent", authFlowUserAgent)
	headers.Set("X-Api-Version", "1.0.0")
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: mintURL, Headers: headers, Body: []byte(`{"onboard":false}`)})
	if err != nil {
		return mintedKey{}, &StatusError{Status: http.StatusBadGateway, Code: "mint_failed", Message: "meta auth: mint request failed: " + err.Error(), Scope: ScopeModel}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		scope := ScopeModel
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			scope = ScopeCredential
		}
		// Never echo mint response bodies: they may contain credential material.
		return mintedKey{}, &StatusError{Status: resp.StatusCode, Code: "mint_failed", Message: fmt.Sprintf("meta auth: mint key failed (HTTP %d)", resp.StatusCode), Scope: scope}
	}
	var minted mintedKey
	if err := json.Unmarshal(resp.Body, &minted); err != nil || strings.TrimSpace(minted.APIKey) == "" {
		return mintedKey{}, &StatusError{Status: http.StatusBadGateway, Code: "mint_failed", Message: "meta auth: mint response missing api_key", Scope: ScopeModel}
	}
	return minted, nil
}
