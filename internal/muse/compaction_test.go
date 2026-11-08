package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These cases port helps/antigravity_compaction_test.go and the Meta native
// compaction preflight regression to the plugin's Call mutation contract.
func TestCompactionSealUnsealAndTampering(t *testing.T) {
	summary := "ORBIT-42, ANSI \x1b[31mred\x1b[0m, quotes \"hi\"\nsecond line"
	capsule, err := sealCompaction(summary, "muse-spark-1.3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(capsule, "cpa-ag-compact-v1:") {
		t.Fatal(capsule)
	}
	got, err := unsealCompaction(capsule)
	if err != nil || got != summary {
		t.Fatalf("summary=%q err=%v", got, err)
	}
	for _, bad := range []string{"invalid-prefix:abc", "cpa-ag-compact-v1:!!", "cpa-ag-compact-v1:AA", capsule[:len(capsule)-4] + "AAAA"} {
		if _, err := unsealCompaction(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCompactionPrepareExpandsAndDropsForeignCapsules(t *testing.T) {
	capsule, err := sealCompaction("Project codename is ORBIT-42.", "muse-spark-1.3")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"foreign"},{"type":"compaction","encrypted_content":"` + capsule + `"},{"type":"message","role":"user","content":"continue"}]}`)
	for _, same := range []bool{true, false} {
		call := &Call{Req: pluginapi.ExecutorRequest{Payload: body, OriginalRequest: body}}
		if !same {
			call.Req.OriginalRequest = append([]byte(" "), body...)
		}
		s, err := (&Executor{}).prepareCompaction(context.Background(), call)
		if err != nil || s != nil {
			t.Fatalf("surface=%v err=%v", s, err)
		}
		for _, payload := range [][]byte{call.Req.Payload, call.Req.OriginalRequest} {
			items := gjson.GetBytes(payload, "input").Array()
			if len(items) != 2 || items[0].Get("role").String() != "developer" || items[0].Get("content.0.text").String() != "Context summary from previous turns:\nProject codename is ORBIT-42." {
				t.Fatalf("expanded=%s", payload)
			}
		}
	}
}

func TestCompactionInvalidCapsuleAndUnsupportedRoute(t *testing.T) {
	for _, original := range []bool{false, true} {
		call := &Call{Req: pluginapi.ExecutorRequest{Payload: []byte(`{"input":[]}`)}}
		bad := []byte(`{"input":[{"type":"compaction","encrypted_content":"cpa-ag-compact-v1:corrupted"}]}`)
		if original {
			call.Req.OriginalRequest = bad
		} else {
			call.Req.Payload = bad
		}
		_, err := (&Executor{}).prepareCompaction(context.Background(), call)
		se, ok := err.(*StatusError)
		if !ok || se.Status != http.StatusBadRequest || se.Scope != ScopeRequest {
			t.Fatalf("error=%v", err)
		}
	}
	_, err := (&Executor{}).prepareCompaction(context.Background(), &Call{Req: pluginapi.ExecutorRequest{Alt: "responses/compact"}})
	se, ok := err.(*StatusError)
	if !ok || se.Status != http.StatusNotImplemented || se.Message != "/responses/compact not supported" {
		t.Fatalf("error=%v", err)
	}
}

func TestCompactionSummaryCallUsesOriginalTriggerAndClearsOptions(t *testing.T) {
	original := []byte(`{"model":"muse-spark-1.3","stream":true,"metadata":{"x":1},"tools":[{"type":"function","name":"bash"}],"tool_choice":"auto","previous_response_id":"old","parallel_tool_calls":true,"additional_tools":[],"truncation":"auto","input":[{"type":"message","role":"user","content":"history from original"},{"type":"compaction_trigger"}]}`)
	call := &Call{Model: "muse-spark-1.3", Stream: true, Req: pluginapi.ExecutorRequest{Format: FormatOpenAIResponse, SourceFormat: FormatOpenAIResponse, Stream: true, Alt: "other", Payload: []byte(`{"input":[]}`), OriginalRequest: original}}
	s, err := (&Executor{}).prepareCompaction(context.Background(), call)
	if err != nil || s == nil {
		t.Fatalf("surface=%v err=%v", s, err)
	}
	summary := compactionSummaryCall(call)
	if summary.Stream || summary.Req.Stream || summary.Req.Format != FormatCodex || summary.Req.Alt != "" || summary.Req.OriginalRequest != nil {
		t.Fatalf("summary=%+v", summary)
	}
	payload := summary.Req.Payload
	if hasResponsesCompactionTrigger(payload) || !bytes.Contains(payload, []byte("history from original")) {
		t.Fatalf("payload=%s", payload)
	}
	for _, field := range []string{"tools", "tool_choice", "previous_response_id", "parallel_tool_calls", "additional_tools", "truncation", "metadata"} {
		if gjson.GetBytes(payload, field).Exists() {
			t.Fatalf("retained %s: %s", field, payload)
		}
	}
	items := gjson.GetBytes(payload, "input").Array()
	if len(items) != 2 || !strings.Contains(items[1].Get("content.0.text").String(), "concise and comprehensive summary") {
		t.Fatal(string(payload))
	}
	if !call.Stream || call.Req.Format != FormatOpenAIResponse || !bytes.Equal(call.Req.OriginalRequest, original) {
		t.Fatal("mutated original summary options")
	}
}

func TestCompactionSummaryStringInputPreservesControlCharacters(t *testing.T) {
	body := []byte(`{"input":"before\u001b[31mafter\nnewline and \"quotes\""}`)
	prepared := prepareCompactionSummaryPayload(body)
	if !json.Valid(prepared) {
		t.Fatal(string(prepared))
	}
	if got := gjson.GetBytes(prepared, "input.0.content.0.text").String(); got != "before\x1b[31mafter\nnewline and \"quotes\"" {
		t.Fatalf("text=%q", got)
	}
}

func TestCompactionResponseAndStreamPreserveSummaryUsage(t *testing.T) {
	body := []byte(`{"id":"resp_sum","status":"completed","error":null,"output":[{"type":"reasoning","summary":[{"text":"thinking"}]},{"type":"message","status":"completed","content":[{"type":"text","text":"Summary with text part."},{"type":"output_text","text":"Another line."}]}],"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":2}}}`)
	response, err := buildResponsesCompactionResponseFromSummary("muse-spark-1.3", body)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(response, "object").String() != "response.compaction" {
		t.Fatal(string(response))
	}
	capsule := gjson.GetBytes(response, "output.0.encrypted_content").String()
	summary, err := unsealCompaction(capsule)
	if err != nil || summary != "Summary with text part.\nAnother line." {
		t.Fatalf("summary=%q err=%v", summary, err)
	}
	chunks, err := buildResponsesCompactionStreamChunksFromResponse("muse-spark-1.3", response)
	if err != nil || len(chunks) != 5 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	types := []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"}
	for i, chunk := range chunks {
		prefix := []byte("event: " + types[i] + "\ndata: ")
		if !bytes.HasPrefix(chunk, prefix) || !bytes.HasSuffix(chunk, []byte("\n\n")) {
			t.Fatalf("chunk=%q", chunk)
		}
		data := bytes.TrimSpace(chunk[len(prefix):])
		if !json.Valid(data) || gjson.GetBytes(data, "type").String() != types[i] || gjson.GetBytes(data, "sequence_number").Int() != int64(i) {
			t.Fatalf("event=%s", data)
		}
		if i == 4 {
			for _, path := range []string{"input_tokens", "output_tokens", "total_tokens", "input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens"} {
				want := gjson.GetBytes(body, "usage."+path).Int()
				if got := gjson.GetBytes(data, "response.usage."+path).Int(); got != want {
					t.Fatalf("%s=%d want=%d", path, got, want)
				}
			}
			if gjson.GetBytes(data, "response.output.0.encrypted_content").String() != capsule {
				t.Fatal("stream resealed capsule")
			}
		}
	}
}

func TestCompactionRejectsIncompleteOrMissingSummary(t *testing.T) {
	valid := []byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"summary"}]}]}`)
	for _, path := range []string{"status", "incomplete_details", "error", "output.0.status", "type"} {
		value := "incomplete"
		if path == "type" {
			value = "response.failed"
		}
		body, _ := sjson.SetBytes(valid, path, value)
		if _, err := buildResponsesCompactionResponseFromSummary("muse-spark-1.3", body); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range []string{`{}`, `{"status":"completed","output":[{"type":"reasoning"}]}`} {
		if _, err := buildResponsesCompactionResponseFromSummary("muse-spark-1.3", []byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if _, err := buildResponsesCompactionStreamChunksFromResponse("muse-spark-1.3", []byte(`{"output":[]}`)); err == nil {
		t.Fatal("accepted missing compaction item")
	}
}
