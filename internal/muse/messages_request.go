package muse

// Ported from the fork internal/runtime/executor/helps/meta_messages_request.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// MetaMessagesPolicy contains explicit operator choices, not claims of upstream verification.
type MetaMessagesPolicy struct {
	Capabilities []string
	Degrade      MessagesDegradeConfig
}

func (p MetaMessagesPolicy) Allows(capability string) bool {
	for _, value := range p.Capabilities {
		if value == capability {
			return true
		}
	}
	return false
}

// MetaMessagesError returns a request-local Anthropic error without credential side effects.
func MetaMessagesError(status int, code, path, reason string) error {
	typ := "invalid_request_error"
	if status == http.StatusForbidden {
		typ = "permission_error"
	}
	if status >= 500 {
		typ = "api_error"
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{
		"type": typ, "message": fmt.Sprintf("[cpa.meta.messages.%s] %s: %s", code, path, reason)}})
	return &StatusError{Status: status, Code: code, Message: fmt.Sprintf("[cpa.meta.messages.%s] %s: %s", code, path, reason), Body: body, Scope: ScopeRequest}
}

func metaInvalid(path, reason string) error {
	return MetaMessagesError(400, "unsupported", path, reason)
}
func metaUnverified(path, reason string) error {
	return MetaMessagesError(400, "capability_unverified", path, reason)
}

// DecodeMetaMessages rejects duplicate keys, including inside tool schemas and arguments.
func DecodeMetaMessages(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	value, err := decodeMetaValue(dec, "$", 0)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, metaInvalid("$", "expected one JSON object")
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, metaInvalid("$", "expected a JSON object")
	}
	return root, nil
}

