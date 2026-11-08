package muse

// Ported from the fork internal/runtime/executor/helps/meta_messages_content.go.

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

func validateMetaHistory(value any, p MetaMessagesPolicy) error {
	messages, ok := value.([]any)
	if !ok || len(messages) == 0 {
		return metaInvalid("messages", "expected a nonempty array")
	}
	pending := map[string]bool{}
	used := map[string]bool{}
	for i, value := range messages {
		path := fmt.Sprintf("messages[%d]", i)
		message, ok := value.(map[string]any)
		if !ok {
			return metaInvalid(path, "expected an object")
		}
		if err := metaKeys(message, path, "role|content"); err != nil {
			return err
		}
		role, ok := message["role"].(string)
		if !ok || (role != "user" && role != "assistant") {
			return metaInvalid(path+".role", "expected user or assistant")
		}
		if len(pending) > 0 && role != "user" {
			return metaInvalid(path, "a tool-use turn must be followed by user tool results")
		}
		content, exists := message["content"]
		if !exists {
			return metaInvalid(path+".content", "content is required")
		}
		if _, ok := content.(string); ok {
			if len(pending) > 0 {
				return metaInvalid(path, "missing tool results from preceding assistant turn")
			}
			continue
		}
		blocks, ok := content.([]any)
		if !ok || len(blocks) == 0 {
			return metaInvalid(path+".content", "expected text or a nonempty content array")
		}
		seenNonResult := false
		serverCalls := map[string]string{}
		selectedCustomCall := map[string]bool{}
		for j, value := range blocks {
			bp := fmt.Sprintf("%s.content[%d]", path, j)
			b, ok := value.(map[string]any)
			if !ok {
				return metaInvalid(bp, "expected an object")
			}
			if err := validateMetaBlock(b, bp, role, p); err != nil {
				return err
			}
			switch b["type"] {
			case "tool_use":
				id := b["id"].(string)
				if used[id] {
					return metaInvalid(bp+".id", "duplicate tool-use ID")
				}
				used[id] = true
				pending[id] = true
				for serverID, name := range serverCalls {
					if name == "tool_search" {
						selectedCustomCall[serverID] = true
					}
				}
			case "tool_result":
				if seenNonResult {
					return metaInvalid(bp, "tool results must precede other user content")
				}
				id := b["tool_use_id"].(string)
				if !pending[id] {
					return metaInvalid(bp+".tool_use_id", "result has no pending tool call or was already supplied")
				}
				delete(pending, id)
			case "server_tool_use":
				id := b["id"].(string)
				if used[id] {
					return metaInvalid(bp+".id", "duplicate server-tool ID")
				}
				used[id] = true
				serverCalls[id] = b["name"].(string)
			case "web_search_tool_result", "tool_search_tool_result":
				id := b["tool_use_id"].(string)
				name, exists := serverCalls[id]
				if !exists {
					return metaInvalid(bp+".tool_use_id", "hosted result has no preceding call in this assistant turn")
				}
				if (b["type"] == "web_search_tool_result" && name != "web_search") || (b["type"] == "tool_search_tool_result" && name != "tool_search") {
					return metaInvalid(bp, "hosted result does not match the call type")
				}
				delete(serverCalls, id)
			default:
				seenNonResult = true
			}
		}
		if role == "user" && len(pending) > 0 {
			return metaInvalid(path, "not all pending tool calls have a result")
		}
		if len(serverCalls) > 0 {
			// Native Meta search can select a custom call without returning a search-result block.
			// Its custom call still requires the immediately following user tool_result.
			for serverID, name := range serverCalls {
				if role != "assistant" || name != "tool_search" || !p.Allows("hosted-tool-search-o1") || !selectedCustomCall[serverID] || len(pending) == 0 {
					return metaUnverified(path, "unfinished hosted-tool continuations are not enabled")
				}
			}
		}
	}
	if len(pending) > 0 {
		return metaInvalid("messages", "tool results are required before continuing generation")
	}
	return nil
}

