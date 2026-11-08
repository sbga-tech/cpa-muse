package muse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const coreCompleted = `{"type":"response.completed","response":{"id":"resp_test","object":"response","status":"completed","output":[]}}`

func TestCoreExecute401ReturnsToHostWithoutRefreshing(t *testing.T) {
	f := coreHost(t)
	f.handle("host.http.do", func(raw json.RawMessage) (any, error) {
		var req coreHTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.CallbackID != "cb-1" || req.URL != "https://example.invalid/v1/responses" || req.Headers.Get("Authorization") != "Bearer test-only" {
			t.Errorf("selected credential request: %+v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":{"message":"expired"}}`)}, nil
	})
	e := NewExecutor(coreConfig(), nil)
	req := selectedRequest(pluginapi.ExecutorRequest{Model: "muse-spark-1.3", Format: FormatCodex, Payload: []byte(`{"input":[]}`)}, "cb-1")
	_, err := e.Execute(context.Background(), req, "cb-1")
	coreStatus(t, err, 401, "credential_rejected")
	if len(f.calls("host.http.do")) != 1 {
		t.Fatal("execute retried or minted after credential rejection")
	}
	for _, method := range []string{"host.auth.list", "host.auth.get", "host.auth.save"} {
		if len(f.calls(method)) != 0 {
			t.Errorf("execute invoked host lifecycle callback %s", method)
		}
	}
}

func TestCoreRetryLoopTransientBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
		calls  int
		code   string
	}{
		{"gateway", 502, `{"error":{"message":"bad gateway"}}`, nil, 2, "upstream_error"},
		{"missing-model", 404, `{"error":{"code":"model_not_found"}}`, nil, 3, "model_not_found"},
		{"transport", 0, "", errors.New("connection reset"), 2, "upstream_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coreHost(t)
			e := NewExecutor(coreConfig(), nil)
			call := e.newCall(selectedRequest(pluginapi.ExecutorRequest{Model: "model-a"}, ""), "", false)
			credential := &Credential{ID: "selected", APIKey: "test-only", BaseURL: "https://example.invalid/v1"}
			s := &responsesSurface{exec: e}
			calls, discarded := 0, 0
			err := s.retryLoop(context.Background(), call, credential, func(token string) (int, []byte, func(), error) {
				if token != "test-only" {
					t.Errorf("unexpected token %q", token)
				}
				calls++
				return tc.status, []byte(tc.body), func() { discarded++ }, tc.err
			})
			status := tc.status
			if status == 0 {
				status = 502
			}
			se := coreStatus(t, err, status, tc.code)
			if calls != tc.calls || (tc.err == nil && discarded != tc.calls) {
				t.Fatalf("retry budget calls=%d discards=%d", calls, discarded)
			}
			if tc.status == http.StatusNotFound && (se.RetryAfter != 30*time.Second || se.Scope != ScopeModel) {
				t.Fatalf("missing model retry metadata: %+v", se)
			}
		})
	}
}
