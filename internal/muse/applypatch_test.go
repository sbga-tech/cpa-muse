package muse

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Lifecycle cases adapted from fork internal/runtime/executor/helps/apply_patch_responses_test.go.
func TestApplyPatchTransformerLifecycle(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	item := patchItem("function_call", "a", "c", "apply_patch", wrapApplyPatchInput("p"))
	for _, terminal := range []string{"response.completed", "response.incomplete", "response.done", "response.failed", ""} {
		for _, marker := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", terminal, marker), func(t *testing.T) {
				transformer, _, err := newApplyPatchBridge("openai-response", request, request)
				if err != nil || transformer == nil {
					t.Fatalf("new bridge: %v", err)
				}
				if _, err = transformer.Transform(patchEvent("response.output_item.done", 0, item)); err != nil {
					t.Fatal(err)
				}
				if terminal != "" {
					if _, err = transformer.Transform([]byte(`{"type":` + patchJSON(terminal) + `,"response":{"output":[]}}`)); err != nil {
						t.Fatal(err)
					}
				}
				var out [][]byte
				if marker {
					out, err = transformer.Transform([]byte("  [DONE]  "))
				} else {
					out, err = transformer.Finish()
				}
				if terminal == "" {
					assertApplyPatchGatewayFailure(t, out, err)
					if repeated, err := transformer.Finish(); err != nil || len(repeated) != 0 {
						t.Fatalf("repeated EOF failure: %s %v", repeated, err)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if marker && (len(out) != 1 || string(out[0]) != "  [DONE]  ") {
					t.Fatalf("transport sentinel changed: %s", out)
				}
				if out, err = transformer.Transform([]byte(`{"type":"response.completed","response":{"output":[]}}`)); err != nil || len(out) != 0 {
					t.Fatalf("closed stream reopened: %s %v", out, err)
				}
				transformer.Reset()
				out, err = transformer.Transform(patchEvent("response.output_item.done", 0, item))
				if err != nil || len(out) == 0 || gjson.GetBytes(out[len(out)-1], "item.input").String() != "p" {
					t.Fatalf("retry did not reset state: %s %v", out, err)
				}
			})
		}
	}
}

func assertApplyPatchGatewayFailure(t *testing.T, out [][]byte, err error) {
	t.Helper()
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusBadGateway || status.Scope != ScopeRequest || status.Message != ApplyPatchUpstreamErrorMessage {
		t.Fatalf("gateway error: %#v", err)
	}
	if len(out) != 1 || gjson.GetBytes(out[0], "type").String() != "response.failed" || gjson.GetBytes(out[0], "response.error.message").String() != status.Message {
		t.Fatalf("missing sanitized failure: %s", out)
	}
}

