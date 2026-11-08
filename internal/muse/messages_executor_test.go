package muse

// Ported consumer behavior from fork meta_messages_executor*_test.go and
// meta_messages_url_watchdog_test.go. Host callback fixtures replace CPA's
// conductor registry and internal usage reporter, which plugins cannot import.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DDD0s/cpa-muse/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const messagesTestResponse = `{"id":"msg_test","type":"message","role":"assistant","model":"muse-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`
const messagesTestSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"muse-test\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

type messagesFixtureRequest struct {
	URL     string
	Headers http.Header
	Body    []byte
}

func messagesFixture(t *testing.T, handler func(method string, request messagesFixtureRequest) any) *Executor {
	t.Helper()
	old := metaMessagesCircuit
	metaMessagesCircuit = NewMetaMessagesCircuit(nil)
	t.Cleanup(func() { host.SetTransport(nil); metaMessagesCircuit = old })
	host.SetTransport(func(method string, raw []byte) ([]byte, error) {
		var request messagesFixtureRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		result := any(map[string]any{})
		if method == pluginabi.MethodHostHTTPDo || method == pluginabi.MethodHostHTTPDoStream {
			result = handler(method, request)
		}
		return json.Marshal(map[string]any{"ok": true, "result": result})
	})
	return NewExecutor(&Config{Messages: MessagesConfig{Mode: "enabled", AllowSubscription: true}}, nil)
}
func messagesCall(e *Executor, stream bool) *Call {
	payload := []byte(`{"model":"client-alias","max_tokens":2048,"messages":[{"role":"user","content":"hello"}]}`)
	call := e.newCall(selectedRequest(pluginapi.ExecutorRequest{Model: "client-alias", Format: FormatClaude, SourceFormat: FormatClaude, Payload: payload, OriginalRequest: bytes.Clone(payload)}, "messages-test"), "messages-test", stream)
	call.Model = "muse-test"
	return call
}
func messagesBuffered(status int, body string) any {
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}
func messagesStreaming(status int, body string) any {
	return map[string]any{"status_code": status, "headers": http.Header{"Content-Type": {"text/event-stream"}}, "chunks": []map[string]any{{"Payload": []byte(body)}}}
}
func messagesStatus(t *testing.T, err error, status int, code string) *StatusError {
	t.Helper()
	var se *StatusError
	if !errors.As(err, &se) || se.Status != status || (code != "" && se.Code != code) || !json.Valid(se.Body) {
		t.Fatalf("status=%d code=%s error=%#v", status, code, err)
	}
	return se
}

func TestMessagesPreflightAndLocalCountAvoidUpstream(t *testing.T) {
	for _, scenario := range []string{"disabled", "invalid", "local-count", "disabled-stream", "oversized", "suffix", "context", "invalid-count-mode"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			e := messagesFixture(t, func(_ string, _ messagesFixtureRequest) any { calls++; return messagesBuffered(500, `{}`) })
			call := messagesCall(e, scenario == "disabled-stream")
			s := &messagesSurface{e}
			switch scenario {
			case "disabled", "disabled-stream":
				e.cfg.Messages.Mode = "disabled"
			case "invalid":
				call.Req.Payload = []byte(`{"messages":[],"max_tokens":2048,"stop_sequences":["END"]}`)
			case "oversized":
				e.cfg.Messages.MaxRequestBytes = 1
			case "suffix":
				call.ThinkingLevel = "high"
			case "context":
				e.cfg.Messages.ContextLimit = 2048
			case "invalid-count-mode":
				e.cfg.Messages.CountTokens = "other"
			}
			var err error
			if scenario == "local-count" {
				var response pluginapi.ExecutorResponse
				response, err = s.count(context.Background(), call)
				if response.Headers.Get("X-CPA-Token-Count") != "approximate-local" || gjson.GetBytes(response.Payload, "input_tokens").Int() <= 0 {
					t.Fatalf("local count: %s %v", response.Payload, response.Headers)
				}
			} else if scenario == "disabled-stream" {
				_, err = s.open(context.Background(), call)
			} else {
				_, err = s.execute(context.Background(), call)
			}
			if (err == nil) != (scenario == "local-count") {
				t.Fatalf("scenario=%s error=%v", scenario, err)
			}
			if calls != 0 {
				t.Fatalf("validation/count reached upstream: calls=%d", calls)
			}
		})
	}
}

