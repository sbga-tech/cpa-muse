package muse

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Native gates and error envelopes ported from helps/meta_responses_native.go.
func MetaResponsesError(status int, code, field, reason string) error {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "code": "cpa.meta.responses." + code,
		"param": field, "message": reason,
	}})
	return &StatusError{Status: status, Code: "cpa.meta.responses." + code, Body: body}
}

func MetaResponsesCapability(cfg ResponsesConfig, model, capability string) bool {
	for _, key := range []string{"*", model} {
		for _, enabled := range cfg.Capabilities[key] {
			if enabled == capability {
				return true
			}
		}
	}
	return false
}

// ValidateMetaResponsesNative never removes constraints from the normalized request.
func ValidateMetaResponsesNative(body []byte, cfg ResponsesConfig) error {
	root, err := DecodeMetaMessages(body)
	if err != nil {
		return MetaResponsesError(400, "invalid_json", "", "Request must be a JSON object without duplicate keys")
	}
	model, _ := root["model"].(string)
	if strings.TrimSpace(model) == "" {
		return MetaResponsesError(400, "missing_model", "model", "A model is required")
	}
	if value, exists := root["max_output_tokens"]; exists {
		number, ok := value.(json.Number)
		if !ok {
			return MetaResponsesError(400, "invalid_max_output_tokens", "max_output_tokens", "Expected a positive integer")
		}
		count, err := number.Int64()
		if err != nil || count <= 0 {
			return MetaResponsesError(400, "invalid_max_output_tokens", "max_output_tokens", "Expected a positive integer")
		}
	}
	if v, ok := root["store"]; ok && v != false && !MetaResponsesCapability(cfg, model, "stateful") {
		return MetaResponsesError(400, "capability_unverified", "store", "Native Meta mode defaults to stateless replay; stored responses require the stateful capability")
	}
	for _, field := range []string{"previous_response_id", "conversation"} {
		if v, ok := root[field]; ok && v != nil && v != "" && !MetaResponsesCapability(cfg, model, "stateful") {
			return MetaResponsesError(400, "capability_unverified", field, "Stateful continuation is not enabled; send complete input history")
		}
	}
	if v, ok := root["background"]; ok && v != false {
		return MetaResponsesError(400, "unsupported", "background", "Background retrieval is not exposed by this gateway; use foreground SSE")
	}
	if v, ok := root["truncation"]; ok && v != "disabled" {
		return MetaResponsesError(400, "unsupported", "truncation", "Meta requires client-managed history; automatic truncation is not supported")
	}
	if reason, ok := root["reasoning"].(map[string]any); ok {
		if effort, ok := reason["effort"].(string); ok {
			switch effort {
			case "low", "medium", "high", "xhigh":
			case "minimal", "max":
				if !MetaResponsesCapability(cfg, model, "effort."+effort) {
					return MetaResponsesError(400, "capability_unverified", "reasoning.effort", "This effort requires an explicit model capability override")
				}
			default:
				return MetaResponsesError(400, "unsupported", "reasoning.effort", "Unsupported reasoning effort; reasoning is not silently disabled")
			}
		}
	}
	if tools, ok := root["tools"].([]any); ok {
		for i, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return MetaResponsesError(400, "invalid_tool", fmt.Sprintf("tools.%d", i), "Each tool must be an object")
			}
			if err := validateMetaNativeTool(tool, fmt.Sprintf("tools.%d", i), cfg, model); err != nil {
				return err
			}
		}
	}
	input, exists := root["input"]
	if !exists || input == nil {
		return MetaResponsesError(400, "missing_input", "input", "Input is required")
	}
	if _, ok := input.(string); ok {
		return nil
	}
	items, ok := input.([]any)
	if !ok || len(items) == 0 {
		return MetaResponsesError(400, "invalid_input", "input", "Input must be text or a non-empty item array")
	}
	calls := make(map[string]bool)
	var outputs []string
	for i, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			return MetaResponsesError(400, "invalid_input", fmt.Sprintf("input.%d", i), "Each input item must be an object")
		}
		typeName, _ := item["type"].(string)
		switch typeName {
		case "reasoning":
			if encrypted, _ := item["encrypted_content"].(string); encrypted == "" {
				return MetaResponsesError(400, "reasoning_replay_missing", fmt.Sprintf("input.%d.encrypted_content", i), "Replay complete encrypted reasoning items; an item ID alone is insufficient")
			}
		case "function_call":
			callID, _ := item["call_id"].(string)
			arguments, _ := item["arguments"].(string)
			if callID == "" || len(callID) > 64 || !json.Valid([]byte(arguments)) {
				return MetaResponsesError(400, "invalid_tool_call", fmt.Sprintf("input.%d", i), "Tool calls require a call_id of 1-64 bytes and JSON arguments")
			}
			if calls[callID] {
				return MetaResponsesError(400, "duplicate_call_id", fmt.Sprintf("input.%d.call_id", i), "Tool call IDs must be unique")
			}
			calls[callID] = true
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			outputs = append(outputs, callID)
		case "compaction":
			return MetaResponsesError(400, "unsupported", fmt.Sprintf("input.%d.type", i), "OpenAI compaction items are provider-specific; compact to ordinary history on the client")
		}
	}
	if root["previous_response_id"] == nil || root["previous_response_id"] == "" {
		for _, callID := range outputs {
			if !calls[callID] {
				return MetaResponsesError(400, "orphan_tool_output", "input", "Every function_call_output must have its matching function_call in stateless history")
			}
		}
	}
	return nil
}

func validateMetaNativeTool(tool map[string]any, path string, cfg ResponsesConfig, model string) error {
	kind, _ := tool["type"].(string)
	switch kind {
	case "function":
		if name, _ := tool["name"].(string); strings.TrimSpace(name) == "" {
			return MetaResponsesError(400, "invalid_tool", path+".name", "Function tools require a name")
		}
	case "namespace":
		children, ok := tool["tools"].([]any)
		if !ok || len(children) == 0 {
			return MetaResponsesError(400, "invalid_tool", path+".tools", "Namespaces require a non-empty tools array")
		}
		for i, child := range children {
			object, ok := child.(map[string]any)
			if !ok {
				return MetaResponsesError(400, "invalid_tool", path, "Namespace tools must be objects")
			}
			if err := validateMetaNativeTool(object, fmt.Sprintf("%s.tools.%d", path, i), cfg, model); err != nil {
				return err
			}
		}
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "tool_search":
		if !MetaResponsesCapability(cfg, model, "hosted-tools") {
			return MetaResponsesError(400, "capability_unverified", path, "Hosted tools require explicit capability verification")
		}
	case "custom":
		return MetaResponsesError(400, "unsupported", path, "Only the existing apply_patch bridge is supported for custom tools")
	default:
		return MetaResponsesError(400, "capability_unverified", path, "This tool type is not enabled in native Meta mode")
	}
	return nil
}
