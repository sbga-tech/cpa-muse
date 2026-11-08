package muse

import (
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// buildResponsesCompactionResponseFromSummary ports the Responses-only path in
// helps/antigravity_compaction.go, preserving its capsule and usage wire format.
func buildResponsesCompactionResponseFromSummary(model string, payload []byte) ([]byte, error) {
	if err := validateCompactionSummaryStatus(payload); err != nil {
		return nil, err
	}
	var parts []string
	for _, item := range gjson.GetBytes(payload, "output").Array() {
		if item.Get("type").String() != "message" {
			continue
		}
		content := item.Get("content")
		if content.IsArray() {
			for _, part := range content.Array() {
				if kind := part.Get("type").String(); kind == "text" || kind == "output_text" {
					if text := part.Get("text").String(); text != "" {
						parts = append(parts, text)
					}
				}
			}
		} else if content.Type == gjson.String && content.String() != "" {
			parts = append(parts, content.String())
		}
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("extract summary: no summary text found in upstream response")
	}
	capsule, err := sealCompaction(strings.Join(parts, "\n"), model)
	if err != nil {
		return nil, fmt.Errorf("seal compaction capsule: %w", err)
	}
	item := []byte(`{"type":"compaction","status":"completed"}`)
	item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("cmp_ag_compact_%d", time.Now().UnixNano()))
	item, _ = sjson.SetBytes(item, "encrypted_content", capsule)
	response := []byte(`{"object":"response.compaction","status":"completed"}`)
	response, _ = sjson.SetBytes(response, "id", fmt.Sprintf("resp_ag_compact_%d", time.Now().UnixNano()))
	response, _ = sjson.SetBytes(response, "created_at", time.Now().Unix())
	response, _ = sjson.SetBytes(response, "model", model)
	response, _ = sjson.SetRawBytes(response, "output", []byte("["+string(item)+"]"))
	response, _ = sjson.SetRawBytes(response, "usage", compactionUsage(payload))
	return response, nil
}

func validateCompactionSummaryStatus(payload []byte) error {
	status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "status").String()))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.status").String()))
	}
	if status != "completed" {
		if status == "" {
			status = "missing"
		}
		return fmt.Errorf("compaction summary response status is %q, want \"completed\"", status)
	}
	if typ := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "type").String())); typ == "response.incomplete" || typ == "response.failed" {
		return fmt.Errorf("compaction summary response terminal event is %q", typ)
	}
	for _, path := range []string{"incomplete_details", "response.incomplete_details", "error", "response.error"} {
		if node := gjson.GetBytes(payload, path); node.Exists() && node.Type != gjson.Null {
			return fmt.Errorf("compaction summary response contains %s", path)
		}
	}
	for _, item := range gjson.GetBytes(payload, "output").Array() {
		if status := strings.ToLower(strings.TrimSpace(item.Get("status").String())); status != "" && status != "completed" {
			return fmt.Errorf("compaction summary output item status is %q, want \"completed\"", status)
		}
	}
	return nil
}

func compactionUsage(payload []byte) []byte {
	usage := gjson.GetBytes(payload, "response.usage")
	if usage.Get("input_tokens").Int() <= 0 && usage.Get("prompt_tokens").Int() <= 0 && usage.Get("output_tokens").Int() <= 0 && usage.Get("completion_tokens").Int() <= 0 && usage.Get("total_tokens").Int() <= 0 {
		usage = gjson.GetBytes(payload, "usage")
	}
	first := func(paths ...string) int64 {
		for _, path := range paths {
			if value := usage.Get(path); value.Exists() {
				return value.Int()
			}
		}
		return 0
	}
	input := first("prompt_tokens", "input_tokens")
	output := first("completion_tokens", "output_tokens")
	total := first("total_tokens")
	if total == 0 {
		total = input + output
	}
	out := []byte(`{"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "input_tokens", input)
	out, _ = sjson.SetBytes(out, "output_tokens", output)
	out, _ = sjson.SetBytes(out, "total_tokens", total)
	out, _ = sjson.SetBytes(out, "input_tokens_details.cached_tokens", first("prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens"))
	out, _ = sjson.SetBytes(out, "output_tokens_details.reasoning_tokens", first("completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens"))
	return out
}

func buildResponsesCompactionStreamChunksFromResponse(model string, payload []byte) ([][]byte, error) {
	item := gjson.GetBytes(payload, "output.0")
	capsule := item.Get("encrypted_content").String()
	if item.Get("type").String() != "compaction" || capsule == "" {
		return nil, fmt.Errorf("extract summary: compaction item missing")
	}
	now := time.Now().Unix()
	// The fork intentionally creates new response/item IDs for the stream.
	completedItem := []byte(`{"type":"compaction","status":"completed"}`)
	completedItem, _ = sjson.SetBytes(completedItem, "id", fmt.Sprintf("cmp_ag_compact_%d", time.Now().UnixNano()))
	completedItem, _ = sjson.SetBytes(completedItem, "encrypted_content", capsule)
	inProgressItem, _ := sjson.SetBytes(completedItem, "status", "in_progress")
	created := []byte(`{"object":"response","status":"in_progress","background":false,"error":null,"output":[]}`)
	created, _ = sjson.SetBytes(created, "id", fmt.Sprintf("resp_ag_compact_%d", time.Now().UnixNano()))
	created, _ = sjson.SetBytes(created, "created_at", now)
	created, _ = sjson.SetBytes(created, "model", model)
	completed, _ := sjson.SetBytes(created, "status", "completed")
	completed, _ = sjson.SetBytes(completed, "completed_at", now)
	completed, _ = sjson.SetRawBytes(completed, "output", []byte("["+string(completedItem)+"]"))
	completed, _ = sjson.SetRawBytes(completed, "usage", compactionUsage(payload))
	types := []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"}
	values := [][]byte{created, created, inProgressItem, completedItem, completed}
	chunks := make([][]byte, len(types))
	for i, typ := range types {
		event := []byte(`{}`)
		event, _ = sjson.SetBytes(event, "type", typ)
		event, _ = sjson.SetBytes(event, "sequence_number", i)
		path := "response"
		if i == 2 || i == 3 {
			path = "item"
			event, _ = sjson.SetBytes(event, "output_index", 0)
		}
		event, _ = sjson.SetRawBytes(event, path, values[i])
		chunks[i] = []byte("event: " + typ + "\ndata: " + string(event) + "\n\n")
	}
	return chunks, nil
}