func TestMessagesNonStreamHeaderIsolationAndPayloadBarrier(t *testing.T) {
	e := messagesFixture(t, func(method string, req messagesFixtureRequest) any {
		if method != pluginabi.MethodHostHTTPDo || req.URL != "https://example.invalid/v1/messages" {
			t.Errorf("method=%s URL=%s", method, req.URL)
		}
		if req.Headers.Get("Authorization") != "Bearer test-only" || req.Headers.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("headers=%v", req.Headers)
		}
		for _, header := range []string{"X-Api-Key", "X-Client-Id", "X-Tbh-Session-Id", "X-Meta-Ai-Gateway-Session-Id", "Traceparent", "Cookie"} {
			if req.Headers.Get(header) != "" {
				t.Errorf("leaked %s", header)
			}
		}
		if gjson.GetBytes(req.Body, "stream").Bool() || gjson.GetBytes(req.Body, "max_tokens").Int() != 77 || gjson.GetBytes(req.Body, "model").String() != "muse-test" {
			t.Errorf("body=%s", req.Body)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"secret=drop"}, "X-Request-Id": {"upstream-id"}}, Body: []byte(messagesTestResponse)}
	})
	call := messagesCall(e, false)
	call.Req.StorageJSON = []byte(`{"type":"muse","api_key":"test-only","base_url":"https://example.invalid/v1","headers":{"Authorization":"Bearer wrong","X-Api-Key":"secret","User-Agent":"muse-build"}}`)
	call.Req.Payload = []byte(strings.Replace(string(call.Req.Payload), "2048", "77", 1))
	call.Req.Headers = http.Header{"Cookie": {"secret"}, "X-Api-Key": {"downstream-secret"}, "Traceparent": {"trace"}}
	response, err := (&messagesSurface{e}).execute(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(response.Payload, "model").String() != "client-alias" || response.Headers.Get("Set-Cookie") != "" || response.Headers.Get("X-Request-Id") != "upstream-id" || response.Headers.Get("X-CPA-Metering") != "complete" {
		t.Fatalf("payload=%s headers=%v", response.Payload, response.Headers)
	}
	call.Req.Payload = []byte(`{"model":"muse-test","messages":[{"role":"user","content":"hello"}],"max_tokens":77,"stop_sequences":["END"]}`)
	_, err = (&messagesSurface{e}).execute(context.Background(), call)
	messagesStatus(t, err, 400, "unsupported")
}

func TestMessagesSSEValidationAndTruncation(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		wantError  bool
	}{
		{"complete", messagesTestSSE, false},
		{"truncated", strings.Split(messagesTestSSE, "event: message_delta")[0], true},
		{"unknown block", strings.Replace(messagesTestSSE, `"type":"text","text":""`, `"type":"future_block","text":""`, 1), true},
		{"bad index", strings.Replace(messagesTestSSE, `"index":0`, `"index":0.5`, 1), true},
		{"upstream error", strings.Split(messagesTestSSE, "event: content_block_start")[0] + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"retry later\"}}\n\n", true},
		{"duplicate start", strings.Split(messagesTestSSE, "event: content_block_start")[0] + messagesTestSSE, true},
		{"data only", strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(messagesTestSSE, "event: message_start\n", ""), "event: content_block_start\n", ""), "event: content_block_delta\n", ""), "event: content_block_stop\n", ""), "event: message_delta\n", ""), "event: message_stop\n", ""), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := messagesFixture(t, func(_ string, _ messagesFixtureRequest) any { return messagesStreaming(200, tt.body) })
			stream, err := (&messagesSurface{e}).open(context.Background(), messagesCall(e, true))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.close()
			var output bytes.Buffer
			err = stream.run(func(chunk []byte) error {
				if !bytes.HasSuffix(chunk, []byte("\n\n")) {
					t.Errorf("incomplete SSE chunk %q", chunk)
				}
				output.Write(chunk)
				return nil
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v wantError=%v", err, tt.wantError)
			}
			if !bytes.Contains(output.Bytes(), []byte(`"model":"client-alias"`)) {
				t.Fatalf("alias not restored: %s", output.Bytes())
			}
			if !tt.wantError && !bytes.Contains(output.Bytes(), []byte("event: message_stop\ndata:")) {
				t.Fatal("missing completion frame")
			}
		})
	}
}

