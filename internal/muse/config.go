package muse

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultBaseURL is the native Meta Muse API base.
const DefaultBaseURL = "https://api.meta.ai/v1"

// Config is the plugin-owned YAML subtree under plugins.configs.<plugin-id>.
//
// Wire sections mirror the fork's meta-messages, meta-responses, and muse-profile
// settings. Credential and routing policy belong to the CPA host.
type Config struct {
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`

	// Models lists extra model IDs the router claims in addition to the built-in Muse catalog.
	Models []string `yaml:"models,omitempty"`
	// Prefixes are optional client-side model prefixes ("meta" claims "meta/muse-spark-1.3").
	Prefixes []string `yaml:"prefixes,omitempty"`
	// Aliases maps a client model name to an upstream Muse model.
	Aliases map[string]string `yaml:"aliases,omitempty"`

	// MintURL overrides the DCA mint endpoint (tests, staging).
	MintURL string `yaml:"mint-url,omitempty"`
	// AuthURL overrides the Meta OAuth host for device login (tests, staging).
	AuthURL string `yaml:"auth-url,omitempty"`

	// Client overrides the Muse Code client identity stamped on legacy/muse requests.
	Client ClientConfig `yaml:"client,omitempty"`
	// MuseNamespace appends the canonical muse tool namespace to tool-bearing requests (default true).
	MuseNamespace *bool `yaml:"muse-namespace,omitempty"`

	Messages  MessagesConfig  `yaml:"meta-messages,omitempty"`
	Responses ResponsesConfig `yaml:"meta-responses,omitempty"`
	Profile   ProfileConfig   `yaml:"muse-profile,omitempty"`
}

// ClientConfig overrides the captured Muse Code identity.
type ClientConfig struct {
	Version string `yaml:"version,omitempty"`
	Build   string `yaml:"build,omitempty"`
	Surface string `yaml:"surface,omitempty"`
	OS      string `yaml:"os,omitempty"`
	Arch    string `yaml:"arch,omitempty"`
}

// MessagesConfig controls the native Anthropic Messages surface.
type MessagesConfig struct {
	Mode              string `yaml:"mode,omitempty"`
	AllowSubscription bool   `yaml:"allow-subscription,omitempty"`
	CountTokens       string `yaml:"count-tokens,omitempty"`
	// Omitted uses 60 seconds; explicit zero disables only the URL-image idle watchdog.
	URLImageIdleTimeoutSeconds *int                  `yaml:"url-image-idle-timeout-seconds,omitempty"`
	Capabilities               map[string][]string   `yaml:"capabilities,omitempty"`
	MaxRequestBytes            int64                 `yaml:"max-request-bytes,omitempty"`
	ContextLimit               int                   `yaml:"context-limit,omitempty"`
	Degrade                    MessagesDegradeConfig `yaml:"degrade,omitempty"`
}

// MessagesDegradeConfig lists explicit, bounded compatibility concessions.
type MessagesDegradeConfig struct {
	ThinkingDisabledToLow  bool `yaml:"thinking-disabled-to-low,omitempty"`
	ThinkingBudgetToEffort bool `yaml:"thinking-budget-to-effort,omitempty"`
	CacheTTL5m             bool `yaml:"cache-ttl-5m,omitempty"`
	ContextManagementDrop  bool `yaml:"context-management-drop,omitempty"`
	ToolSearchInline       bool `yaml:"tool-search-inline,omitempty"`
}

// ResponsesConfig controls the Responses surface.
type ResponsesConfig struct {
	Mode            string              `yaml:"mode,omitempty"`
	HeaderProfile   string              `yaml:"header-profile,omitempty"`
	Capabilities    map[string][]string `yaml:"capabilities,omitempty"`
	MaxRequestBytes int64               `yaml:"max-request-bytes,omitempty"`
	ContextLimit    int                 `yaml:"context-limit,omitempty"`
}

// ProfileConfig selects the opt-in Muse wire profile engine.
type ProfileConfig struct {
	Enabled      bool                        `yaml:"enabled,omitempty"`
	Profile      string                      `yaml:"profile,omitempty"`
	FillDefaults bool                        `yaml:"fill-defaults,omitempty"`
	Keys         map[string]ProfileKeyConfig `yaml:"keys,omitempty"`
}

// ProfileKeyConfig overrides the profile selection for one downstream API key.
type ProfileKeyConfig struct {
	Enabled      *bool  `yaml:"enabled,omitempty"`
	Profile      string `yaml:"profile,omitempty"`
	FillDefaults *bool  `yaml:"fill-defaults,omitempty"`
}

// ParseConfig decodes the plugin YAML subtree. Empty input yields defaults.
func ParseConfig(raw []byte) (*Config, error) {
	cfg := &Config{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		var document yaml.Node
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return nil, fmt.Errorf("muse plugin config: %w", err)
		}
		if len(document.Content) > 0 && document.Content[0].Kind == yaml.MappingNode {
			fields := document.Content[0].Content
			for i := 0; i < len(fields); i += 2 {
				switch fields[i].Value {
				case "api-keys", "host-credentials":
					return nil, fmt.Errorf("muse plugin config: %s was removed; migrate credentials to type:muse JSON files in CPA's auth-dir", fields[i].Value)
				case "meta-limits":
					return nil, fmt.Errorf("muse plugin config: meta-limits was removed; configure admission and routing policy in CPA instead")
				}
			}
		}
		if err := document.Decode(cfg); err != nil {
			return nil, fmt.Errorf("muse plugin config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.ResponsesMode() {
	case "legacy", "native", "disabled":
	default:
		return fmt.Errorf("muse plugin config: meta-responses.mode must be legacy, native, or disabled")
	}
	switch c.MessagesMode() {
	case "disabled", "enabled", "legacy":
	default:
		return fmt.Errorf("muse plugin config: meta-messages.mode must be disabled, enabled, or legacy")
	}
	switch strings.ToLower(strings.TrimSpace(c.Responses.HeaderProfile)) {
	case "", "generic", "muse":
	default:
		return fmt.Errorf("muse plugin config: meta-responses.header-profile must be generic or muse")
	}
	switch strings.TrimSpace(c.Messages.CountTokens) {
	case "", "local", "upstream-then-local":
	default:
		return fmt.Errorf("muse plugin config: meta-messages.count-tokens must be local or upstream-then-local")
	}
	return nil
}

// ResponsesMode is legacy (codex translation), native, or disabled. Default legacy.
func (c *Config) ResponsesMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.Responses.Mode))
	if mode == "" {
		return "legacy"
	}
	return mode
}

// MessagesMode is disabled, enabled (native /v1/messages), or legacy (Responses translation).
// Default legacy, which matches upstream CPA: Claude-format requests go through Responses.
func (c *Config) MessagesMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.Messages.Mode))
	if mode == "" {
		return "legacy"
	}
	return mode
}

// HeaderProfile is the native Responses header profile: generic or muse. Default generic,
// matching the fork: native requests carry a neutral identity unless muse is chosen.
func (c *Config) HeaderProfile() string {
	profile := strings.ToLower(strings.TrimSpace(c.Responses.HeaderProfile))
	if profile == "" {
		return "generic"
	}
	return profile
}

// InjectMuseNamespace reports whether tool-bearing requests get the canonical muse namespace.
func (c *Config) InjectMuseNamespace() bool {
	return c.MuseNamespace == nil || *c.MuseNamespace
}

// AuthProvider returns the credential lifecycle provider for this configuration.
func (c *Config) AuthProvider() AuthProvider {
	return AuthProvider{MintURL: c.MintURL, AuthURL: c.AuthURL}
}

// ProfileSelection is the resolved Muse profile choice for one request.
type ProfileSelection struct {
	Enabled      bool
	Profile      string
	FillDefaults bool
}

// ResolveProfile applies per-key overrides. principal is the downstream key when known.
func (c *Config) ResolveProfile(principal string) ProfileSelection {
	selection := ProfileSelection{Enabled: c.Profile.Enabled, Profile: strings.TrimSpace(c.Profile.Profile), FillDefaults: c.Profile.FillDefaults}
	if override, ok := c.Profile.Keys[principal]; ok && principal != "" {
		if override.Enabled != nil {
			selection.Enabled = *override.Enabled
		}
		if profile := strings.TrimSpace(override.Profile); profile != "" {
			selection.Profile = profile
		}
		if override.FillDefaults != nil {
			selection.FillDefaults = *override.FillDefaults
		}
	}
	if selection.Profile == "" {
		selection.Profile = defaultMuseProfile
	}
	return selection
}
