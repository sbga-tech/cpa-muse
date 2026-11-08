package muse

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyPatchUpstreamErrorMessage is ported from fork internal/runtime/executor/helps/apply_patch.go.
// Never include the upstream arguments or patch text in a gateway error.
const ApplyPatchUpstreamErrorMessage = "Invalid apply_patch tool arguments received from upstream."

// newApplyPatchBridge ports the Meta executor's request-local bridge from fork
// internal/runtime/executor/helps/apply_patch_responses.go. The plugin runner
// owns SSE framing and usage/error reporting. xAI dispatcher folding is not
// used by Meta and is deliberately excluded.
func newApplyPatchBridge(source string, original, body []byte) (eventTransformer, []byte, error) {
	declarations := body
	if source == "openai" {
		declarations = preferChatFunctionPatchTools(original, declarations)
	}
	normalized, err := NormalizeApplyPatchResponsesRequest(preferChatFunctionPatchTools(original, body))
	if err != nil {
		return nil, nil, &StatusError{Status: http.StatusBadRequest, Code: "invalid_apply_patch", Message: err.Error(), Scope: ScopeRequest}
	}
	bridge := NewApplyPatchResponsesBridge(declarations)
	if !bridge.active {
		return nil, normalized, nil
	}
	return &applyPatchTransformer{declarations: declarations, bridge: bridge}, normalized, nil
}

// preferChatFunctionPatchTools preserves the Chat request converter's existing winner rule.
func preferChatFunctionPatchTools(original, declarations []byte) []byte {
	ordinary := map[string]bool{}
	for _, tool := range gjson.GetBytes(original, "tools").Array() {
		if tool.Get("type").String() == "function" {
			ordinary[tool.Get("function.name").String()] = true
		}
	}
	if len(ordinary) == 0 {
		return declarations
	}
	available := map[string]bool{}
	for _, tool := range gjson.GetBytes(declarations, "tools").Array() {
		if tool.Get("type").String() == "function" {
			available[tool.Get("name").String()] = true
		}
	}
	var tools [][]byte
	for _, tool := range gjson.GetBytes(declarations, "tools").Array() {
		name := tool.Get("name").String()
		if isApplyPatchCustomTool(tool) && ordinary[name] && available[name] {
			continue
		}
		tools = append(tools, []byte(tool.Raw))
	}
	declarations, _ = sjson.SetRawBytes(declarations, "tools", joinApplyPatchRawArray(tools))
	return declarations
}

type applyPatchTransformer struct {
	declarations                  []byte
	bridge                        *ApplyPatchResponsesBridge
	closed, failed, transportDone bool
}

func (t *applyPatchTransformer) Reset() {
	t.bridge = NewApplyPatchResponsesBridge(t.declarations)
	t.closed, t.failed, t.transportDone = false, false, false
}

func applyPatchStatusError(err error) error {
	if err == nil {
		return nil
	}
	return &StatusError{Status: http.StatusBadGateway, Code: "invalid_tool_arguments", Message: ApplyPatchUpstreamErrorMessage, Scope: ScopeRequest}
}

func (t *applyPatchTransformer) fail(err error) ([][]byte, error) {
	if t.failed {
		return nil, applyPatchStatusError(err)
	}
	t.failed = true
	events, _ := t.bridge.Fail(err)
	return events, applyPatchStatusError(err)
}

func (t *applyPatchTransformer) Transform(event []byte) ([][]byte, error) {
	if t.failed || t.transportDone {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
		if err := t.finish(); err != nil {
			return t.fail(err)
		}
		t.transportDone = true
		return [][]byte{event}, nil
	}
	if t.closed {
		return nil, nil
	}
	events, err := t.bridge.Transform(event)
	if err != nil {
		t.failed = true
		return events, applyPatchStatusError(err)
	}
	switch gjson.GetBytes(event, "type").String() {
	case "response.completed", "response.incomplete", "response.done", "response.failed":
		t.closed = true
	}
	return events, nil
}

func (t *applyPatchTransformer) finish() error {
	if err := t.bridge.Finish(); err != nil {
		return err
	}
	if t.closed {
		return nil
	}
	return errors.New("incomplete apply_patch source response received from upstream")
}

func (t *applyPatchTransformer) Finish() ([][]byte, error) {
	if t.failed || t.transportDone {
		return nil, nil
	}
	if err := t.finish(); err != nil {
		return t.fail(err)
	}
	return nil, nil
}

func (t *applyPatchTransformer) TransformCompleted(completed []byte) ([]byte, error) {
	out, err := t.bridge.TransformNonStream(completed)
	return out, applyPatchStatusError(err)
}