func TestMessagesStreamToolArguments(t *testing.T) {
	for _, tt := range []struct {
		name, args string
		valid      bool
	}{{"empty", "", true}, {"object", `{\"path\":\"README.md\"}`, true}, {"broken", `{\"path\":`, false}, {"duplicate", `{\"x\":1,\"x\":2}`, false}} {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(messagesTestSSE, `"content_block":{"type":"text","text":""}`, `"content_block":{"type":"tool_use","id":"call_abc","name":"read","input":{}}`, 1)
			body = strings.Replace(body, `"delta":{"type":"text_delta","text":"hello"}`, `"delta":{"type":"input_json_delta","partial_json":"`+tt.args+`"}`, 1)
			e := messagesFixture(t, func(_ string, _ messagesFixtureRequest) any { return messagesStreaming(200, body) })
			stream, err := (&messagesSurface{e}).open(context.Background(), messagesCall(e, true))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.close()
			err = stream.run(func([]byte) error { return nil })
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
		})
	}
}

func TestMessagesUpstreamCountFallbackAndValidation(t *testing.T) {
	for _, tt := range []struct {
		status       int
		body, header string
		wantError    bool
	}{{402, `{"error":{"type":"billing_error"}}`, "approximate-local", false}, {200, `{"input_tokens":0}`, "upstream", false}, {200, `{"input_tokens":-1}`, "", true}, {200, `{"input_tokens":0.5}`, "", true}, {200, `{}`, "", true}, {200, `{"input_tokens":1,"input_tokens":2}`, "", true}, {500, `{}`, "", true}} {
		t.Run(fmt.Sprintf("%d/%s", tt.status, tt.body), func(t *testing.T) {
			e := messagesFixture(t, func(_ string, req messagesFixtureRequest) any {
				if !strings.HasSuffix(req.URL, "/messages/count_tokens") {
					t.Errorf("URL=%s", req.URL)
				}
				return messagesBuffered(tt.status, tt.body)
			})
			e.cfg.Messages.CountTokens = "upstream-then-local"
			ticket, _ := metaMessagesCircuit.Acquire("selected")
			ticket.Complete(MetaMessagesDenied)
			response, err := (&messagesSurface{e}).count(context.Background(), messagesCall(e, false))
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v wantError=%v", err, tt.wantError)
			}
			if !tt.wantError && response.Headers.Get("X-CPA-Token-Count") != tt.header {
				t.Fatalf("headers=%v", response.Headers)
			}
			if ticket, _ := metaMessagesCircuit.Acquire("selected"); ticket != nil {
				t.Fatal("count changed generation circuit")
			}
		})
	}
}

func TestMessagesSubscriptionOptInHonorsSelectedCredential(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprint(ordinary), func(t *testing.T) {
			calls := 0
			e := messagesFixture(t, func(_ string, req messagesFixtureRequest) any {
				calls++
				if req.Headers.Get("Authorization") != "Bearer ordinary" {
					t.Error("subscription selected")
				}
				return messagesBuffered(200, messagesTestResponse)
			})
			e.cfg.Messages.AllowSubscription = false
			call := messagesCall(e, false)
			if ordinary {
				call.Req.StorageJSON = []byte(`{"type":"muse","api_key":"ordinary","base_url":"https://example.invalid/v1"}`)
			} else {
				call.Req.StorageJSON = []byte(`{"type":"muse","api_key":"subscription","dca_token":"dca:must-not-mint","base_url":"https://example.invalid/v1"}`)
			}
			_, err := (&messagesSurface{e}).execute(context.Background(), call)
			if ordinary {
				if err != nil || calls != 1 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
			} else {
				se := messagesStatus(t, err, 403, "subscription_opt_in")
				if se.Scope != ScopeRequest {
					t.Fatal("opt-out poisoned account")
				}
			}
			if !ordinary && calls != 0 {
				t.Fatal("opted-out subscription reached upstream")
			}
		})
	}
}