func decodeMetaValue(dec *json.Decoder, path string, depth int) (any, error) {
	if depth > 128 {
		return nil, metaInvalid(path, "JSON nesting exceeds 128 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, metaInvalid(path, "invalid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for dec.More() {
			keyToken, errKey := dec.Token()
			key, valid := keyToken.(string)
			if errKey != nil || !valid {
				return nil, metaInvalid(path, "invalid object key")
			}
			if _, exists := m[key]; exists {
				return nil, metaInvalid(path+"."+key, "duplicate JSON key")
			}
			value, errValue := decodeMetaValue(dec, path+"."+key, depth+1)
			if errValue != nil {
				return nil, errValue
			}
			m[key] = value
		}
		if _, err = dec.Token(); err != nil {
			return nil, metaInvalid(path, "invalid object")
		}
		return m, nil
	case '[':
		var values []any
		for dec.More() {
			value, errValue := decodeMetaValue(dec, fmt.Sprintf("%s[%d]", path, len(values)), depth+1)
			if errValue != nil {
				return nil, errValue
			}
			values = append(values, value)
		}
		if _, err = dec.Token(); err != nil {
			return nil, metaInvalid(path, "invalid array")
		}
		if values == nil {
			values = []any{}
		}
		return values, nil
	}
	return nil, metaInvalid(path, "invalid JSON delimiter")
}

func metaKeys(m map[string]any, path, keys string) error {
	allowed := "|" + keys + "|"
	for key := range m {
		if !strings.Contains(allowed, "|"+key+"|") {
			return metaInvalid(path+"."+key, "field cannot be forwarded without changing its meaning")
		}
	}
	return nil
}

func metaString(m map[string]any, key, path string, required bool) error {
	v, exists := m[key]
	if !exists && !required {
		return nil
	}
	s, ok := v.(string)
	if !ok || (required && strings.TrimSpace(s) == "") {
		return metaInvalid(path+"."+key, "expected a nonempty string")
	}
	return nil
}

func metaInteger(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

func metaCache(v any, path string) error {
	m, ok := v.(map[string]any)
	if !ok {
		return metaInvalid(path, "expected an object")
	}
	if err := metaKeys(m, path, "type|ttl"); err != nil {
		return err
	}
	if m["type"] != "ephemeral" {
		return metaInvalid(path+".type", "only ephemeral caching is supported")
	}
	if ttl, exists := m["ttl"]; exists && ttl != "5m" {
		return metaInvalid(path+".ttl", "only 5m is supported; cache-ttl-5m is an explicit lossy option")
	}
	return nil
}

// NormalizeMetaMessages applies only the explicitly selected degradations before payload rules.
func NormalizeMetaMessages(root map[string]any, model string, stream, count bool, p MetaMessagesPolicy) error {
	root["model"] = model
	if !count {
		root["stream"] = stream
	}
	if p.Degrade.ContextManagementDrop {
		delete(root, "context_management")
	}
	if thinking, ok := root["thinking"].(map[string]any); ok {
		if err := metaKeys(thinking, "thinking", "type|budget_tokens"); err != nil {
			return err
		}
		if thinking["type"] == "disabled" && p.Degrade.ThinkingDisabledToLow {
			root["thinking"] = map[string]any{"type": "adaptive"}
			output, ok := root["output_config"].(map[string]any)
			if !ok {
				output = map[string]any{}
				root["output_config"] = output
			}
			if _, exists := output["effort"]; !exists {
				output["effort"] = "low"
			}
		}
		if thinking["type"] == "enabled" && p.Degrade.ThinkingBudgetToEffort {
			budget, valid := metaInteger(thinking["budget_tokens"])
			if !valid || budget < 1024 {
				return metaInvalid("thinking.budget_tokens", "expected an integer of at least 1024")
			}
			root["thinking"] = map[string]any{"type": "adaptive"}
			output, ok := root["output_config"].(map[string]any)
			if !ok {
				output = map[string]any{}
				root["output_config"] = output
			}
			if _, exists := output["effort"]; !exists {
				effort := "low"
				if budget >= 8192 {
					effort = "medium"
				}
				if budget >= 16384 {
					effort = "high"
				}
				if budget >= 32768 {
					effort = "xhigh"
				}
				output["effort"] = effort
			}
		}
	}
	if p.Degrade.CacheTTL5m {
		normalizeMetaCache(root)
	}
	if p.Degrade.ToolSearchInline {
		return inlineMetaToolSearch(root)
	}
	return nil
}

func normalizeMetaCache(value any) {
	switch v := value.(type) {
	case map[string]any:
		if cache, ok := v["cache_control"].(map[string]any); ok && cache["ttl"] == "1h" {
			cache["ttl"] = "5m"
		}
		for key, child := range v {
			if key != "input_schema" && key != "input" && key != "schema" && key != "context_management" {
				normalizeMetaCache(child)
			}
		}
	case []any:
		for _, child := range v {
			normalizeMetaCache(child)
		}
	}
}

func inlineMetaToolSearch(root map[string]any) error {
	tools, ok := root["tools"].([]any)
	if !ok {
		return nil
	}
	kept := make([]any, 0, len(tools))
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			return metaInvalid("tools", "expected tool objects")
		}
		typ, _ := tool["type"].(string)
		if strings.HasPrefix(typ, "tool_search") {
			continue
		}
		if value, exists := tool["defer_loading"]; exists {
			if _, ok := value.(bool); !ok {
				return metaInvalid("tools.defer_loading", "expected a boolean")
			}
		}
		delete(tool, "defer_loading")
		kept = append(kept, tool)
	}
	root["tools"] = kept
	// Historical search blocks require verified bidirectional mapping. Start a fresh session.
	if hasMetaSearchHistory(root["messages"]) {
		return metaUnverified("messages", "tool-search-inline supports fresh histories only; start a new session or enable a verified O1 shape")
	}
	return nil
}

func hasMetaSearchHistory(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		typ, _ := v["type"].(string)
		name, _ := v["name"].(string)
		if strings.Contains(typ, "tool_search") || typ == "tool_reference" || (typ == "server_tool_use" && strings.Contains(name, "tool_search")) {
			return true
		}
		for key, child := range v {
			if key != "input" && hasMetaSearchHistory(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasMetaSearchHistory(child) {
				return true
			}
		}
	}
	return false
}

