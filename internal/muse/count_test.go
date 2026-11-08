package muse

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// Ported from meta_responses_count_basis_test.go, with executor output formats
// replacing the fork's translator registry and preflight response interception.
func TestCountResponsesNormalizedLegacyAndOutputFormats(t *testing.T) {
	body := []byte(`{"model":"muse-spark-1.3","input":"Hello there","reasoning":{"effort":"high"}}`)
	prepared, err := convertResponsesRequestToCodex("muse-spark-1.3", body)
	if err != nil {
		t.Fatal(err)
	}
	prepared = setString(prepared, "instructions", "")
	want, err := estimateNativeResponses(prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{FormatOpenAIResponse, FormatCodex} {
		t.Run(format, func(t *testing.T) {
			payload := body
			if format == FormatCodex {
				payload = prepared
			}
			call := &Call{Model: "muse-spark-1.3", Req: pluginapi.ExecutorRequest{Format: format, SourceFormat: FormatOpenAIResponse, Alt: "responses/input_tokens", Payload: payload}}
			resp, err := countResponsesLocal(call)
			if err != nil {
				t.Fatal(err)
			}
			path := "input_tokens"
			if format == FormatCodex {
				path = "response.usage.input_tokens"
			}
			if got := gjson.GetBytes(resp.Payload, path).Int(); got != want {
				t.Fatalf("count=%d, normalized=%d; %s", got, want, resp.Payload)
			}
			if resp.Headers.Get("X-CPA-Token-Count-Basis") != "normalized-legacy" {
				t.Fatalf("headers=%v", resp.Headers)
			}
			if resp.Headers.Get("X-CPA-Token-Count-Method") != "o200k-semantic-json-vision-heuristic-v2" {
				t.Fatalf("headers=%v", resp.Headers)
			}
			for _, path := range []string{"choices", "content", "candidates", "output", "response.output"} {
				if gjson.GetBytes(resp.Payload, path).Exists() {
					t.Fatalf("count reply looks like a completion: %s", resp.Payload)
				}
			}
		})
	}
}

func TestCountCodexInputIncludesToolsAndSchema(t *testing.T) {
	body := []byte(`{"instructions":"Be useful","input":[{"type":"message","content":[{"type":"input_text","text":"hello"}]},{"type":"function_call","name":"bash","arguments":"{\"x\":1}"},{"type":"function_call_output","output":"result"}],"tools":[{"name":"bash","description":"run shell","parameters":{"type":"object"}}],"text":{"format":{"name":"answer","schema":{"type":"string"}}}}`)
	codec, err := claudeInputTokenizer()
	if err != nil {
		t.Fatal(err)
	}
	want, err := codec.Count("Be useful\nhello\nbash\n{\"x\":1}\nresult\nbash\nrun shell\n{\"type\":\"object\"}\nanswer\n{\"type\":\"string\"}")
	if err != nil {
		t.Fatal(err)
	}
	call := &Call{Model: "muse-spark-1.3", Req: pluginapi.ExecutorRequest{Format: FormatCodex, Payload: body}}
	resp, err := countResponsesLocal(call)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(resp.Payload, "response.usage.input_tokens").Int(); got != int64(want) {
		t.Fatalf("count=%d want=%d", got, want)
	}
	if gjson.GetBytes(resp.Payload, "response.usage.output_tokens").Int() != 0 {
		t.Fatal(string(resp.Payload))
	}
}

func TestCountRejectsInvalidJSON(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `{"input":1,"input":2}`, `{"input":[{"x":1,"x":2}]}`, `{"input":"x"} {}`} {
		_, err := countResponsesLocal(&Call{Req: pluginapi.ExecutorRequest{Format: FormatOpenAIResponse, Payload: []byte(body)}})
		se, ok := err.(*StatusError)
		if !ok || se.Status != http.StatusBadRequest {
			t.Fatalf("body=%s error=%v", body, err)
		}
	}
}

func TestCountSemanticMediaAndNonSemanticFields(t *testing.T) {
	plain := []byte(`{"input":[{"type":"message","content":[{"type":"input_text","text":"Hello"}]}]}`)
	withExtras := []byte(`{"model":"ignored","stream":true,"metadata":{"ignored":"long noise"},"input":[{"type":"message","content":[{"type":"input_text","text":"Hello"}]}]}`)
	base, err := estimateNativeResponses(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := estimateNativeResponses(withExtras)
	if err != nil || got != base {
		t.Fatalf("extra fields count=%d base=%d err=%v", got, base, err)
	}
	media := map[string]any{"type": "input_file", "file_data": "very long data"}
	if got := estimateMetaNativeMedia(media); got != 32768 || media["file_data"] != "" {
		t.Fatalf("file estimate=%d media=%v", got, media)
	}
	body := []byte(`{"input":[{"type":"input_video","video_url":"data:video/mp4;base64,AAAA"}]}`)
	count, err := estimateNativeResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	semantic, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "input_video", "video_url": ""}}})
	codec, _ := claudeInputTokenizer()
	tokens, _ := codec.Count(string(semantic))
	want := int64(math.Ceil(float64(tokens+65536+32) * 1.15))
	if count != want {
		t.Fatalf("video count=%d want=%d", count, want)
	}
}

func TestCountCompactionTriggerCountsWithoutSummaryExecution(t *testing.T) {
	e := &Executor{}
	call := &Call{Req: pluginapi.ExecutorRequest{Format: FormatCodex, Payload: []byte(`{"input":[{"type":"compaction_trigger"}]}`)}}
	s, err := e.prepareCompaction(context.Background(), call)
	if err != nil || s == nil {
		t.Fatalf("surface=%v err=%v", s, err)
	}
	if _, err := s.count(context.Background(), call); err != nil {
		t.Fatal(err)
	}
}
