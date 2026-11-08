package muse

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// Regression cases adapted from fork internal/runtime/executor/openai_responses_signature_test.go.
func assertEmptyReasoningContent(t *testing.T, body []byte, path string) {
	t.Helper()
	content := gjson.GetBytes(body, path)
	if !content.Exists() || !content.IsArray() || len(content.Array()) != 0 {
		t.Fatalf("%s should be an empty array for Codex maxItems:0, got %s body=%s", path, content.Raw, body)
	}
}

func TestSignatureSanitizer_StripsOrphanIDsWhenStoreDisabled(t *testing.T) {
	valid := testMetaReasoningSignature(1024)
	body := []byte(`{"store":false,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_orphan","type":"reasoning","summary":[]},` +
		`{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[]},` +
		`{"id":"msg_1","type":"message","role":"user","content":"hi"}` +
		`]}`)

	got := sanitizeMetaReasoning(body, "")

	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content still present: %s", got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("invalid reasoning id should be stripped when store=false: %s", got)
	}
	if gjson.GetBytes(got, "input.1.id").Exists() {
		t.Fatalf("orphan reasoning id should be stripped when store=false: %s", got)
	}
	if gotID := gjson.GetBytes(got, "input.2.id").String(); gotID != "rs_good" {
		t.Fatalf("valid reasoning id = %q, want rs_good; body=%s", gotID, got)
	}
	if gotEC := gjson.GetBytes(got, "input.2.encrypted_content").String(); gotEC != valid {
		t.Fatalf("valid encrypted_content not preserved: %s", got)
	}
	if gotID := gjson.GetBytes(got, "input.3.id").String(); gotID != "msg_1" {
		t.Fatalf("non-reasoning id should stay: %s", got)
	}
}

func TestSignatureSanitizer_KeepsIDsWhenStoreEnabled(t *testing.T) {
	body := []byte(`{"store":true,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_orphan","type":"reasoning","summary":[]}` +
		`]}`)

	got := sanitizeMetaReasoning(body, "")

	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content still present: %s", got)
	}
	if gotID := gjson.GetBytes(got, "input.0.id").String(); gotID != "rs_bad" {
		t.Fatalf("store=true should keep reasoning id after dropping invalid encrypted_content, got %q body=%s", gotID, got)
	}
	if gotID := gjson.GetBytes(got, "input.1.id").String(); gotID != "rs_orphan" {
		t.Fatalf("store=true should keep orphan reasoning id, got %q body=%s", gotID, got)
	}
}

func TestSignatureSanitizer_MovesCleartextContentToSummary(t *testing.T) {
	// Codex Desktop replays third-party thinking as reasoning.content[reasoning_text].
	// Official Codex upstream rejects that shape with array_above_max_length (maxItems: 0).
	body := []byte(`{"store":false,"input":[` +
		`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"The model thinking process from a previous turn with a third-party provider..."}],"encrypted_content":null},` +
		`{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}` +
		`]}`)

	got := sanitizeMetaReasoning(body, "")

	assertEmptyReasoningContent(t, got, "input.0.content")
	if gotType := gjson.GetBytes(got, "input.0.summary.0.type").String(); gotType != "summary_text" {
		t.Fatalf("summary.0.type = %q, want summary_text; body=%s", gotType, got)
	}
	if gotText := gjson.GetBytes(got, "input.0.summary.0.text").String(); gotText != "The model thinking process from a previous turn with a third-party provider..." {
		t.Fatalf("summary.0.text = %q, want promoted reasoning_text; body=%s", gotText, got)
	}
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("null encrypted_content should still be stripped: %s", got)
	}
	if gotType := gjson.GetBytes(got, "input.1.content.0.type").String(); gotType != "input_text" {
		t.Fatalf("non-reasoning content should stay: %s", got)
	}
}

func TestSignatureSanitizer_DoesNotDuplicateExistingSummary(t *testing.T) {
	body := []byte(`{"store":false,"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"already summarized"}],"content":[{"type":"reasoning_text","text":"duplicate thinking"}]}]}`)

	got := sanitizeMetaReasoning(body, "")

	assertEmptyReasoningContent(t, got, "input.0.content")
	if gotLen := len(gjson.GetBytes(got, "input.0.summary").Array()); gotLen != 1 {
		t.Fatalf("summary length = %d, want 1 (no duplicate); body=%s", gotLen, got)
	}
	if gotText := gjson.GetBytes(got, "input.0.summary.0.text").String(); gotText != "already summarized" {
		t.Fatalf("existing summary overwritten: %s", got)
	}
}

func TestSignatureSanitizer_PromotesMultipleReasoningTextParts(t *testing.T) {
	body := []byte(`{"store":false,"input":[{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"step one"},{"type":"reasoning_text","text":"step two"}]}]}`)

	got := sanitizeMetaReasoning(body, "")

	assertEmptyReasoningContent(t, got, "input.0.content")
	if gotLen := len(gjson.GetBytes(got, "input.0.summary").Array()); gotLen != 2 {
		t.Fatalf("summary length = %d, want 2; body=%s", gotLen, got)
	}
	if gotText := gjson.GetBytes(got, "input.0.summary.0.text").String(); gotText != "step one" {
		t.Fatalf("summary.0.text = %q, want step one; body=%s", gotText, got)
	}
	if gotText := gjson.GetBytes(got, "input.0.summary.1.text").String(); gotText != "step two" {
		t.Fatalf("summary.1.text = %q, want step two; body=%s", gotText, got)
	}
}