func validateMetaBlock(b map[string]any, path, role string, p MetaMessagesPolicy) error {
	typ, ok := b["type"].(string)
	if !ok {
		return metaInvalid(path+".type", "expected a content block type")
	}
	if cache, exists := b["cache_control"]; exists {
		if err := metaCache(cache, path+".cache_control"); err != nil {
			return err
		}
	}
	switch typ {
	case "text":
		if err := metaKeys(b, path, "type|text|cache_control|citations"); err != nil {
			return err
		}
		if _, ok := b["text"].(string); !ok {
			return metaInvalid(path+".text", "expected a string")
		}
		if citations, exists := b["citations"]; exists {
			values, ok := citations.([]any)
			if !ok {
				return metaInvalid(path+".citations", "expected an array")
			}
			for _, value := range values {
				citation, ok := value.(map[string]any)
				if !ok {
					return metaInvalid(path+".citations", "expected citation objects")
				}
				if err := metaString(citation, "type", path+".citations", true); err != nil {
					return err
				}
			}
		}
	case "image", "document", "video":
		if role != "user" && role != "tool_result" {
			return metaInvalid(path, "media blocks are only accepted in user content or tool results")
		}
		if err := metaKeys(b, path, "type|source|cache_control|title|context|citations"); err != nil {
			return err
		}
		if err := metaString(b, "title", path, false); err != nil {
			return err
		}
		if err := metaString(b, "context", path, false); err != nil {
			return err
		}
		if value, exists := b["citations"]; exists {
			m, ok := value.(map[string]any)
			if !ok {
				return metaInvalid(path+".citations", "expected an object")
			}
			if err := metaKeys(m, path+".citations", "enabled"); err != nil {
				return err
			}
			if _, ok := m["enabled"].(bool); !ok {
				return metaInvalid(path+".citations.enabled", "expected a boolean")
			}
		}
		if err := validateMetaSource(b["source"], path+".source", typ, p); err != nil {
			return err
		}
	case "tool_use", "server_tool_use":
		if role != "assistant" {
			return metaInvalid(path, "tool calls belong to assistant messages")
		}
		if err := metaKeys(b, path, "type|id|name|input|cache_control"); err != nil {
			return err
		}
		if err := metaString(b, "id", path, true); err != nil {
			return err
		}
		if err := metaString(b, "name", path, true); err != nil {
			return err
		}
		if _, ok := b["input"].(map[string]any); !ok {
			return metaInvalid(path+".input", "expected an object")
		}
		if typ == "server_tool_use" {
			name := b["name"].(string)
			if !((name == "web_search" && p.Allows("hosted-web-search-o1")) || (name == "tool_search" && p.Allows("hosted-tool-search-o1"))) {
				return metaUnverified(path, "hosted-tool history requires an explicitly enabled O1 shape")
			}
		}
	case "tool_result":
		if role != "user" {
			return metaInvalid(path, "tool results belong to user messages")
		}
		if err := metaKeys(b, path, "type|tool_use_id|content|is_error|cache_control"); err != nil {
			return err
		}
		if err := metaString(b, "tool_use_id", path, true); err != nil {
			return err
		}
		if value, exists := b["is_error"]; exists {
			if _, ok := value.(bool); !ok {
				return metaInvalid(path+".is_error", "expected a boolean")
			}
		}
		if content, exists := b["content"]; exists {
			if _, ok := content.(string); !ok {
				blocks, ok := content.([]any)
				if !ok {
					return metaInvalid(path+".content", "expected a string or content array")
				}
				for i, value := range blocks {
					child, ok := value.(map[string]any)
					if !ok {
						return metaInvalid(path+".content", "expected content objects")
					}
					if child["type"] != "text" && child["type"] != "image" && child["type"] != "document" && child["type"] != "video" {
						return metaUnverified(path+".content", "nested tool or reference blocks are not enabled")
					}
					if err := validateMetaBlock(child, fmt.Sprintf("%s.content[%d]", path, i), "tool_result", p); err != nil {
						return err
					}
				}
			}
		}
	case "thinking":
		if role != "assistant" {
			return metaInvalid(path, "thinking belongs to assistant messages")
		}
		if err := metaKeys(b, path, "type|thinking|signature"); err != nil {
			return err
		}
		if _, ok := b["thinking"].(string); !ok {
			return metaInvalid(path+".thinking", "expected a string")
		}
		if err := metaString(b, "signature", path, false); err != nil {
			return err
		}
	case "redacted_thinking":
		if role != "assistant" {
			return metaInvalid(path, "redacted thinking belongs to assistant messages")
		}
		if err := metaKeys(b, path, "type|data"); err != nil {
			return err
		}
		if err := metaString(b, "data", path, true); err != nil {
			return err
		}
	case "web_search_tool_result", "tool_search_tool_result":
		if role != "assistant" {
			return metaInvalid(path, "hosted-tool results belong to assistant messages")
		}
		capability := "hosted-web-search-o1"
		if typ == "tool_search_tool_result" {
			capability = "hosted-tool-search-o1"
		}
		if !p.Allows(capability) {
			return metaUnverified(path, "hosted-tool result replay is not enabled")
		}
		if err := metaKeys(b, path, "type|tool_use_id|content"); err != nil {
			return err
		}
		if err := metaString(b, "tool_use_id", path, true); err != nil {
			return err
		}
		if err := validateMetaHostedResult(b["content"], path+".content", typ); err != nil {
			return err
		}
	default:
		return metaUnverified(path+".type", "content block is not implemented; it will not be silently dropped")
	}
	return nil
}

