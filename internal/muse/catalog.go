package muse

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogJSON is the fork's Muse catalog, with limits measured from the
// official client (1007997 context, 128000 output).
//
//go:embed catalog.json
var catalogJSON []byte

type catalogEntry struct {
	ID                        string   `json:"id"`
	Object                    string   `json:"object"`
	Created                   int64    `json:"created"`
	OwnedBy                   string   `json:"owned_by"`
	Type                      string   `json:"type"`
	DisplayName               string   `json:"display_name"`
	Name                      string   `json:"name"`
	Description               string   `json:"description"`
	ContextLength             int64    `json:"context_length"`
	MaxCompletionTokens       int64    `json:"max_completion_tokens"`
	SupportedInputModalities  []string `json:"supportedInputModalities"`
	SupportedOutputModalities []string `json:"supportedOutputModalities"`
	Thinking                  *struct {
		Levels []string `json:"levels"`
	} `json:"thinking"`
}

var catalog = mustLoadCatalog()

func mustLoadCatalog() []catalogEntry {
	var entries []catalogEntry
	if err := json.Unmarshal(catalogJSON, &entries); err != nil {
		panic("muse: invalid embedded catalog: " + err.Error())
	}
	return entries
}

// Catalog returns the Muse catalog plus configured extra model IDs.
func (c *Config) Catalog() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(catalog)+len(c.Models))
	seen := make(map[string]bool, len(catalog))
	for _, entry := range catalog {
		info := pluginapi.ModelInfo{
			ID:                        entry.ID,
			Object:                    firstNonEmpty(entry.Object, "model"),
			Created:                   entry.Created,
			OwnedBy:                   firstNonEmpty(entry.OwnedBy, "meta"),
			Type:                      firstNonEmpty(entry.Type, "meta"),
			DisplayName:               entry.DisplayName,
			Name:                      entry.Name,
			Description:               entry.Description,
			ContextLength:             entry.ContextLength,
			MaxCompletionTokens:       entry.MaxCompletionTokens,
			InputTokenLimit:           entry.ContextLength,
			OutputTokenLimit:          entry.MaxCompletionTokens,
			SupportedInputModalities:  append([]string(nil), entry.SupportedInputModalities...),
			SupportedOutputModalities: append([]string(nil), entry.SupportedOutputModalities...),
		}
		if entry.Thinking != nil {
			info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), entry.Thinking.Levels...)}
		}
		out = append(out, info)
		seen[entry.ID] = true
	}
	for _, id := range c.Models {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: "meta", Type: "meta", Name: id, DisplayName: id, UserDefined: true})
	}
	return out
}

// RouteModel decides whether a client-requested model belongs to the plugin.
// It accepts the catalog IDs, configured extras, aliases, configured prefixes
// and a CPA thinking suffix such as "muse-spark-1.3(high)".
func (c *Config) RouteModel(requested string) (string, bool) {
	name := strings.TrimSpace(requested)
	if name == "" {
		return "", false
	}
	base := stripThinkingSuffix(name)
	for _, prefix := range c.Prefixes {
		prefix = strings.Trim(strings.TrimSpace(prefix), "/")
		if prefix != "" && strings.HasPrefix(base, prefix+"/") {
			base = strings.TrimPrefix(base, prefix+"/")
			break
		}
	}
	if target, ok := c.Aliases[base]; ok && strings.TrimSpace(target) != "" {
		return strings.TrimSpace(target), true
	}
	for _, model := range c.Catalog() {
		if strings.EqualFold(model.ID, base) {
			return model.ID, true
		}
	}
	return "", false
}

// UpstreamModel strips a CPA thinking suffix the host left on the model name.
func UpstreamModel(model string) string {
	return stripThinkingSuffix(strings.TrimSpace(model))
}

func stripThinkingSuffix(name string) string {
	if strings.HasSuffix(name, ")") {
		if open := strings.LastIndex(name, "("); open > 0 {
			return name[:open]
		}
	}
	return name
}

// downstreamPrincipal identifies the downstream API key for per-key profile
// overrides. The host passes the caller scope (a hash of the client key) in
// metadata; configured keys are matched by the same hash, and an explicit
// bearer/x-api-key header is used when the host forwards it.
func downstreamPrincipal(cfg *Config, req pluginapi.ExecutorRequest) string {
	if len(cfg.Profile.Keys) == 0 {
		return ""
	}
	if key := bearerKey(req.Headers); key != "" {
		if _, ok := cfg.Profile.Keys[key]; ok {
			return key
		}
	}
	scope, _ := req.Metadata["caller_scope"].(string)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return ""
	}
	for key := range cfg.Profile.Keys {
		if session.CallerScope(key) == scope {
			return key
		}
	}
	return ""
}

func bearerKey(h http.Header) string {
	if h == nil {
		return ""
	}
	if v := strings.TrimSpace(h.Get("Authorization")); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return strings.TrimSpace(h.Get("X-Api-Key"))
}
