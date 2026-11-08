package muse

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// prepareCompaction ports Meta's 0019/0020 Responses compaction dispatch from
// meta_executor_execute.go and helps/antigravity_compaction.go. Unlike the fork's
// executor options, the plugin request carries OriginalRequest directly.
func (e *Executor) prepareCompaction(ctx context.Context, call *Call) (surface, error) {
	if call.Req.Alt == "responses/compact" {
		return nil, errorf(http.StatusNotImplemented, "compact_unsupported", "/responses/compact not supported")
	}
	sameOriginal := len(call.Req.OriginalRequest) > 0 && bytes.Equal(call.Req.Payload, call.Req.OriginalRequest)
	payload, err := expandResponsesCompactionCapsules(call.Req.Payload)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "invalid_compaction", "%s", err)
	}
	call.Req.Payload = payload
	if sameOriginal {
		call.Req.OriginalRequest = payload
	} else if len(call.Req.OriginalRequest) > 0 {
		original, err := expandResponsesCompactionCapsules(call.Req.OriginalRequest)
		if err != nil {
			return nil, errorf(http.StatusBadRequest, "invalid_compaction", "%s", err)
		}
		call.Req.OriginalRequest = original
	}
	if hasResponsesCompactionTrigger(call.Req.Payload) || hasResponsesCompactionTrigger(call.Req.OriginalRequest) {
		return &compactionSurface{exec: e}, nil
	}
	return nil, nil
}

type compactionSurface struct{ exec *Executor }

func (s *compactionSurface) execute(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error) {
	summary := compactionSummaryCall(call)
	// The fork re-enters Execute with Responses source format, so perform that
	// request conversion before invoking the Codex-only runner directly.
	converted, err := convertResponsesRequestToCodex(call.Model, summary.Req.Payload)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	summary.Req.Payload = setBool(converted, "stream", false)
	runner := &responsesSurface{exec: s.exec, planner: legacyPlanner{}}
	resp, err := runner.execute(ctx, summary)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	// The legacy runner emits a Codex terminal event; the fork's summary path
	// requested a Responses object. Unwrap it before sealing the same payload.
	body := resp.Payload
	if response := gjson.GetBytes(body, "response"); response.IsObject() {
		body = []byte(response.Raw)
	}
	compacted, err := buildResponsesCompactionResponseFromSummary(call.Model, body)
	if err != nil {
		return pluginapi.ExecutorResponse{}, errorf(http.StatusBadGateway, "compaction_failed", "%s", err)
	}
	resp.Payload = compacted
	return resp, nil
}

func (s *compactionSurface) open(ctx context.Context, call *Call) (*openStream, error) {
	resp, err := s.execute(ctx, call)
	if err != nil {
		return nil, err
	}
	chunks, err := buildResponsesCompactionStreamChunksFromResponse(call.Model, resp.Payload)
	if err != nil {
		return nil, errorf(http.StatusBadGateway, "compaction_failed", "%s", err)
	}
	headers := resp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &openStream{headers: headers, run: func(sink func([]byte) error) error {
		for _, chunk := range chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := sink(chunk); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

func (s *compactionSurface) count(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error) {
	return countResponsesLocal(call)
}

func compactionSummaryCall(call *Call) *Call {
	payload := call.Req.Payload
	if (!hasResponsesCompactionTrigger(payload) && hasResponsesCompactionTrigger(call.Req.OriginalRequest)) || len(payload) == 0 {
		payload = call.Req.OriginalRequest
	}
	summary := *call
	summary.Stream = false
	summary.Req.Format = FormatCodex
	summary.Req.SourceFormat = FormatCodex
	summary.Req.Stream = false
	summary.Req.Alt = ""
	summary.Req.OriginalRequest = nil
	summary.Req.Payload = prepareCompactionSummaryPayload(payload)
	return &summary
}

func hasResponsesCompactionTrigger(payload []byte) bool {
	for _, item := range gjson.GetBytes(payload, "input").Array() {
		if item.Get("type").String() == "compaction_trigger" {
			return true
		}
	}
	return false
}

func expandResponsesCompactionCapsules(payload []byte) ([]byte, error) {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload, nil
	}
	var items []string
	changed := false
	for _, item := range input.Array() {
		if item.Get("type").String() != "compaction" {
			items = append(items, item.Raw)
			continue
		}
		changed = true
		encrypted := item.Get("encrypted_content").String()
		if !strings.HasPrefix(strings.TrimSpace(encrypted), compactionCapsulePrefix) {
			// Foreign provider capsules are deliberately dropped, as in the fork.
			continue
		}
		summary, err := unsealCompaction(encrypted)
		if err != nil {
			return nil, err
		}
		message := []byte(`{"type":"message","role":"developer","content":[{"type":"input_text"}]}`)
		message, _ = sjson.SetBytes(message, "content.0.text", "Context summary from previous turns:\n"+summary)
		items = append(items, string(message))
	}
	if !changed {
		return payload, nil
	}
	return sjson.SetRawBytes(payload, "input", []byte("["+strings.Join(items, ",")+"]"))
}

func prepareCompactionSummaryPayload(payload []byte) []byte {
	const prompt = `{"type":"message","role":"user","content":[{"type":"input_text","text":"Please provide a concise and comprehensive summary of the preceding conversation and task progress so far, including user goals, key findings, actions taken, and current status, so that work can continue smoothly."}]}`
	input := gjson.GetBytes(payload, "input")
	var items []string
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() != "compaction_trigger" {
				items = append(items, item.Raw)
			}
		}
	} else if input.Type == gjson.String && input.String() != "" {
		message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text"}]}`)
		message, _ = sjson.SetBytes(message, "content.0.text", input.String())
		items = append(items, string(message))
	}
	items = append(items, prompt)
	out, _ := sjson.SetRawBytes(payload, "input", []byte("["+strings.Join(items, ",")+"]"))
	for _, field := range []string{"stream", "tools", "tool_choice", "previous_response_id", "parallel_tool_calls", "additional_tools", "truncation", "metadata"} {
		out, _ = sjson.DeleteBytes(out, field)
	}
	out, _ = sjson.SetBytes(out, "stream", false)
	return out
}