func TestMessages403OpensSurfaceCircuitWithoutBlockingResponses(t *testing.T) {
	calls := 0
	e := messagesFixture(t, func(_ string, req messagesFixtureRequest) any {
		if strings.HasSuffix(req.URL, "/responses") {
			return messagesBuffered(200, coreCompleted)
		}
		calls++
		return messagesBuffered(403, `{"type":"error","error":{"type":"permission_error","message":"Messages unavailable"}}`)
	})
	s := &messagesSurface{e}
	call := messagesCall(e, false)
	_, err := s.execute(context.Background(), call)
	se := messagesStatus(t, err, 403, "")
	if se.Scope != ScopeRequest {
		t.Fatal("surface failure poisoned shared account")
	}
	_, err = s.execute(context.Background(), call)
	messagesStatus(t, err, 503, "surface_unavailable")
	if calls != 1 {
		t.Fatal("open circuit reached upstream")
	}
	req := selectedRequest(pluginapi.ExecutorRequest{Model: "muse-spark-1.3", Format: FormatCodex, Payload: []byte(`{"input":[]}`)}, "messages-test")
	if _, err := e.Execute(context.Background(), req, "messages-test"); err != nil {
		t.Fatalf("Messages denial blocked Responses with same credential: %v", err)
	}
}

func TestMessagesErrorScopeAndRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		status  int
		scope   Scope
		outcome MetaMessagesOutcome
	}{{400, ScopeRequest, MetaMessagesNeutral}, {401, ScopeCredential, MetaMessagesNeutral}, {402, ScopeRequest, MetaMessagesDenied}, {403, ScopeRequest, MetaMessagesDenied}, {404, ScopeModel, MetaMessagesNeutral}, {429, ScopeModel, MetaMessagesNeutral}, {503, ScopeRequest, MetaMessagesTransient}} {
		err, outcome := messagesClassify(messagesUpstreamError(tt.status, http.Header{"Retry-After": {"45"}}, []byte(`{"error":{"type":"api_error","message":"test"}}`)))
		se := messagesStatus(t, err, tt.status, "")
		if se.Scope != tt.scope || outcome != tt.outcome || se.RetryAfter != 45*time.Second {
			t.Fatalf("status=%d scope=%v outcome=%v retry=%v", tt.status, se.Scope, outcome, se.RetryAfter)
		}
	}
	err, _ := messagesClassify(messagesUpstreamError(429, nil, []byte(`{"error":{"type":"rate_limit_error","message":"subscription quota exhausted"}}`)))
	if err.(*StatusError).Scope != ScopeCredential {
		t.Fatal("quota lost account scope")
	}
}