func validateMetaSource(value any, path, kind string, p MetaMessagesPolicy) error {
	source, ok := value.(map[string]any)
	if !ok {
		return metaInvalid(path, "expected a source object")
	}
	if err := metaKeys(source, path, "type|media_type|data|url|content"); err != nil {
		return err
	}
	switch source["type"] {
	case "url":
		s, ok := source["url"].(string)
		parsed, err := url.Parse(s)
		if !ok || err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return metaInvalid(path+".url", "expected an HTTP or HTTPS URL")
		}
		if _, exists := source["data"]; exists {
			return metaInvalid(path, "URL sources cannot also contain data")
		}
	case "base64":
		if err := metaString(source, "data", path, true); err != nil {
			return err
		}
		if err := metaString(source, "media_type", path, true); err != nil {
			return err
		}
		mime := source["media_type"].(string)
		if (kind == "image" && !strings.HasPrefix(mime, "image/")) || (kind == "video" && !strings.HasPrefix(mime, "video/")) || (kind == "document" && mime != "application/pdf" && mime != "text/plain") {
			return metaInvalid(path+".media_type", "media type does not match the block")
		}
		if _, err := base64.StdEncoding.DecodeString(source["data"].(string)); err != nil {
			return metaInvalid(path+".data", "expected standard base64")
		}
		if _, exists := source["url"]; exists {
			return metaInvalid(path, "base64 sources cannot also contain a URL")
		}
	case "text":
		if kind != "document" {
			return metaInvalid(path+".type", "text sources are only supported for documents")
		}
		if _, ok := source["data"].(string); !ok {
			return metaInvalid(path+".data", "expected document text")
		}
	case "content":
		if kind != "document" {
			return metaInvalid(path+".type", "content sources are only supported for documents")
		}
		blocks, ok := source["content"].([]any)
		if !ok {
			return metaInvalid(path+".content", "expected content blocks")
		}
		for i, value := range blocks {
			block, ok := value.(map[string]any)
			if !ok || (block["type"] != "text" && block["type"] != "image") {
				return metaInvalid(path+".content", "document content supports text and images")
			}
			if err := validateMetaBlock(block, fmt.Sprintf("%s.content[%d]", path, i), "user", p); err != nil {
				return err
			}
		}
	default:
		return metaUnverified(path+".type", "source shape is not implemented")
	}
	return nil
}

func validateMetaHostedResult(value any, path, kind string) error {
	if kind == "tool_search_tool_result" {
		result, ok := value.(map[string]any)
		if !ok {
			return metaInvalid(path, "expected an O1 tool search result object")
		}
		if result["type"] == "tool_search_tool_result_error" {
			return metaString(result, "error_code", path, true)
		}
		if result["type"] != "tool_search_tool_search_result" {
			return metaUnverified(path, "only direct Anthropic O1 search results are supported")
		}
		refs, ok := result["tool_references"].([]any)
		if !ok {
			return metaInvalid(path+".tool_references", "expected an array")
		}
		for _, value := range refs {
			ref, ok := value.(map[string]any)
			if !ok || ref["type"] != "tool_reference" {
				return metaInvalid(path, "expected tool references")
			}
			if err := metaString(ref, "tool_name", path, true); err != nil {
				return err
			}
		}
		return nil
	}
	if result, ok := value.(map[string]any); ok {
		if result["type"] != "web_search_tool_result_error" {
			return metaUnverified(path, "unknown hosted search error shape")
		}
		return metaString(result, "error_code", path, true)
	}
	results, ok := value.([]any)
	if !ok {
		return metaInvalid(path, "expected web search results")
	}
	for _, value := range results {
		result, ok := value.(map[string]any)
		if !ok || result["type"] != "web_search_result" {
			return metaUnverified(path, "unknown web search result shape")
		}
		for _, key := range []string{"url", "title", "encrypted_content"} {
			if err := metaString(result, key, path, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateMetaMessagesResponseBlock prevents unsupported output from poisoning replay history.
func ValidateMetaMessagesResponseBlock(block map[string]any, p MetaMessagesPolicy, streamStart bool) error {
	if streamStart && block["type"] == "tool_use" {
		if _, ok := block["input"].(map[string]any); !ok {
			return metaInvalid("response.content.input", "expected an object")
		}
	}
	return validateMetaBlock(block, "response.content", "assistant", p)
}
