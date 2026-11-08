package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func coreSSE(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
	}
	return b.String()
}

// coreStream runs through the public asynchronous executor and real stream_read
// callbacks. Each upstream body is split mid-frame, not supplied as open chunks.
func coreStream(t *testing.T, native bool, bodies ...string) (*Executor, *coreFakeHost, [][]byte, *pluginabi.Error) {
	t.Helper()
	f := coreHost(t)
	var mu sync.Mutex
	opened := 0
	chunks := make(map[string][]string)
	f.handle("host.http.do_stream", func(raw json.RawMessage) (any, error) {
		var req coreHTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Headers.Get("Authorization") != "Bearer test-only" || req.URL != "https://example.invalid/v1/responses" {
			t.Errorf("stream ignored selected credential: %+v", req)
		}
		mu.Lock()
		defer mu.Unlock()
		if opened >= len(bodies) {
			return nil, fmt.Errorf("unexpected stream reopen %d", opened+1)
		}
		body := bodies[opened]
		opened++
		id := fmt.Sprintf("upstream-%d", opened)
		mid := len(body) / 2
		chunks[id] = []string{body[:mid], body[mid:]}
		return map[string]any{"status_code": 200, "headers": http.Header{"Content-Type": {"text/event-stream"}}, "stream_id": id}, nil
	})
	f.handle("host.http.stream_read", func(raw json.RawMessage) (any, error) {
		id := gjson.GetBytes(raw, "stream_id").String()
		mu.Lock()
		defer mu.Unlock()
		parts := chunks[id]
		if len(parts) == 0 {
			return map[string]any{"done": true}, nil
		}
		chunks[id] = parts[1:]
		return map[string]any{"payload": []byte(parts[0]), "done": len(parts) == 1}, nil
	})
	var output [][]byte
	f.handle("host.stream.emit", func(raw json.RawMessage) (any, error) {
		var req struct {
			Payload []byte `json:"payload"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		output = append(output, req.Payload)
		return struct{}{}, nil
	})
	done := make(chan *pluginabi.Error, 1)
	f.handle("host.stream.close", func(raw json.RawMessage) (any, error) {
		var req struct {
			Error *pluginabi.Error `json:"error_detail"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		done <- req.Error
		return struct{}{}, nil
	})
	cfg := &Config{}
	format := FormatCodex
	if native {
		cfg.Responses.Mode = "native"
		format = FormatOpenAIResponse
	}
	e := NewExecutor(cfg, nil)
	_, err := e.ExecuteStream(context.Background(), selectedRequest(pluginapi.ExecutorRequest{Model: "muse-spark-1.3", Format: format, Payload: []byte(`{"input":[{"role":"user","content":"hi"}]}`)}, ""), "", "downstream")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case detail := <-done:
		return e, f, output, detail
	case <-time.After(5 * time.Second):
		t.Fatal("stream pump did not close downstream")
		return nil, nil, nil, nil
	}
}

func TestCoreResponsesStreamQuotaFilteredAndFetched(t *testing.T) {
	usage := `{"type":"response.subscription_usage","subscription":{"weekly":{"used_percent":25,"resets_at":1893456000},"window":{"used_percent":60,"resets_at":1893459600,"window_duration_mins":300}}}`
	// Also cover the upstream event-name-only usage variant.
	unnamed := "event: response.subscription_usage\ndata: " + strings.Replace(strings.Replace(usage, `"type":"response.subscription_usage",`, "", 1), `"used_percent":25`, `"used_percent":50`, 1) + "\n\n"
	e, _, output, detail := coreStream(t, false, coreSSE(usage)+unnamed+coreSSE(coreCompleted))
	if detail != nil || len(output) != 1 || strings.Contains(string(output[0]), "subscription") {
		t.Fatalf("usage leaked or damaged stream: chunks=%q error=%+v", output, detail)
	}
	quota, err := e.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "selected"})
	if err != nil || len(quota.Groups) != 1 || len(quota.Groups[0].Buckets) != 2 {
		t.Fatalf("quota=%+v error=%v", quota, err)
	}
	weekly, window := quota.Groups[0].Buckets[0], quota.Groups[0].Buckets[1]
	if weekly.Window != "weekly" || weekly.RemainingFraction != .5 || weekly.ResetTime != "2030-01-01T00:00:00Z" || window.Window != "5h0m0s" || math.Abs(window.RemainingFraction-.4) > 1e-9 || window.ResetTime != "2030-01-01T01:00:00Z" {
		t.Fatalf("wrong quota buckets: %+v", quota)
	}
}

func TestCoreResponsesStreamRetriesBeforeAnswerOnly(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_test","output":[]}}`
	progress := `{"type":"response.in_progress","response":{"id":"resp_test"}}`
	failure := `{"type":"response.failed","response":{"error":{"code":"server_error","message":"failed to generate a response"}}}`
	answer := `{"type":"response.output_text.delta","delta":"visible answer"}`
	for _, visible := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre-answer-reopens", true: "visible-answer-no-retry"}[visible], func(t *testing.T) {
			first := coreSSE(created, progress)
			if visible {
				first += coreSSE(answer)
			}
			first += coreSSE(failure)
			_, f, output, detail := coreStream(t, false, first, coreSSE(created, progress, answer, coreCompleted))
			counts := make(map[string]int)
			for _, chunk := range output {
				counts[gjson.Get(strings.TrimPrefix(string(chunk), "data: "), "type").String()]++
			}
			if counts["response.created"] != 1 || counts["response.in_progress"] != 1 || counts["response.output_text.delta"] != 1 || counts["response.failed"] != 0 {
				t.Fatalf("replayed lifecycle or error frames: %v (%q)", counts, output)
			}
			if visible {
				if len(f.calls("host.http.do_stream")) != 1 || detail == nil || detail.HTTPStatus != 502 || counts["response.completed"] != 0 {
					t.Fatalf("post-answer failure retried/hidden: error=%+v counts=%v opens=%d", detail, counts, len(f.calls("host.http.do_stream")))
				}
			} else if len(f.calls("host.http.do_stream")) != 2 || detail != nil || counts["response.completed"] != 1 {
				t.Fatalf("pre-answer failure not recovered: error=%+v counts=%v opens=%d", detail, counts, len(f.calls("host.http.do_stream")))
			}
		})
	}
}

func TestCoreResponsesStreamNativeTruncation(t *testing.T) {
	_, f, _, detail := coreStream(t, true, coreSSE(`{"type":"response.output_text.delta","delta":"partial"}`))
	if detail == nil || detail.HTTPStatus != 502 || detail.Code != "stream_truncated" || len(f.calls("host.http.do_stream")) != 1 {
		t.Fatalf("native EOF not surfaced: %+v", detail)
	}
}

func TestCoreResponsesStreamFraming(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			_, _, output, detail := coreStream(t, native, coreSSE(coreCompleted))
			if detail != nil || len(output) != 1 {
				t.Fatalf("output=%q error=%+v", output, detail)
			}
			want := "data: " + coreCompleted
			if native {
				want = "event: response.completed\n" + want + "\n\n"
			}
			if string(output[0]) != want {
				t.Fatalf("wire framing: got %q want %q", output[0], want)
			}
		})
	}
}