func TestMessagesBetaGateAndEffortMatrix(t *testing.T) {
	e := messagesFixture(t, func(_ string, _ messagesFixtureRequest) any { return messagesBuffered(200, messagesTestResponse) })
	call := messagesCall(e, false)
	call.Req.Headers = http.Header{"anthropic-beta": {"future-beta"}}
	_, err := e.prepareMessagesRequest(context.Background(), call, false)
	messagesStatus(t, err, 400, "capability_unverified")
	e.cfg.Messages.Capabilities = map[string][]string{"muse-test": {"beta.future-beta"}}
	if _, err = e.prepareMessagesRequest(context.Background(), call, false); err != nil {
		t.Fatal(err)
	}
	call.Req.Headers = nil
	for _, model := range []string{"muse-wonder-full-1.0", "muse-wonder-1.0", "muse-spark-1.3", "muse-spark-1.2", "muse-spark-1.3-contributor"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "minimal"} {
			call.Model = model
			e.cfg.Messages.Capabilities[model] = []string{"effort.max"}
			call.Req.Payload = []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"test"}],"max_tokens":2048,"output_config":{"effort":%q}}`, model, effort))
			_, err := e.prepareMessagesRequest(context.Background(), call, false)
			allowed := effort != "minimal" && !(model == "muse-spark-1.2" && effort == "max")
			if (err == nil) != allowed {
				t.Fatalf("%s/%s allowed=%v error=%v", model, effort, allowed, err)
			}
		}
	}
}

func TestMessagesURLImageTimeoutCancelsHostAndPreservesCredential(t *testing.T) {
	for _, scenario := range []string{"open", "json-body", "sse-body"} {
		t.Run(scenario, func(t *testing.T) {
			e := messagesFixture(t, func(_ string, _ messagesFixtureRequest) any { return nil })
			seconds := 1
			e.cfg.Messages.URLImageIdleTimeoutSeconds = &seconds
			canceled := make(chan struct{})
			var cancelOnce sync.Once
			var calls, mints atomic.Int32
			host.SetTransport(func(method string, raw []byte) ([]byte, error) {
				result := any(map[string]any{})
				switch method {
				case pluginabi.MethodHostHTTPOperationOpen:
					result = map[string]any{"operation_id": "watchdog-operation"}
				case pluginabi.MethodHostHTTPCancel, pluginabi.MethodHostHTTPStreamClose:
					cancelOnce.Do(func() { close(canceled) })
				case pluginabi.MethodHostHTTPDo:
					mints.Add(1)
					return nil, errors.New("unexpected mint")
				case pluginabi.MethodHostHTTPDoStream:
					if calls.Add(1) == 1 {
						if scenario == "open" {
							<-canceled
							return nil, errors.New("upstream canceled before headers")
						}
						headers := http.Header{"Content-Type": {"application/json"}}
						chunks := []map[string]any{}
						if scenario == "sse-body" {
							headers.Set("Content-Type", "text/event-stream")
							chunks = append(chunks, map[string]any{"Payload": []byte(strings.Split(messagesTestSSE, "event: content_block_start")[0])})
						}
						result = map[string]any{"status_code": 200, "headers": headers, "stream_id": "watchdog-stream", "chunks": chunks}
					} else if scenario == "sse-body" {
						result = messagesStreaming(200, messagesTestSSE)
					} else {
						result = map[string]any{"status_code": 200, "headers": http.Header{"Content-Type": {"application/json"}}, "chunks": []map[string]any{{"Payload": []byte(messagesTestResponse)}}}
					}
				case pluginabi.MethodHostHTTPStreamRead:
					<-canceled
					result = map[string]any{"error": "upstream canceled", "done": true}
				}
				return json.Marshal(map[string]any{"ok": true, "result": result})
			})
			call := messagesCall(e, scenario == "sse-body")
			call.Req.Payload = []byte(`{"model":"muse-test","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/fixture.png"}}]}]}`)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			s := &messagesSurface{e}
			invoke := func() error {
				if !call.Stream {
					_, err := s.execute(ctx, call)
					return err
				}
				stream, err := s.open(ctx, call)
				if err != nil {
					return err
				}
				defer stream.close()
				return stream.run(func([]byte) error { return nil })
			}
			err := invoke()
			if !IsMetaMessagesURLImageTimeout(err) {
				t.Fatalf("expected URL-image timeout, got %v", err)
			}
			se := messagesStatus(t, err, 504, "url_image_idle_timeout")
			if se.Scope != ScopeRequest {
				t.Fatal("watchdog cooled shared credential")
			}
			select {
			case <-canceled:
			default:
				t.Fatal("watchdog did not cancel upstream host operation")
			}
			if calls.Load() != 1 || mints.Load() != 0 {
				t.Fatalf("timeout retried/minted: calls=%d mints=%d", calls.Load(), mints.Load())
			}
			if err = invoke(); err != nil {
				t.Fatalf("timeout prevented subsequent selected-credential request: %v", err)
			}
			if calls.Load() != 2 || mints.Load() != 0 || gjson.GetBytes(call.Req.StorageJSON, "api_key").String() != "test-only" {
				t.Fatal("watchdog changed credentials or prevented reuse")
			}
		})
	}
}
