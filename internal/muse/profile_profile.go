package muse

import (
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// Profile engine ported from internal/museprofile in the Enhanced fork.
const maxProfileBytes = 64 << 10
const defaultMuseProfile = "profiles/muse-linux-exec-1.4.14.json"

//go:embed profiles/*.json
var museProfiles embed.FS

type profile struct {
	SchemaVersion int                        `json:"schema_version"`
	ID            string                     `json:"id"`
	StaticHeaders map[string]string          `json:"static_headers"`
	Defaults      map[string]json.RawMessage `json:"defaults"`
}

var staticHeaderNames = map[string]bool{
	"User-Agent": true, "X-Client-Id": true, "X-Tbh-Agent-Role": true,
	"X-Tbh-Contributor-Mode": true, "Accept": true, "Accept-Encoding": true, "Content-Type": true,
}

// ValidateCustomHeaders also supports the declared hardening on the disabled path.
func ValidateCustomHeaders(headers http.Header) error {
	// Only header names the profile itself resolves unconditionally are rejected here. Names a
	// profile may legitimately declare (User-Agent, X-Client-Id, X-Tbh-Agent-Role, ...) are left to
	// validateProfileHeaders, which accepts them when the value matches the selected profile.
	for name := range headers {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		switch canonical {
		case "Authorization", "Proxy-Authorization", "X-Api-Key", "X-Tbh-Session-Id", "X-Meta-Ai-Gateway-Session-Id", "X-Tbh-Thread-Id", "X-Tbh-Root-Session-Id", "Traceparent":
			return identityConflict()
		}
	}
	return nil
}

func loadProfile(path string) (*profile, error) {
	file, err := museProfiles.Open(path)
	if err != nil {
		file, err = os.Open(path)
	}
	if err != nil {
		return nil, invalidProfile()
	}
	data, errRead := io.ReadAll(io.LimitReader(file, maxProfileBytes+1))
	errClose := file.Close()
	if errRead != nil || errClose != nil || len(data) > maxProfileBytes {
		return nil, invalidProfile()
	}
	if _, err := decodeObject(data); err != nil {
		return nil, invalidProfile()
	}
	var result profile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.SchemaVersion != 1 || strings.TrimSpace(result.ID) == "" || len(result.ID) > 128 {
		return nil, invalidProfile()
	}
	canonicalHeaders := make(map[string]string, len(result.StaticHeaders))
	for name, value := range result.StaticHeaders {
		canonical := http.CanonicalHeaderKey(name)
		if !staticHeaderNames[canonical] || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) || strings.TrimSpace(value) == "" {
			return nil, invalidProfile()
		}
		if _, duplicate := canonicalHeaders[canonical]; duplicate {
			return nil, invalidProfile()
		}
		canonicalHeaders[canonical] = value
	}
	for _, required := range []string{"User-Agent", "X-Client-Id", "X-Tbh-Agent-Role", "Content-Type", "Accept", "Accept-Encoding"} {
		if canonicalHeaders[required] == "" {
			return nil, invalidProfile()
		}
	}
	result.StaticHeaders = canonicalHeaders
	for _, forbidden := range []string{"model", "input", "instructions", "tools", "prompt_cache_key"} {
		if _, present := result.Defaults[forbidden]; present {
			return nil, invalidProfile()
		}
	}
	return &result, nil
}

func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return nil, invalidBody()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, invalidBody()
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, invalidBody()
	}
	return object, nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 256 {
		return invalidBody()
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			name, ok := token.(string)
			if err != nil || !ok || seen[name] {
				return invalidBody()
			}
			seen[name] = true
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return invalidBody()
	}
	_, err = decoder.Token()
	return err
}

type insertion struct {
	path  string
	value json.RawMessage
}

func defaultInsertions(body, defaults map[string]json.RawMessage, prefix string) []insertion {
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result []insertion
	for _, key := range keys {
		path := escapePathKey(key)
		if prefix != "" {
			path = prefix + "." + path
		}
		value, exists := body[key]
		if !exists {
			result = append(result, insertion{path: path, value: bytes.Clone(defaults[key])})
			continue
		}
		var current, fallback map[string]json.RawMessage
		if json.Unmarshal(value, &current) == nil && current != nil && json.Unmarshal(defaults[key], &fallback) == nil && fallback != nil {
			result = append(result, defaultInsertions(current, fallback, path)...)
		}
	}
	return result
}

func escapePathKey(key string) string {
	return strings.NewReplacer("\\", "\\\\", ".", "\\.", ":", "\\:", "*", "\\*", "?", "\\?").Replace(key)
}