func TestApplyPatchTransformerInvalidInputAndNonStream(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	for _, arguments := range []string{`{"input":42}`, `{"input":"SECRET","extra":true}`, `{"input":"\uD800"}`, `{}`} {
		bridge, _, err := newApplyPatchBridge("codex", request, request)
		if err != nil {
			t.Fatal(err)
		}
		item := patchItem("function_call", "a", "c", "apply_patch", arguments)
		out, err := bridge.Transform(patchEvent("response.output_item.done", 0, item))
		assertApplyPatchGatewayFailure(t, out, err)
		if bytes.Contains(bytes.Join(out, nil), []byte("SECRET")) || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("upstream arguments leaked")
		}
		bridge.Reset()
		_, err = bridge.TransformCompleted([]byte(`{"output":[` + item + `]}`))
		var status *StatusError
		if !errors.As(err, &status) || status.Status != http.StatusBadGateway || status.Scope != ScopeRequest || status.Message != ApplyPatchUpstreamErrorMessage {
			t.Fatalf("nonstream error: %v", err)
		}
	}
	bridge, _, err := newApplyPatchBridge("codex", request, request)
	if err != nil {
		t.Fatal(err)
	}
	item := patchItem("function_call", "a", "c", "apply_patch", wrapApplyPatchInput("  patch\n"))
	for _, raw := range []string{`{"output":[` + item + `]}`, `{"type":"response.completed","response":{"output":[` + item + `]}}`} {
		bridge.Reset()
		out, err := bridge.TransformCompleted([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		path := "output.0"
		if gjson.GetBytes(out, "response").Exists() {
			path = "response." + path
		}
		if gjson.GetBytes(out, path+".type").String() != "custom_tool_call" || gjson.GetBytes(out, path+".input").String() != "  patch\n" || gjson.GetBytes(out, path+".arguments").Exists() {
			t.Fatalf("nonstream patch not restored: %s", out)
		}
	}
}

func TestApplyPatchRequestNoCustomToolAndChatPreference(t *testing.T) {
	for _, raw := range []string{`{}`, `{"tools":[{"type":"function","name":"apply_patch"}]}`, `{"tools":[{"type":"custom","name":"other"}]}`} {
		bridge, out, err := newApplyPatchBridge("codex", nil, []byte(raw))
		if err != nil || bridge != nil || string(out) != raw {
			t.Fatalf("inactive request changed: %s %v", out, err)
		}
	}
	original := []byte(`{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","function":{"name":"apply_patch"}}]}`)
	body := []byte(`{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"apply_patch","parameters":{"properties":{"x":{"type":"integer"}}}}]}`)
	bridge, out, err := newApplyPatchBridge("openai", original, body)
	if err != nil || bridge != nil || !gjson.GetBytes(out, "tools.0.parameters.properties.x").Exists() || gjson.GetBytes(out, "tools.0.parameters.properties.input").Exists() {
		t.Fatalf("chat function precedence lost: %s %v", out, err)
	}
}

func TestApplyPatchRequestNormalization(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch","description":"This is a FREEFORM tool, so do not wrap the patch in JSON.","format":{"type":"grammar","definition":"*** Environment ID: test"}}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"custom","name":"apply_patch"}]},"input":[{"type":"custom_tool_call","call_id":"old","name":"apply_patch","input":"raw patch"},{"type":"custom_tool_call_output","call_id":"old","output":"ok"}]}`)
	bridge, out, err := newApplyPatchBridge("codex", request, request)
	if err != nil || bridge == nil {
		t.Fatalf("new bridge: %v", err)
	}
	if gjson.GetBytes(out, "tools.0.type").String() != "function" || gjson.GetBytes(out, "tools.0.format").Exists() || gjson.GetBytes(out, "tools.0.parameters.additionalProperties").Bool() || gjson.GetBytes(out, "tools.0.parameters.required.0").String() != "input" || strings.Contains(gjson.GetBytes(out, "tools.0.description").String(), "FREEFORM") || !strings.Contains(gjson.GetBytes(out, "tools.0.description").String(), "Original patch grammar:") {
		t.Fatalf("tool normalization: %s", out)
	}
	if gjson.GetBytes(out, "tool_choice.tools.0.type").String() != "function" || gjson.GetBytes(out, "input.0.arguments").String() != wrapApplyPatchInput("raw patch") || gjson.GetBytes(out, "input.1.type").String() != "function_call_output" {
		t.Fatalf("history or choice normalization: %s", out)
	}
	for _, raw := range []string{`{`, `{"input":[{"type":"custom_tool_call","name":"apply_patch","input":{}}]}`} {
		_, _, err := newApplyPatchBridge("codex", nil, []byte(raw))
		var status *StatusError
		if !errors.As(err, &status) || status.Status != http.StatusBadRequest || status.Scope != ScopeRequest {
			t.Fatalf("invalid request accepted: %v", err)
		}
	}
}

func TestToolSchemaCodexUserAgent(t *testing.T) {
	for _, tc := range []struct {
		headers http.Header
		want    bool
	}{
		{nil, false}, {http.Header{}, false},
		{http.Header{"user-agent": {"", "  CoDeX-Desktop/1.0  "}}, true},
		{http.Header{"USER-AGENT": {"curl/8.0"}}, false},
		{http.Header{"X-User-Agent": {"codex"}}, false},
		{http.Header{"User-Agent": {"curl/8.0", "codex"}}, false},
	} {
		if got := IsCodexUserAgent(tc.headers); got != tc.want {
			t.Fatalf("UA %v: got %v, want %v", tc.headers, got, tc.want)
		}
	}
}
