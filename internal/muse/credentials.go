package muse

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// Credential is the wire view of the one credential selected by CPA.
// It contains no selection, admission, cooldown, or persistence state.
type Credential struct {
	ID       string
	Label    string
	BaseURL  string
	APIKey   string
	DCAToken string
	Headers  map[string]string
}

func (c *Credential) Subscription() bool {
	return c.DCAToken != "" || strings.HasPrefix(c.APIKey, "LLM|")
}

// credentialFromJSON accepts Meta storage and host metadata shapes.
func credentialFromJSON(raw []byte) Credential {
	get := func(path string) string { return strings.TrimSpace(gjson.GetBytes(raw, path).String()) }
	cred := Credential{BaseURL: firstNonEmpty(get("base_url"), get("api_base_url"), get("base-url")), Headers: make(map[string]string)}
	for _, candidate := range []string{get("api_key"), get("access_token"), get("api-key")} {
		if candidate != "" && !strings.HasPrefix(candidate, "dca:") {
			cred.APIKey = candidate
			break
		}
	}
	cred.DCAToken = firstNonEmpty(get("dca_token"), get("dca-token"))
	if cred.DCAToken == "" {
		if token := get("access_token"); strings.HasPrefix(token, "dca:") {
			cred.DCAToken = token
		}
	}
	gjson.GetBytes(raw, "headers").ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.String && strings.TrimSpace(key.String()) != "" {
			cred.Headers[key.String()] = value.String()
		}
		return true
	})
	return cred
}

// selectedCredential never discovers or substitutes another account. Mutable
// host metadata overlays persisted storage; routing attributes are authoritative.
func selectedCredential(req pluginapi.ExecutorRequest) (*Credential, error) {
	if strings.TrimSpace(req.AuthID) == "" || !strings.EqualFold(strings.TrimSpace(req.AuthProvider), "muse") {
		return nil, errorf(http.StatusUnauthorized, "auth_unavailable", "Muse requires a host-selected muse credential")
	}
	if req.HTTPClient == nil {
		return nil, errorf(http.StatusServiceUnavailable, "http_client_unavailable", "Muse requires the host-selected HTTP client")
	}
	if err := perAuthProxyError(req.StorageJSON, req.AuthMetadata); err != nil {
		return nil, err
	}
	if len(req.StorageJSON) > 0 && !gjson.ValidBytes(req.StorageJSON) {
		return nil, errorf(http.StatusUnauthorized, "auth_unavailable", "selected Muse credential storage is not valid JSON")
	}
	cred := materializeCredential(req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	cred.ID = req.AuthID
	cred.Label = req.AuthID
	if cred.APIKey == "" {
		return nil, &StatusError{Status: http.StatusUnauthorized, Code: "auth_unavailable", Message: "selected Muse credential requires provider refresh before execution", Scope: ScopeCredential}
	}
	return &cred, nil
}

func materializeCredential(storage []byte, metadata map[string]any, attributes map[string]string) Credential {
	cred := credentialFromJSON(storage)
	overlay := func(raw []byte) {
		next := credentialFromJSON(raw)
		cred.APIKey = firstNonEmpty(next.APIKey, cred.APIKey)
		cred.DCAToken = firstNonEmpty(next.DCAToken, cred.DCAToken)
		cred.BaseURL = firstNonEmpty(next.BaseURL, cred.BaseURL)
		for key, value := range next.Headers {
			cred.Headers[key] = value
		}
	}
	if raw, err := json.Marshal(metadata); err == nil {
		overlay(raw)
	}
	if raw, err := json.Marshal(attributes); err == nil {
		overlay(raw)
	}
	if raw := attributes["headers"]; raw != "" {
		var headers map[string]string
		if json.Unmarshal([]byte(raw), &headers) == nil {
			for key, value := range headers {
				cred.Headers[key] = value
			}
		}
	}
	for key, value := range attributes {
		if strings.HasPrefix(strings.ToLower(key), "header:") && len(key) > len("header:") {
			cred.Headers[key[len("header:"):]] = value
		}
	}
	return cred
}

// perAuthProxyError refuses credentials with their own proxy_url. Stock CPA
// v8.0.54 rebuilds dynamic-plugin HTTP clients without the selected auth, so
// such traffic would leave through the global proxy instead. Forbidden makes
// CPA cool this credential down and fail over rather than refresh it.
func perAuthProxyError(storage []byte, metadata map[string]any) error {
	proxy, _ := metadata["proxy_url"].(string)
	if strings.TrimSpace(proxy) == "" && strings.TrimSpace(gjson.GetBytes(storage, "proxy_url").String()) == "" {
		return nil
	}
	return &StatusError{Status: http.StatusForbidden, Code: "auth_proxy_unsupported", Scope: ScopeCredential,
		Message: "selected Muse credential sets proxy_url, which this CPA host cannot apply to plugin traffic; remove proxy_url or use the global proxy"}
}

func (c *Call) Credential() (*Credential, error) { return selectedCredential(c.Req) }
