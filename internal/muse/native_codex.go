package muse

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertResponsesRequestToCodex ports upstream v8.0.54's
// translator/codex/openai/responses/codex_openai-responses_request.go using
// gjson/sjson instead of the host-internal ojson document wrapper.
func convertResponsesRequestToCodex(_ string, body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		message, _ := sjson.SetBytes([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`), "0.content.0.text", input.String())
		body, _ = sjson.SetRawBytes(body, "input", message)
	}
	body = setBool(body, "stream", true)
	body = setBool(body, "store", false)
	if gjson.GetBytes(body, "parallel_tool_calls").Type != gjson.False {
		body = setBool(body, "parallel_tool_calls", true)
	}
	include := gjson.GetBytes(body, "include")
	sources := false
	// gjson's Array() wraps a scalar into a one-element slice; upstream only
	// honors a real include array.
	if include.IsArray() {
		for _, value := range include.Array() {
			if value.Type == gjson.String && value.String() == "web_search_call.action.sources" {
				sources = true
				break
			}
		}
	}
	required := `["reasoning.encrypted_content"]`
	if sources {
		required = `["reasoning.encrypted_content","web_search_call.action.sources"]`
	}
	values := include.Array()
	normalized := include.IsArray() && len(values) == 1 && values[0].Type == gjson.String && values[0].String() == "reasoning.encrypted_content"
	if sources {
		normalized = include.IsArray() && len(values) == 2 && values[0].Type == gjson.String && values[0].String() == "reasoning.encrypted_content" && values[1].Type == gjson.String && values[1].String() == "web_search_call.action.sources"
	}
	if !normalized {
		body, _ = sjson.SetRawBytes(body, "include", []byte(required))
	}
	if output := gjson.GetBytes(body, "max_output_tokens"); !output.Exists() || output.Type == gjson.Null {
		for _, path := range []string{"max_completion_tokens", "max_tokens"} {
			if v := gjson.GetBytes(body, path); v.Type == gjson.Number {
				body, _ = sjson.SetBytes(body, "max_output_tokens", v.Int())
				break
			}
		}
	}
	for _, path := range []string{"max_completion_tokens", "max_tokens", "temperature", "top_p", "truncation", "prompt_cache_options", "prompt_cache_retention", "context_management", "user"} {
		if gjson.GetBytes(body, path).Exists() {
			body, _ = sjson.DeleteBytes(body, path)
		}
	}
	if tier := gjson.GetBytes(body, "service_tier"); tier.Exists() {
		name := strings.ToLower(strings.TrimSpace(tier.String()))
		if tier.Type != gjson.String || name != "priority" && name != "fast" && name != "ultrafast" {
			body, _ = sjson.DeleteBytes(body, "service_tier")
		} else {
			if name == "fast" {
				name = "priority"
			}
			body = setString(body, "service_tier", name)
		}
	}
	input = gjson.GetBytes(body, "input")
	if input.IsArray() {
		input.ForEach(func(index, item gjson.Result) bool {
			if !item.IsObject() {
				return true
			}
			var raw []byte
			ensureRaw := func() {
				if raw == nil {
					raw = []byte(item.Raw)
				}
			}
			if item.Get("prompt_cache_breakpoint").Exists() {
				ensureRaw()
				raw, _ = sjson.DeleteBytes(raw, "prompt_cache_breakpoint")
			}
			for _, path := range []string{"content", "output"} {
				parts := item.Get(path)
				if !parts.IsArray() {
					continue
				}
				parts.ForEach(func(partIndex, part gjson.Result) bool {
					if part.IsObject() && part.Get("prompt_cache_breakpoint").Exists() {
						ensureRaw()
						raw, _ = sjson.DeleteBytes(raw, path+"."+strconv.FormatInt(partIndex.Int(), 10)+".prompt_cache_breakpoint")
					}
					return true
				})
			}
			if item.Get("role").String() == "system" {
				ensureRaw()
				raw = setString(raw, "role", "developer")
			}
			if item.Get("type").String() == "function_call" {
				if args := item.Get("arguments"); args.Type == gjson.String && strings.TrimSpace(args.String()) == "" {
					ensureRaw()
					raw = setString(raw, "arguments", "{}")
				}
			}
			if raw != nil {
				body, _ = sjson.SetRawBytes(body, "input."+strconv.FormatInt(index.Int(), 10), raw)
			}
			return true
		})
	}
	for _, path := range []string{"tools", "tool_choice.tools"} {
		tools := gjson.GetBytes(body, path)
		if !tools.IsArray() {
			continue
		}
		for i, tool := range tools.Array() {
			if kind := codexBuiltinToolType(tool.Get("type").String()); kind != "" {
				body = setString(body, path+"."+strconv.Itoa(i)+".type", kind)
			}
		}
	}
	if kind := codexBuiltinToolType(gjson.GetBytes(body, "tool_choice.type").String()); kind != "" {
		body = setString(body, "tool_choice.type", kind)
	}
	return body, nil
}
func codexBuiltinToolType(kind string) string {
	switch kind {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	}
	return ""
}

// codexToResponsesTransformer ports the corresponding upstream response
// translator. The apply_patch bridge is an earlier runner stage. Terminal event
// extraction is owned by responsesPlan.nonStreamResponseObject, not this stage.
type codexToResponsesTransformer struct{ model string }

func newCodexToResponsesTransformer(original []byte) eventTransformer {
	return &codexToResponsesTransformer{model: gjson.GetBytes(original, "model").String()}
}
func (*codexToResponsesTransformer) Reset()                    {}
func (*codexToResponsesTransformer) Finish() ([][]byte, error) { return nil, nil }
func (t *codexToResponsesTransformer) Transform(event []byte) ([][]byte, error) {
	kind := gjson.GetBytes(event, "type").String()
	if (kind == "response.created" || kind == "response.in_progress") && !gjson.GetBytes(event, "response.model").Exists() && t.model != "" {
		event = setString(event, "response.model", t.model)
	}
	return [][]byte{event}, nil
}
func (t *codexToResponsesTransformer) TransformCompleted(event []byte) ([]byte, error) {
	return event, nil
}