func TestSignatureSanitizer_KeepsValidEncryptedContentWhenStrippingContent(t *testing.T) {
	valid := testMetaReasoningSignature(1024)
	body := []byte(`{"store":false,"input":[{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[],"content":[{"type":"reasoning_text","text":"cleartext thinking"}]}]}`)

	got := sanitizeMetaReasoning(body, "")

	assertEmptyReasoningContent(t, got, "input.0.content")
	if gotID := gjson.GetBytes(got, "input.0.id").String(); gotID != "rs_good" {
		t.Fatalf("valid reasoning id = %q, want rs_good; body=%s", gotID, got)
	}
	if gotEC := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotEC != valid {
		t.Fatalf("valid encrypted_content not preserved: %s", got)
	}
	if gotText := gjson.GetBytes(got, "input.0.summary.0.text").String(); gotText != "cleartext thinking" {
		t.Fatalf("summary.0.text = %q, want promoted reasoning_text; body=%s", gotText, got)
	}
}

func TestSignatureSanitizer_NoopReturnsOriginalBody(t *testing.T) {
	valid := testMetaReasoningSignature(1024)
	body := []byte(`{"store":false,"input":[{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[]},{"role":"user","content":"hi"}]}`)
	got := sanitizeMetaReasoning(body, "")
	if string(got) != string(body) {
		t.Fatalf("noop path should return original body unchanged\ngot=%s\nwant=%s", got, body)
	}
	if len(got) > 0 && len(body) > 0 && &got[0] != &body[0] {
		t.Fatalf("noop path should return the original body slice")
	}
}

func TestSignatureMetaTargetURLPolicies(t *testing.T) {
	native := testMetaReasoningSignature(1024)
	routed := native + "." + testMetaRouterSuffix()
	for _, tc := range []struct {
		url        string
		wantRouted string
	}{
		{"", ""}, {"https://api.meta.ai/api/v1", ""},
		{" https://API.META.AI:443/v1 ", ""}, {"https://edge.meta.ai/v1", ""},
		{"https://[invalid", ""}, {"api.meta.ai/v1", ""},
		{"https://openrouter.ai/api/v1", routed}, {"https://EDGE.OPENROUTER.AI:443/v1", routed},
		{"https://example-proxy.internal/v1", native},
		{"https://api.meta.ai.evil.example/v1", native},
		{"https://openrouter.ai.evil.example/v1", native},
	} {
		t.Run(tc.url, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"store": false, "input": []any{
				map[string]any{"type": "reasoning", "id": "native", "encrypted_content": native},
				map[string]any{"type": "reasoning", "id": "routed", "encrypted_content": routed},
			}})
			out := sanitizeMetaReasoning(body, tc.url)
			if got := gjson.GetBytes(out, "input.0.encrypted_content").String(); got != native {
				t.Fatal("native envelope changed")
			}
			if got := gjson.GetBytes(out, "input.1.encrypted_content").String(); got != tc.wantRouted {
				t.Fatalf("routed policy: got %q, want %q", got, tc.wantRouted)
			}
			if got := gjson.GetBytes(out, "input.1.id").Exists(); got != (tc.wantRouted != "") {
				t.Fatal("orphan id policy mismatch")
			}
			if again := sanitizeMetaReasoning(out, tc.url); !bytes.Equal(again, out) {
				t.Fatal("sanitizer is not idempotent")
			}
		})
	}
}

func TestSignatureMetaRejectsForeignAndMalformed(t *testing.T) {
	native := testMetaReasoningSignature(1024)
	values := []any{"", "opaque-encrypted-reasoning-token", "gAAAAABforeignGPT", "EoYBforeignClaude", "meta#" + native, "muse#" + native,
		" " + native, native + " ", native + ".", native + ".bad-suffix", native + "=", native[:len(native)-1], nil, 42, true, []string{native}, map[string]string{"signature": native}}
	for _, baseURL := range []string{"https://api.meta.ai/v1", "https://openrouter.ai/api/v1", "https://third-party.example/v1"} {
		for _, value := range values {
			body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "reasoning", "id": "orphan", "encrypted_content": value, "summary": []any{}}}})
			out := sanitizeMetaReasoning(body, baseURL)
			if gjson.GetBytes(out, "input.0.encrypted_content").Exists() || gjson.GetBytes(out, "input.0.id").Exists() {
				t.Fatalf("unsafe signature retained for %s: %s", baseURL, out)
			}
		}
	}
}

func TestSignatureUnchangedInputsAndSummaryShapes(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("{}"), []byte(`{"input":"plain text"}`), []byte(`{"input":[{"type":"message","id":"m","encrypted_content":"opaque","content":"hi"}]}`)} {
		if got := sanitizeMetaReasoning(body, ""); !bytes.Equal(got, body) {
			t.Fatalf("unrelated input changed: %s", got)
		}
	}
	for _, summary := range []string{"", `,"summary":null`, `,"summary":[]`, `,"summary":{}`, `,"summary":"existing"`} {
		body := []byte(`{"input":[{"type":"reasoning"` + summary + `,"content":[{"type":"reasoning_text","text":"first"},{"type":"output_text","text":"skip"},{"type":"reasoning_text","text":""},{"type":"reasoning_text","text":"second"}]}]}`)
		got := sanitizeMetaReasoning(body, "")
		assertEmptyReasoningContent(t, got, "input.0.content")
		if summary == "" || summary == `,"summary":null` || summary == `,"summary":[]` {
			if gjson.GetBytes(got, "input.0.summary.0.text").String() != "first" || gjson.GetBytes(got, "input.0.summary.1.text").String() != "second" || len(gjson.GetBytes(got, "input.0.summary").Array()) != 2 {
				t.Fatalf("summary promotion: %s", got)
			}
		} else if gjson.GetBytes(got, "input.0.summary").Raw != gjson.GetBytes(body, "input.0.summary").Raw {
			t.Fatalf("nonempty summary overwritten: %s", got)
		}
	}
}
