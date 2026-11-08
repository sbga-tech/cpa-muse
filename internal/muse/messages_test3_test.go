package muse

// Consumer-visible regression cases ported from fork helps/meta_messages_test3_test.go.

import (
	"bytes"
	"encoding/json"
	"testing"
)

const nativeSearchDirectContentFixture = `[
 {"data":"Q-PaDgHdjkb4lsOR15uENchy<truncated 1402 chars>","type":"redacted_thinking"},
 {"data":"Q-PaDgF78bY_9LA1-P-YQfrV<truncated 1402 chars>","type":"redacted_thinking"},
 {"text":"I'll load the weather tool, then fetch Paris conditions.","type":"text"},
 {"id":"tsc_call_2f356037-714d-4c3f-8923-616a6253bb74","input":{"paths":[""]},"name":"tool_search","type":"server_tool_use"},
 {"text":"Tool loaded — now checking the weather in Paris.","type":"text"},
 {"id":"call_ae4fd9a2-09d2-4f31-b923-9b95ce855cc2","input":{"city":"Paris"},"name":"get_weather","type":"tool_use"}
]`

func nativeSearchHistoryFixture(t *testing.T) map[string]any {
	t.Helper()
	body := []byte(`{"model":"muse-wonder-full-1.0","max_tokens":2048,"tools":[{"type":"tool_search"},{"name":"get_weather","description":"Read the weather for a city.","defer_loading":true,"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"messages":[{"role":"user","content":"Use your tools to get the weather in Paris."},{"role":"assistant","content":` + nativeSearchDirectContentFixture + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_ae4fd9a2-09d2-4f31-b923-9b95ce855cc2","content":"Paris: clear."}]}]}`)
	root, err := DecodeMetaMessages(body)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMetaMessagesReportedNativeSearchSelectionReplaysUnchanged(t *testing.T) {
	for _, model := range []string{"muse-wonder-full-1.0", "muse-wonder-1.0", "muse-spark-1.2", "muse-spark-1.3-contributor"} {
		for _, count := range []bool{false, true} {
			root := nativeSearchHistoryFixture(t)
			root["model"] = model
			before, _ := json.Marshal(root)
			if err := ValidateMetaMessages(root, count, MetaMessagesPolicy{Capabilities: []string{"hosted-tool-search-o1"}}); err != nil {
				t.Fatalf("%s count=%v: %v", model, count, err)
			}
			after, _ := json.Marshal(root)
			if !bytes.Equal(before, after) {
				t.Fatal("history validation rewrote the supplied replay")
			}
			if bytes.Contains(after, []byte("tool_search_tool_result")) {
				t.Fatal("a hosted result was synthesized")
			}
		}
	}
	if err := ValidateMetaMessages(nativeSearchHistoryFixture(t), false, MetaMessagesPolicy{}); err == nil {
		t.Fatal("native search history bypassed the capability gate")
	}
}

func TestMetaMessagesDirectSearchKeepsHistoryClosureChecks(t *testing.T) {
	for _, scenario := range []string{"missing-result", "wrong-result", "duplicate-result", "result-after-text", "custom-before-search", "unrelated-hosted", "unfinished-later-search", "no-custom-call"} {
		t.Run(scenario, func(t *testing.T) {
			root := nativeSearchHistoryFixture(t)
			messages := root["messages"].([]any)
			assistant := messages[1].(map[string]any)
			blocks := assistant["content"].([]any)
			user := messages[2].(map[string]any)
			results := user["content"].([]any)
			switch scenario {
			case "missing-result":
				root["messages"] = messages[:2]
			case "wrong-result":
				results[0].(map[string]any)["tool_use_id"] = "tsc_call_2f356037-714d-4c3f-8923-616a6253bb74"
			case "duplicate-result":
				user["content"] = append(results, results[0])
			case "result-after-text":
				user["content"] = append([]any{map[string]any{"type": "text", "text": "Before results"}}, results...)
			case "custom-before-search":
				blocks[3], blocks[5] = blocks[5], blocks[3]
			case "unrelated-hosted":
				blocks[3].(map[string]any)["name"] = "web_search"
			case "unfinished-later-search":
				assistant["content"] = append(blocks, map[string]any{"type": "server_tool_use", "id": "later_search", "name": "tool_search", "input": map[string]any{"paths": []any{""}}})
			case "no-custom-call":
				assistant["content"] = blocks[:5]
				root["messages"] = messages[:2]
			}
			policy := MetaMessagesPolicy{Capabilities: []string{"hosted-tool-search-o1", "hosted-web-search-o1"}}
			if err := ValidateMetaMessages(root, false, policy); err == nil {
				t.Fatal("unsupported or unmatched continuation was accepted")
			}
		})
	}
}

func TestMetaMessagesMeteringPreservesReportedZerosAndCacheAbsence(t *testing.T) {
	data := []byte(`{"message":{"content":[],"id":"msg_6ace9535bcae932fa10e48fa","model":"muse-wonder-1.0","role":"assistant","stop_reason":null,"stop_sequence":null,"type":"message","usage":{"input_tokens":0,"output_tokens":0}},"type":"message_start"}`)
	var metering MetaMessagesMetering
	metering.Observe(data, false)
	if metering.Status() != "partial" || metering.InputTokens == nil || *metering.InputTokens != 0 || metering.OutputTokens == nil || *metering.OutputTokens != 0 {
		t.Fatalf("observed zero changed: %+v", metering)
	}
	if metering.CacheReadInputTokens != nil || metering.CacheCreationInputTokens != nil {
		t.Fatal("missing cache values were invented")
	}
	fields := metering.logFields()
	if fields["input_usage_source"] != "message.usage.input_tokens" || fields["output_usage_source"] != "message.usage.output_tokens" {
		t.Fatalf("missing usage provenance: %+v", fields)
	}
}

func TestMetaMessagesMeteringEventPrecedenceAndExplicitCorrection(t *testing.T) {
	var metering MetaMessagesMetering
	metering.Observe([]byte(`{"type":"message_start","usage":{"input_tokens":0,"output_tokens":0},"message":{"usage":{"input_tokens":11,"cache_read_input_tokens":7}}}`), false)
	if metering.InputTokens == nil || *metering.InputTokens != 11 || metering.CacheReadInputTokens == nil || *metering.CacheReadInputTokens != 7 {
		t.Fatalf("canonical nested usage was masked: %+v", metering)
	}
	metering.Observe([]byte(`{"type":"message_delta","usage":{"output_tokens":3}}`), true)
	if *metering.InputTokens != 11 || *metering.CacheReadInputTokens != 7 {
		t.Fatal("omitted counters overwrote known values")
	}
	metering.Observe([]byte(`{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`), true)
	if *metering.InputTokens != 0 || *metering.OutputTokens != 0 || *metering.CacheReadInputTokens != 0 {
		t.Fatal("authoritative explicit zero correction was ignored")
	}
	if metering.inputSource != "usage.input_tokens" || metering.Status() != "complete" {
		t.Fatal("final correction source/status was lost")
	}
	var zero MetaMessagesMetering
	zero.Observe([]byte(`{"type":"message_start","usage":{"input_tokens":11},"message":{"usage":{"input_tokens":0}}}`), false)
	if zero.InputTokens == nil || *zero.InputTokens != 0 {
		t.Fatal("a positive alternate value replaced canonical zero")
	}
}

func TestMetaMessagesMeteringCacheCountersAreNotInferredInput(t *testing.T) {
	var metering MetaMessagesMetering
	metering.Observe([]byte(`{"type":"message_start","message":{"usage":{"cache_read_input_tokens":21,"cache_creation_input_tokens":4}}}`), false)
	if metering.Status() != "partial" || metering.InputTokens != nil || metering.OutputTokens != nil || *metering.CacheReadInputTokens != 21 || *metering.CacheCreationInputTokens != 4 {
		t.Fatalf("raw cache counters confused with input: %+v", metering)
	}
	for _, invalid := range []string{"-1", "0.5", "9223372036854775808", `"16"`} {
		var unknown MetaMessagesMetering
		unknown.Observe([]byte(`{"usage":{"input_tokens":`+invalid+`}}`), false)
		if unknown.InputTokens != nil {
			t.Fatalf("invalid counter accepted: %s", invalid)
		}
	}
}
