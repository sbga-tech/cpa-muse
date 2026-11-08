package muse

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const metaIncompleteOutputFixture = `{"type":"response.incomplete","sequence_number":3,"response":{"id":"resp_fixture","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":14,"output_tokens":256,"output_tokens_details":{"reasoning_tokens":253},"total_tokens":270}}}`

func TestNativeOutputRestoresOnlyActualAddedMetadata(t *testing.T) {
	var state MetaResponsesOutput
	added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_fixture","status":"in_progress","summary":[],"type":"reasoning"}}`)
	if got := state.Transform(added); !bytes.Equal(got, added) {
		t.Fatal("added event changed")
	}
	terminal := state.Transform([]byte(metaIncompleteOutputFixture))
	item := gjson.GetBytes(terminal, "response.output.0")
	if item.Raw != gjson.GetBytes(added, "item").Raw || gjson.GetBytes(terminal, "response.output.#").Int() != 1 {
		t.Fatalf("snapshot changed or missing: %s", terminal)
	}
	if item.Get("encrypted_content").Exists() || item.Get("content").Exists() || item.Get("summary.#").Int() != 0 {
		t.Fatalf("reasoning content invented: %s", terminal)
	}
	if gjson.GetBytes(terminal, "type").String() != "response.incomplete" || gjson.GetBytes(terminal, "sequence_number").Int() != 3 || gjson.GetBytes(terminal, "response.usage.output_tokens_details.reasoning_tokens").Int() != 253 {
		t.Fatalf("terminal metadata changed: %s", terminal)
	}
}

func TestNativeOutputRetainsActualSummaryDeltasWithoutInventingCiphertext(t *testing.T) {
	var state MetaResponsesOutput
	state.Transform([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_fixture","status":"in_progress","summary":[],"type":"reasoning"}}`))
	state.Transform([]byte(`{"type":"response.reasoning_summary_part.added","item_id":"rs_fixture","summary_index":0,"part":{"type":"summary_text","text":""}}`))
	delta := []byte(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_fixture","summary_index":0,"delta":"a"}`)
	for i := 0; i < 5000; i++ {
		if got := state.Transform(delta); !bytes.Equal(got, delta) {
			t.Fatal("delta event changed")
		}
	}
	terminal := state.Transform([]byte(metaIncompleteOutputFixture))
	if gjson.GetBytes(terminal, "response.output.0.summary.0.text").String() != strings.Repeat("a", 5000) {
		t.Fatal("observed summary deltas were lost or duplicated")
	}
	if gjson.GetBytes(terminal, "response.output.0.encrypted_content").Exists() {
		t.Fatal("ciphertext invented from summary")
	}
	state.Transform([]byte(`{"type":"response.reasoning_summary_text.done","item_id":"rs_fixture","summary_index":0,"text":"authoritative text"}`))
	terminal = state.Transform([]byte(metaIncompleteOutputFixture))
	if gjson.GetBytes(terminal, "response.output.0.summary.0.text").String() != "authoritative text" {
		t.Fatalf("summary done snapshot was not authoritative: %s", terminal)
	}
}

func TestNativeOutputDoneAndTerminalFieldsWinWithoutLosingOmittedEvidence(t *testing.T) {
	var state MetaResponsesOutput
	state.Transform([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_fixture","type":"reasoning","summary":[],"encrypted_content":"earlier-opaque","extension":9007199254740993}}`))
	state.Transform([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_fixture","type":"reasoning","summary":[{"type":"summary_text","text":"actual summary"}],"encrypted_content":"final-opaque"}}`))
	state.Transform([]byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_fixture","type":"message","role":"assistant","status":"in_progress","content":[]}}`))
	terminal := state.Transform([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"actual message"}]}]}}`))
	if gjson.GetBytes(terminal, "response.output.#").Int() != 2 || gjson.GetBytes(terminal, "response.output.0.id").String() != "rs_fixture" || gjson.GetBytes(terminal, "response.output.1.id").String() != "msg_fixture" {
		t.Fatalf("missing, duplicate, or reordered items: %s", terminal)
	}
	if gjson.GetBytes(terminal, "response.output.0.encrypted_content").String() != "final-opaque" || gjson.GetBytes(terminal, "response.output.0.extension").Raw != "9007199254740993" {
		t.Fatalf("observed snapshot fields changed: %s", terminal)
	}
	if gjson.GetBytes(terminal, "response.output.1.status").String() != "incomplete" || gjson.GetBytes(terminal, "response.output.1.content.0.text").String() != "actual message" {
		t.Fatalf("terminal snapshot was overwritten: %s", terminal)
	}
	terminal = state.Transform([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"id":"rs_fixture","type":"reasoning","summary":[],"encrypted_content":"terminal-opaque"}]}}`))
	if gjson.GetBytes(terminal, "response.output.0.encrypted_content").String() != "terminal-opaque" || gjson.GetBytes(terminal, "response.output.0.summary.#").Int() != 0 {
		t.Fatalf("explicit terminal fields were not authoritative: %s", terminal)
	}
}

func TestNativeOutputIgnoresUsageAndUnknownItemDeltas(t *testing.T) {
	var state MetaResponsesOutput
	state.Transform([]byte(`{"type":"response.reasoning_summary_text.delta","item_id":"never-added","summary_index":0,"delta":"unmatched"}`))
	terminal := []byte(metaIncompleteOutputFixture)
	if got := state.Transform(terminal); !bytes.Equal(got, terminal) {
		t.Fatalf("item invented without a snapshot: %s", got)
	}
}

func TestNativeOutputIdentityAndSparseIndexesDoNotCrossContaminate(t *testing.T) {
	var state MetaResponsesOutput
	state.Transform([]byte(`{"type":"response.output_item.added","output_index":9223372036854775806,"item":{"id":"rs_old","type":"reasoning","encrypted_content":"old-opaque","summary":[]}}`))
	state.Transform([]byte(`{"type":"response.output_item.done","output_index":9223372036854775806,"item":{"id":"rs_new","type":"reasoning","summary":[]}}`))
	state.Transform([]byte(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_old","output_index":9223372036854775806,"summary_index":0,"delta":"old text"}`))
	state.Transform([]byte(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_new","summary_index":9223372036854775806,"delta":"observed"}`))
	terminal := state.Transform([]byte(metaIncompleteOutputFixture))
	if gjson.GetBytes(terminal, "response.output.#").Int() != 1 || gjson.GetBytes(terminal, "response.output.0.id").String() != "rs_new" || gjson.GetBytes(terminal, "response.output.0.encrypted_content").Exists() {
		t.Fatalf("item identity mixed old evidence: %s", terminal)
	}
	if gjson.GetBytes(terminal, "response.output.0.summary.#").Int() != 1 || gjson.GetBytes(terminal, "response.output.0.summary.0.text").String() != "observed" {
		t.Fatalf("sparse index was not bounded by observed parts: %s", terminal)
	}
}

func TestNativeOutputStreamLineKeepsFraming(t *testing.T) {
	var state MetaResponsesOutput
	state.StreamLine([]byte("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_fixture\",\"type\":\"reasoning\",\"summary\":[]}}\r\n\r\n"))
	line := []byte("data: " + metaIncompleteOutputFixture + "\r\n\r\n")
	got := state.StreamLine(line)
	if !bytes.HasPrefix(got, []byte("data: ")) || !bytes.HasSuffix(got, []byte("\r\n\r\n")) || gjson.GetBytes(bytes.TrimSpace(got[5:]), "response.output.#").Int() != 1 {
		t.Fatalf("SSE framing changed: %s", got)
	}
}
