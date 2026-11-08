package muse

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Captured from Muse Code 1.4.14-R7309.1. Subscription keys answer 403
// client_attestation_denied when requests still look like muse-build/1.3.0.
const (
	defaultClientVersion = "1.4.14"
	defaultClientBuild   = "dec7d7497fecdad135ec5835a8161dbc92e55b58"
	defaultClientSurface = "tbh:exec"
)

// genericUserAgent is the neutral identity of the native generic header profile.
const genericUserAgent = "cli-proxy-api/meta-dual-protocol"

// clientIdentity is the Muse Code client fingerprint stamped on upstream requests.
type clientIdentity struct {
	userAgent string
	surface   string
}

func (c *Config) identity() clientIdentity {
	version := firstNonEmpty(c.Client.Version, defaultClientVersion)
	build := firstNonEmpty(c.Client.Build, defaultClientBuild)
	surface := firstNonEmpty(c.Client.Surface, defaultClientSurface)
	goos := firstNonEmpty(c.Client.OS, runtime.GOOS)
	if goos == "darwin" {
		goos = "macos"
	}
	arch := firstNonEmpty(c.Client.Arch, runtime.GOARCH)
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return clientIdentity{
		userAgent: fmt.Sprintf("muse-build/%s (non-interactive; %s-%s; build %s)", version, goos, arch, build),
		surface:   surface,
	}
}

// stableSessionID returns a caller-stable session identity when the client sent one.
// X-Claude-Code-Session-Id is accepted only as a UUID so arbitrary strings never
// become upstream session identity.
func stableSessionID(clientHeaders http.Header) (string, bool) {
	if clientHeaders == nil {
		return "", false
	}
	for _, name := range []string{"X-Tbh-Session-Id", "X-Meta-Ai-Gateway-Session-Id"} {
		if v := strings.TrimSpace(clientHeaders.Get(name)); v != "" {
			return v, true
		}
	}
	if v := strings.TrimSpace(clientHeaders.Get("X-Claude-Code-Session-Id")); v != "" {
		if _, err := uuid.Parse(v); err == nil {
			return v, true
		}
	}
	return "", false
}

// sessionID returns the stable session id or a fresh UUIDv7 for this request.
func sessionID(clientHeaders http.Header) string {
	if id, ok := stableSessionID(clientHeaders); ok {
		return id
	}
	return uuid.Must(uuid.NewV7()).String()
}

// applyMuseIdentity stamps the Muse Code client headers and session set on req.
// The same session id is used for the session, gateway, thread and root headers
// so a stable caller session keeps upstream cache affinity across turns.
func applyMuseIdentity(h http.Header, id clientIdentity, session, model string) {
	h.Set("User-Agent", id.userAgent)
	h.Set("X-Client-Id", id.surface)
	h.Set("X-Tbh-Session-Id", session)
	h.Set("X-Meta-Ai-Gateway-Session-Id", session)
	h.Set("X-Tbh-Thread-Id", session)
	h.Set("X-Tbh-Root-Session-Id", session)
	h.Set("X-Tbh-Agent-Role", "main")
	// Muse Code derives this flag from the resolved model, not the account.
	switch {
	case len(model) > len("-contributor") && strings.HasSuffix(model, "-contributor"):
		h.Set("X-Tbh-Contributor-Mode", "1")
	case strings.HasSuffix(model, "-internal"):
		h.Del("X-Tbh-Contributor-Mode")
	default:
		h.Set("X-Tbh-Contributor-Mode", "0")
	}
	h.Set("Traceparent", traceparent(session))
}

// traceparent builds a W3C trace context whose trace id follows the session.
func traceparent(session string) string {
	traceID := strings.ReplaceAll(session, "-", "")
	if len(traceID) != 32 {
		traceID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	var span [8]byte
	_, _ = rand.Read(span[:])
	return fmt.Sprintf("00-%s-%s-01", traceID, hex.EncodeToString(span[:]))
}

// ensurePromptCacheKey sets prompt_cache_key to tbh:main:<session> when absent.
func ensurePromptCacheKey(body []byte, session string) []byte {
	if len(body) == 0 || strings.TrimSpace(session) == "" {
		return body
	}
	if v := gjson.GetBytes(body, "prompt_cache_key"); v.Exists() && strings.TrimSpace(v.String()) != "" {
		return body
	}
	out, err := sjson.SetBytes(body, "prompt_cache_key", "tbh:main:"+session)
	if err != nil {
		return body
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
