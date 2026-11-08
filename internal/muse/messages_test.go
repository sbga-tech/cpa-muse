package muse

// Consumer-visible regression cases ported from fork helps/meta_messages_test.go.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMetaMessagesStrictConstraints(t *testing.T) {
	tests := []struct {
		name, extra string
		count       bool
		accept      bool
	}{
		{"plain", "", false, true},
		{"disabled thinking", `,"thinking":{"type":"disabled"}`, false, false},
		{"budget valid", `,"thinking":{"type":"enabled","budget_tokens":1024}`, false, true},
		{"budget too high", `,"thinking":{"type":"enabled","budget_tokens":2048}`, false, false},
		{"temperature", `,"temperature":1.01`, false, false},
		{"stop constraint", `,"stop_sequences":["END"]`, false, false},
		{"unknown", `,"future_constraint":true`, false, false},
		{"standard effort", `,"output_config":{"effort":"high"}`, false, true},
		{"max effort gated", `,"output_config":{"effort":"max"}`, false, false},
		{"named tool", `,"tool_choice":{"type":"tool","name":"read"}`, false, false},
		{"cache hour", `,"cache_control":{"type":"ephemeral","ttl":"1h"}`, false, false},
		{"system media", `,"system":[{"type":"image","source":{"type":"url","url":"https://example.invalid/x.png"}}]`, false, false},
		{"hosted unverified", `,"tools":[{"type":"web_search"}]`, false, false},
		{"deferred unverified", `,"tools":[{"name":"read","input_schema":{"type":"object"},"defer_loading":true}]`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"muse-test","max_tokens":2048,"messages":[{"role":"user","content":"hello"}]` + tt.extra + `}`)
			root, err := DecodeMetaMessages(body)
			if err == nil {
				err = ValidateMetaMessages(root, tt.count, MetaMessagesPolicy{})
			}
			if (err == nil) != tt.accept {
				t.Fatalf("accept=%v, err=%v", tt.accept, err)
			}
			if err != nil {
				var typed *StatusError
				if !errors.As(err, &typed) || typed.Status != 400 || !json.Valid(typed.Body) {
					t.Fatalf("invalid error: %#v", err)
				}
			}
		})
	}
}

func TestMetaMessagesDuplicateKeysAndIntegerTypes(t *testing.T) {
	for _, body := range []string{
		`{"model":"a","model":"b"}`,
		`{"model":"a","tools":[{"input_schema":{"type":"object","type":"array"}}]}`,
		`{"messages":[]} {"model":"a"}`,
		`[]`,
	} {
		if _, err := DecodeMetaMessages([]byte(body)); err == nil {
			t.Fatalf("accepted invalid JSON %s", body)
		}
	}
	root, _ := DecodeMetaMessages([]byte(`{"model":"a","messages":[{"role":"user","content":"x"}],"max_tokens":2.5}`))
	if err := ValidateMetaMessages(root, false, MetaMessagesPolicy{}); err == nil {
		t.Fatal("accepted fractional max_tokens")
	}
}

func TestMetaMessagesCountAndSmallBudgetPreserved(t *testing.T) {
	root, _ := DecodeMetaMessages([]byte(`{"model":"alias","messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":1024}}`))
	if err := ValidateMetaMessages(root, true, MetaMessagesPolicy{}); err != nil {
		t.Fatal(err)
	}
	delete(root, "thinking")
	root["max_tokens"] = json.Number("1")
	if err := NormalizeMetaMessages(root, "upstream", false, false, MetaMessagesPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMetaMessages(root, false, MetaMessagesPolicy{}); err != nil {
		t.Fatal(err)
	}
	if root["max_tokens"] != json.Number("1") {
		t.Fatal("output budget was raised")
	}
}

func TestMetaMessagesExplicitDegradeDoesNotTouchToolSchema(t *testing.T) {
	root, _ := DecodeMetaMessages([]byte(`{"model":"a","messages":[{"role":"user","content":"x"}],"max_tokens":2048,"thinking":{"type":"disabled"},"cache_control":{"type":"ephemeral","ttl":"1h"},"tools":[{"name":"read","input_schema":{"type":"object","properties":{"cache_control":{"const":{"ttl":"1h"}}}}}]}`))
	policy := MetaMessagesPolicy{Degrade: MessagesDegradeConfig{ThinkingDisabledToLow: true, CacheTTL5m: true}}
	if err := NormalizeMetaMessages(root, "a", false, false, policy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMetaMessages(root, false, policy); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(root)
	if !bytes.Contains(encoded, []byte(`"const":{"ttl":"1h"}`)) {
		t.Fatal("degradation modified the user's tool schema")
	}
	if !bytes.Contains(encoded, []byte(`"effort":"low"`)) {
		t.Fatal("explicit thinking degradation was not applied")
	}
}

func TestMetaMessagesToolHistoryAndMedia(t *testing.T) {
	base := `{"model":"a","max_tokens":2048,"messages":[{"role":"user","content":"read"},{"role":"assistant","content":[{"type":"tool_use","id":"call_hex","name":"read","input":{"cache_control":{"ttl":"1h"}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_hex","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},{"type":"document","source":{"type":"url","url":"https://example.invalid/report.pdf"}},{"type":"video","source":{"type":"url","url":"https://example.invalid/demo.mp4"}}]}]}]}`
	root, err := DecodeMetaMessages([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMetaMessages(root, false, MetaMessagesPolicy{}); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(base, `"tool_use_id":"call_hex"`, `"tool_use_id":"unmatched"`, 1)
	root, _ = DecodeMetaMessages([]byte(bad))
	if err := ValidateMetaMessages(root, false, MetaMessagesPolicy{}); err == nil {
		t.Fatal("accepted unmatched tool result")
	}
	count, err := EstimateMetaMessagesTokens([]byte(base))
	if err != nil || count < MetaUnknownImageTokens+metaUnknownDocumentTokens+metaUnknownVideoTokens {
		t.Fatalf("media omitted from local estimate: count=%d err=%v", count, err)
	}
}

func TestMetaMessagesHostedO1Replay(t *testing.T) {
	body := []byte(`{"model":"a","max_tokens":2048,"messages":[{"role":"user","content":"search"},{"role":"assistant","content":[{"type":"server_tool_use","id":"call_s","name":"web_search","input":{"query":"x"}},{"type":"web_search_tool_result","tool_use_id":"call_s","content":[{"type":"web_search_result","url":"https://example.invalid","title":"Example","encrypted_content":"opaque"}]}]},{"role":"user","content":"continue"}]}`)
	root, _ := DecodeMetaMessages(body)
	if err := ValidateMetaMessages(root, false, MetaMessagesPolicy{}); err == nil {
		t.Fatal("hosted history bypassed capability gate")
	}
	if err := ValidateMetaMessages(root, false, MetaMessagesPolicy{Capabilities: []string{"hosted-web-search-o1"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMetaMessagesHeadersAreFresh(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
	req.Header.Set("X-Api-Key", "downstream-secret")
	req.Header.Set("Traceparent", "trace")
	req.Header.Set("X-Tbh-Session-Id", "session")
	inbound := http.Header{"Authorization": []string{"Bearer downstream-secret"}, "Anthropic-Version": []string{"2023-06-01"}, "Anthropic-Beta": []string{"oauth-feature"}, "User-Agent": []string{"muse-build"}}
	ApplyMetaMessagesHeaders(req, "upstream-secret", true, inbound)
	if req.Header.Get("Authorization") != "Bearer upstream-secret" {
		t.Fatal("incorrect upstream authentication")
	}
	for _, key := range []string{"X-Api-Key", "Traceparent", "X-Tbh-Session-Id", "Anthropic-Beta"} {
		if req.Header.Get(key) != "" {
			t.Fatalf("leaked header %s", key)
		}
	}
	if strings.Contains(req.Header.Get("User-Agent"), "muse") {
		t.Fatal("leaked Muse profile")
	}
}

func TestMetaMessagesSSEReader(t *testing.T) {
	input := ": keepalive\r\n\r\nevent: ping\r\ndata: {\"type\":\r\ndata: \"ping\"}\r\n\r\n"
	r := NewMetaSSEReader(strings.NewReader(input))
	frame, data, err := r.Next()
	if err != nil || len(data) != 0 || string(frame) != ": keepalive\r\n\r\n" {
		t.Fatalf("comment frame: %q %q %v", frame, data, err)
	}
	_, data, err = r.Next()
	if err != nil || !json.Valid(data) {
		t.Fatalf("multiline data: %q %v", data, err)
	}
	if _, _, err = r.Next(); err != io.EOF {
		t.Fatalf("EOF: %v", err)
	}
	r = NewMetaSSEReader(strings.NewReader("data: {\"type\":\"ping\"}\n"))
	if _, _, err = r.Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("accepted unterminated event: %v", err)
	}
}

func TestMetaMessagesCircuitGeneration(t *testing.T) {
	now := time.Unix(100, 0)
	circuit := NewMetaMessagesCircuit(func() time.Time { return now })
	stale, _ := circuit.Acquire("a")
	denied, _ := circuit.Acquire("a")
	denied.Complete(MetaMessagesDenied)
	stale.Complete(MetaMessagesSuccess)
	if ticket, _ := circuit.Acquire("a"); ticket != nil {
		t.Fatal("stale success cleared the newer circuit")
	}
	now = now.Add(30 * time.Minute)
	probe, _ := circuit.Acquire("a")
	if probe == nil {
		t.Fatal("half-open probe unavailable")
	}
	if second, _ := circuit.Acquire("a"); second != nil {
		t.Fatal("multiple half-open probes admitted")
	}
	probe.Complete(MetaMessagesNeutral)
	probe.Complete(MetaMessagesSuccess)
	next, _ := circuit.Acquire("a")
	if next == nil {
		t.Fatal("neutral completion leaked lease")
	}
	next.Complete(MetaMessagesSuccess)
	for i := 0; i < 5; i++ {
		ticket, _ := circuit.Acquire("a")
		if ticket == nil {
			t.Fatal("circuit opened too early")
		}
		ticket.Complete(MetaMessagesTransient)
	}
	if ticket, _ := circuit.Acquire("a"); ticket != nil {
		t.Fatal("transient threshold did not open circuit")
	}
}
