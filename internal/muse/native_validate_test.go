package muse

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeTokenEstimateCoversAllInputForms(t *testing.T) {
	for _, form := range []string{"scalar", "easy-message", "typed-message"} {
		t.Run(form, func(t *testing.T) {
			makeBody := func(text string) []byte {
				var input any = text
				if form == "easy-message" {
					input = []any{map[string]any{"role": "user", "content": text}}
				}
				if form == "typed-message" {
					input = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
				}
				body, _ := json.Marshal(map[string]any{"model": "muse-test", "input": input})
				return body
			}
			short, err := estimateNativeResponses(makeBody("hello"))
			if err != nil {
				t.Fatal(err)
			}
			long, err := estimateNativeResponses(makeBody(strings.Repeat("long prompt content ", 20000)))
			if err != nil {
				t.Fatal(err)
			}
			if long <= short+20000 {
				t.Fatalf("input content was omitted: short=%d long=%d", short, long)
			}
		})
	}
}

func TestNativeTokenEstimateIncludesNamespacesAndMedia(t *testing.T) {
	plain := []byte(`{"model":"a","input":"hello"}`)
	base, err := estimateNativeResponses(plain)
	if err != nil {
		t.Fatal(err)
	}
	root := map[string]any{"model": "a", "input": "hello", "tools": []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{map[string]any{"type": "function", "name": "read", "description": strings.Repeat("tool documentation ", 4000), "parameters": map[string]any{"type": "object"}}}}}}
	body, _ := json.Marshal(root)
	withTools, err := estimateNativeResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	if withTools < base+4000 {
		t.Fatalf("nested tool definition omitted: base=%d tools=%d", base, withTools)
	}
	media := []byte(`{"model":"a","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/x.png"},{"type":"input_file","file_url":"https://example.invalid/x.pdf"}]}]}`)
	withMedia, err := estimateNativeResponses(media)
	if err != nil || withMedia < 39000 {
		t.Fatalf("media allowance omitted: %d %v", withMedia, err)
	}
	if string(media) != `{"model":"a","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/x.png"},{"type":"input_file","file_url":"https://example.invalid/x.pdf"}]}]}` {
		t.Fatal("count changed input bytes")
	}
}

func TestNativeRejectsLossyConstraintsAndForeignState(t *testing.T) {
	for _, body := range []string{
		`{"model":"muse-spark-1.3","input":"hello","store":true}`,
		`{"model":"muse-spark-1.3","input":"hello","truncation":"auto"}`,
		`{"model":"muse-spark-1.3","input":"hello","background":true}`,
		`{"model":"muse-spark-1.3","input":"hello","reasoning":{"effort":"none"}}`,
		`{"model":"muse-spark-1.3","input":[{"type":"reasoning","id":"rs_previous","summary":[]}]}`,
		`{"model":"muse-spark-1.3","input":[{"type":"function_call_output","call_id":"missing","output":"ok"}]}`,
		`{"model":"muse-spark-1.3","input":"a","input":"b"}`,
	} {
		if err := ValidateMetaResponsesNative([]byte(body), ResponsesConfig{}); err == nil {
			t.Errorf("accepted invalid payload: %s", body)
		}
	}
}

func TestNativePreservesStatelessReasoningAndToolPairs(t *testing.T) {
	body := []byte(`{"model":"muse-spark-1.3","store":false,"input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque-provider-data"},{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Reading the file."}]},{"type":"function_call","name":"read_file","call_id":"call_1","arguments":"{\"path\":\"file.txt\"}"},{"type":"function_call_output","call_id":"call_1","output":"data"}]}`)
	if err := ValidateMetaResponsesNative(body, ResponsesConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRequiresExplicitExperimentalCapabilities(t *testing.T) {
	body := []byte(`{"model":"muse-spark-1.3","input":"hello","reasoning":{"effort":"max"}}`)
	if err := ValidateMetaResponsesNative(body, ResponsesConfig{}); err == nil {
		t.Fatal("unverified effort accepted")
	}
	cfg := ResponsesConfig{Capabilities: map[string][]string{"muse-spark-1.3": {"effort.max"}}}
	if err := ValidateMetaResponsesNative(body, cfg); err != nil {
		t.Fatal(err)
	}
}