// ValidateMetaMessages is read-only and must run after the final payload rule barrier.
func ValidateMetaMessages(root map[string]any, count bool, p MetaMessagesPolicy) error {
	if err := metaKeys(root, "$", "model|max_tokens|messages|system|stream|tools|tool_choice|metadata|temperature|top_p|thinking|service_tier|cache_control|output_config|context_management"); err != nil {
		return err
	}
	if err := metaString(root, "model", "$", true); err != nil {
		return err
	}
	if value, exists := root["context_management"]; exists {
		// Operator probes establish acceptance, not whether the requested edits execute.
		if _, ok := value.(map[string]any); !ok {
			return MetaMessagesError(400, "invalid_value", "context_management", "expected an object")
		}
	}
	maxTokens, hasMax := metaInteger(root["max_tokens"])
	if _, exists := root["max_tokens"]; exists && (!hasMax || maxTokens <= 0) {
		return metaInvalid("max_tokens", "expected a positive integer")
	}
	if !count && !hasMax {
		return metaInvalid("max_tokens", "a positive max_tokens is required for generation")
	}
	if value, exists := root["stream"]; exists {
		if _, ok := value.(bool); !ok {
			return metaInvalid("stream", "expected a boolean")
		}
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, exists := root[key]; exists {
			n, ok := value.(json.Number)
			if !ok {
				return metaInvalid(key, "expected a number in [0,1]")
			}
			f, err := n.Float64()
			if err != nil || f < 0 || f > 1 || math.IsNaN(f) {
				return metaInvalid(key, "expected a number in [0,1]")
			}
		}
	}
	if value, exists := root["thinking"]; exists {
		t, ok := value.(map[string]any)
		if !ok {
			return metaInvalid("thinking", "expected an object")
		}
		if err := metaKeys(t, "thinking", "type|budget_tokens"); err != nil {
			return err
		}
		switch t["type"] {
		case "adaptive":
			if _, exists := t["budget_tokens"]; exists {
				return metaInvalid("thinking.budget_tokens", "cannot accompany adaptive thinking")
			}
		case "enabled":
			budget, ok := metaInteger(t["budget_tokens"])
			if !ok || budget < 1024 || (hasMax && budget >= maxTokens) {
				return metaInvalid("thinking.budget_tokens", "must be at least 1024 and smaller than max_tokens when present")
			}
		default:
			return metaInvalid("thinking.type", "supported types are adaptive and enabled; disabled requires explicit degradation")
		}
	}
	if value, exists := root["service_tier"]; exists && value != "auto" && value != "standard_only" {
		return metaInvalid("service_tier", "expected auto or standard_only")
	}
	if value, exists := root["cache_control"]; exists {
		if err := metaCache(value, "cache_control"); err != nil {
			return err
		}
	}
	if value, exists := root["metadata"]; exists {
		m, ok := value.(map[string]any)
		if !ok {
			return metaInvalid("metadata", "expected an object")
		}
		if err := metaKeys(m, "metadata", "user_id"); err != nil {
			return err
		}
		if err := metaString(m, "user_id", "metadata", false); err != nil {
			return err
		}
	}
	if value, exists := root["output_config"]; exists {
		m, ok := value.(map[string]any)
		if !ok {
			return metaInvalid("output_config", "expected an object")
		}
		if err := metaKeys(m, "output_config", "effort|format"); err != nil {
			return err
		}
		if effort, exists := m["effort"]; exists {
			s, ok := effort.(string)
			if !ok {
				return MetaMessagesError(400, "invalid_value", "output_config.effort", "expected one of low, medium, high, xhigh, max, or minimal")
			}
			if s == "minimal" {
				return metaInvalid("output_config.effort", "minimal is rejected by the reported Messages capability probes")
			}
			switch s {
			case "low", "medium", "high", "xhigh":
			case "max":
				if root["model"] == "muse-spark-1.2" {
					return metaInvalid("output_config.effort", "operator probes reported an upstream rejection of max on muse-spark-1.2")
				}
				if !p.Allows("effort.max") {
					return metaUnverified("output_config.effort", "max is not enabled for this model")
				}
			default:
				return MetaMessagesError(400, "invalid_value", "output_config.effort", "expected one of low, medium, high, xhigh, max, or minimal")
			}
		}
		if value, exists := m["format"]; exists {
			f, ok := value.(map[string]any)
			if !ok {
				return metaInvalid("output_config.format", "expected an object")
			}
			if err := metaKeys(f, "output_config.format", "type|schema"); err != nil {
				return err
			}
			if f["type"] != "json_schema" {
				return metaInvalid("output_config.format.type", "expected json_schema")
			}
			if _, ok := f["schema"].(map[string]any); !ok {
				return metaInvalid("output_config.format.schema", "expected a JSON schema object")
			}
		}
	}
	search, deferred, err := validateMetaTools(root, p)
	if err != nil {
		return err
	}
	if search && !deferred {
		return metaInvalid("tools", "tool search requires deferred custom tools")
	}
	if deferred && !search {
		return metaInvalid("tools", "deferred tools require tool search")
	}
	if search {
		if out, ok := root["output_config"].(map[string]any); ok {
			if _, exists := out["format"]; exists {
				return metaInvalid("output_config.format", "cannot combine structured output with tool search")
			}
		}
	}
	if choice, exists := root["tool_choice"]; exists {
		m, ok := choice.(map[string]any)
		if !ok {
			return metaInvalid("tool_choice", "expected an object")
		}
		if err := metaKeys(m, "tool_choice", "type|disable_parallel_tool_use"); err != nil {
			return err
		}
		if m["type"] != "auto" && m["type"] != "any" && m["type"] != "none" {
			return metaInvalid("tool_choice.type", "named tool choice cannot be preserved")
		}
		if value, exists := m["disable_parallel_tool_use"]; exists {
			if _, ok := value.(bool); !ok {
				return metaInvalid("tool_choice.disable_parallel_tool_use", "expected a boolean")
			}
		}
		if m["type"] == "any" {
			tools, _ := root["tools"].([]any)
			if len(tools) == 0 {
				return metaInvalid("tool_choice", "any requires tools")
			}
		}
	}
	if system, exists := root["system"]; exists {
		if _, ok := system.(string); !ok {
			blocks, ok := system.([]any)
			if !ok {
				return metaInvalid("system", "expected text or text blocks")
			}
			for i, value := range blocks {
				m, ok := value.(map[string]any)
				if !ok || m["type"] != "text" {
					return metaInvalid(fmt.Sprintf("system[%d]", i), "system supports only text")
				}
				if err := validateMetaBlock(m, fmt.Sprintf("system[%d]", i), "system", p); err != nil {
					return err
				}
			}
		}
	}
	return validateMetaHistory(root["messages"], p)
}

