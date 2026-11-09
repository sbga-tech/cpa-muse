package muse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/sbga-tech/cpa-muse/internal/host"
)

// coreFakeHost scripts host callbacks rather than bypassing the plugin HTTP ABI.
// Handlers run unlocked so a blocked stream read cannot prevent cancellation.
type coreFakeHost struct {
	mu       sync.Mutex
	handlers map[string]func(json.RawMessage) (any, error)
	requests map[string][]json.RawMessage
}

func coreHost(t *testing.T) *coreFakeHost {
	t.Helper()
	f := &coreFakeHost{handlers: make(map[string]func(json.RawMessage) (any, error)), requests: make(map[string][]json.RawMessage)}
	host.SetTransport(func(method string, raw []byte) ([]byte, error) {
		f.mu.Lock()
		f.requests[method] = append(f.requests[method], append(json.RawMessage(nil), raw...))
		h := f.handlers[method]
		f.mu.Unlock()
		var result any = struct{}{}
		if h != nil {
			var err error
			result, err = h(raw)
			if err != nil {
				return nil, err
			}
		} else if method != "host.log" && method != "host.http.stream_close" {
			return nil, fmt.Errorf("unexpected host callback %s", method)
		}
		return json.Marshal(map[string]any{"ok": true, "result": result})
	})
	t.Cleanup(func() { host.SetTransport(nil) })
	return f
}

func (f *coreFakeHost) handle(method string, handler func(json.RawMessage) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = handler
}

func (f *coreFakeHost) calls(method string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]json.RawMessage(nil), f.requests[method]...)
}

type coreHTTPRequest struct {
	CallbackID string      `json:"host_callback_id"`
	URL        string      `json:"url"`
	Method     string      `json:"method"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}

func coreConfig() *Config {
	return &Config{}
}

func selectedRequest(req pluginapi.ExecutorRequest, callbackID string) pluginapi.ExecutorRequest {
	req.AuthID = "selected"
	req.AuthProvider = "muse"
	req.StorageJSON = []byte(`{"type":"muse","api_key":"test-only","base_url":"https://example.invalid/v1"}`)
	req.HTTPClient = host.HTTPClient(callbackID)
	return req
}
