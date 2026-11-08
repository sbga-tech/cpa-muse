package muse

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
)

// countResponsesLocal ports MetaExecutor.CountTokens and countLegacyResponsesInput
// from meta_executor_execute.go and meta_responses_native_executor.go. The host
// already translates requests; replies must remain in the executor's output
// format so pluginhost's token-count translator can recognize them.
func countResponsesLocal(call *Call) (pluginapi.ExecutorResponse, error) {
	if _, err := DecodeMetaMessages(call.Req.Payload); err != nil {
		return pluginapi.ExecutorResponse{}, errorf(http.StatusBadRequest, "invalid_json", "Request must be a JSON object without duplicate keys")
	}
	body := call.Req.Payload
	if call.Req.Format == FormatOpenAIResponse {
		converted, err := convertResponsesRequestToCodex(call.Model, body)
		if err != nil {
			return pluginapi.ExecutorResponse{}, err
		}
		body = converted
	}
	body = setString(body, "model", call.Model)
	body = setBool(body, "stream", false)
	for _, field := range legacyDroppedFields {
		body, _ = sjson.DeleteBytes(body, field)
	}
	body = applyThinkingLevel(body, call.ThinkingLevel)
	_, body, err := newApplyPatchBridge(call.Req.SourceFormat, call.Req.OriginalRequest, body)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if inst := gjson.GetBytes(body, "instructions"); !inst.Exists() || inst.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	body = sanitizeMetaReasoning(body, "")
	body = sanitizeMetaWebSearchTools(body)
	body = normalizeCodexToolIntegerTypes(body, call.Req.Headers)

	var count int64
	semantic := call.Req.Format == FormatOpenAIResponse || (call.Req.SourceFormat == FormatOpenAIResponse && call.Req.Alt == "responses/input_tokens")
	if semantic {
		count, err = estimateNativeResponses(body)
	} else {
		var enc tokenizer.Codec
		enc, err = claudeInputTokenizer()
		if err == nil {
			count, err = countCodexInputTokens(enc, body)
		}
	}
	if err != nil {
		return pluginapi.ExecutorResponse{}, errorf(http.StatusInternalServerError, "count_failed", "Local token estimation failed: %v", err)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if semantic {
		headers = nativeCountResponse(count).Headers
		headers.Set("X-CPA-Token-Count-Basis", "normalized-legacy")
	}
	payload := []byte(fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count))
	if call.Req.Format == FormatOpenAIResponse {
		payload = []byte(fmt.Sprintf(`{"object":"response.input_tokens","input_tokens":%d}`, count))
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: headers}, nil
}

// countCodexInputTokens ports codex_executor_tokens.go without model selection:
// Meta's legacy path always uses O200kBase, independently of its model name.
func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	root := gjson.ParseBytes(body)
	var segments []string
	add := func(text string) {
		if text = strings.TrimSpace(text); text != "" {
			segments = append(segments, text)
		}
	}
	add(root.Get("instructions").String())
	for _, item := range root.Get("input").Array() {
		switch item.Get("type").String() {
		case "message":
			for _, part := range item.Get("content").Array() {
				add(part.Get("text").String())
			}
		case "function_call":
			add(item.Get("name").String())
			add(item.Get("arguments").String())
		case "function_call_output":
			add(item.Get("output").String())
		default:
			add(item.Get("text").String())
		}
	}
	for _, tool := range root.Get("tools").Array() {
		add(tool.Get("name").String())
		add(tool.Get("description").String())
		if params := tool.Get("parameters"); params.Exists() {
			if params.Type == gjson.String {
				add(params.String())
			} else {
				add(params.Raw)
			}
		}
	}
	format := root.Get("text.format")
	add(format.Get("name").String())
	if schema := format.Get("schema"); schema.Exists() {
		if schema.Type == gjson.String {
			add(schema.String())
		} else {
			add(schema.Raw)
		}
	}
	if len(segments) == 0 {
		return 0, nil
	}
	count, err := enc.Count(strings.Join(segments, "\n"))
	return int64(count), err
}