func validateMetaTools(root map[string]any, p MetaMessagesPolicy) (search, deferred bool, err error) {
	value, exists := root["tools"]
	if !exists {
		return
	}
	tools, ok := value.([]any)
	if !ok {
		return false, false, metaInvalid("tools", "expected an array")
	}
	names := map[string]bool{}
	active := 0
	for i, value := range tools {
		path := fmt.Sprintf("tools[%d]", i)
		m, ok := value.(map[string]any)
		if !ok {
			return false, false, metaInvalid(path, "expected an object")
		}
		typ, _ := m["type"].(string)
		if typ != "" && typ != "custom" {
			switch {
			case typ == "code_execution_20250825" || typ == "tool_search_tool_regex_20251119":
				return false, false, metaInvalid(path+".type", "operator probes reported an upstream rejection for this exact hosted tool type: "+typ)
			case typ == "web_search":
				if !p.Allows("hosted-web-search-o1") {
					return false, false, metaUnverified(path, "hosted web search replay requires an explicit O1 capability")
				}
			case typ == "tool_search":
				if !p.Allows("hosted-tool-search-o1") {
					return false, false, metaUnverified(path, "hosted tool search replay requires an explicit O1 capability")
				}
				if search {
					return false, false, metaInvalid(path, "only one search tool is allowed")
				}
				search = true
			default:
				return false, false, metaUnverified(path+".type", "hosted tool declaration is not implemented; versioned declarations are not silently remapped")
			}
			if err := metaKeys(m, path, "type|name"); err != nil {
				return false, false, err
			}
			active++
			continue
		}
		if err := metaKeys(m, path, "type|name|description|input_schema|cache_control|defer_loading"); err != nil {
			return false, false, err
		}
		if err := metaString(m, "name", path, true); err != nil {
			return false, false, err
		}
		name := m["name"].(string)
		if names[name] {
			return false, false, metaInvalid(path+".name", "duplicate tool name")
		}
		names[name] = true
		if err := metaString(m, "description", path, false); err != nil {
			return false, false, err
		}
		schema, ok := m["input_schema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			return false, false, metaInvalid(path+".input_schema", "expected an object schema")
		}
		isDeferred := false
		if value, exists := m["defer_loading"]; exists {
			b, ok := value.(bool)
			if !ok {
				return false, false, metaInvalid(path+".defer_loading", "expected a boolean")
			}
			isDeferred = b
		}
		if isDeferred {
			if !p.Allows("hosted-tool-search-o1") {
				return false, false, metaUnverified(path+".defer_loading", "tool search is not enabled")
			}
			deferred = true
		} else {
			active++
		}
		if value, exists := m["cache_control"]; exists {
			if isDeferred {
				return false, false, metaInvalid(path, "deferred tools cannot have cache_control")
			}
			if err := metaCache(value, path+".cache_control"); err != nil {
				return false, false, err
			}
		}
	}
	if deferred && active == 0 {
		return false, false, metaInvalid("tools", "at least one tool must be non-deferred")
	}
	return
}
