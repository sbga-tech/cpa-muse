package muse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/sbga-tech/cpa-muse/internal/host"
	"github.com/tidwall/gjson"
)

type authProviderClient struct {
	do func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (c authProviderClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return c.do(ctx, req)
}
func (c authProviderClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("unexpected stream request")
}

func TestAuthProviderParsesOnlyMuse(t *testing.T) {
	for _, tokens := range []string{`"api_key":"api-key"`, `"access_token":"access-key"`, `"dca_token":"dca:refresh"`, `"access_token":"dca:refresh"`} {
		t.Run(tokens, func(t *testing.T) {
			raw := []byte(`{"type":"muse",` + tokens + `,"base_url":"https://selected.invalid/v1","headers":{"X-Account":"selected"},"priority":7,"prefix":"mine","proxy_url":"http://proxy.invalid","disabled":true,"attributes":{"base_url":"must-not-select-openai-compat"}}`)
			resp, err := (AuthProvider{}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "selected.json"})
			if err != nil || !resp.Handled {
				t.Fatalf("parse: handled=%v err=%v", resp.Handled, err)
			}
			auth := resp.Auth
			if auth.Provider != "muse" || auth.FileName != "selected.json" || auth.Prefix != "mine" || !auth.Disabled || auth.ProxyURL != "http://proxy.invalid" {
				t.Fatalf("auth routing: %+v", auth)
			}
			if auth.Attributes["priority"] != "7" || auth.Attributes["base_url"] != "" {
				t.Fatalf("routing attributes: %v", auth.Attributes)
			}
			cred := materializeCredential(auth.StorageJSON, auth.Metadata, auth.Attributes)
			if cred.BaseURL != "https://selected.invalid/v1" || cred.Headers["X-Account"] != "selected" {
				t.Fatalf("wire credential: %+v", cred)
			}
			if cred.DCAToken != "" && auth.Metadata["refresh_token"] != cred.DCAToken {
				t.Fatal("DCA token is not exposed to the host refresh gate")
			}
			if cred.APIKey != "" && auth.Metadata["access_token"] != cred.APIKey {
				t.Fatal("API key is not exposed to host token rotation checks")
			}
		})
	}
	// Native meta files stay with CPA's meta executor; migration is explicit.
	for _, raw := range []string{`{"type":"meta","api_key":"native"}`, `{"type":"other","api_key":"unrelated"}`} {
		resp, err := (AuthProvider{}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(raw)})
		if err != nil || resp.Handled {
			t.Fatalf("claimed %s: %+v %v", raw, resp, err)
		}
	}
}

func TestSelectedCredentialMaterializesOnlySelectedInputs(t *testing.T) {
	client := authProviderClient{}
	req := pluginapi.ExecutorRequest{
		AuthID: "chosen", AuthProvider: "muse", HTTPClient: client,
		StorageJSON:    []byte(`{"api_key":"storage-key","dca_token":"dca:storage","base_url":"https://storage.invalid/v1","headers":{"X-Storage":"yes","X-Overlay":"storage"}}`),
		AuthMetadata:   map[string]any{"api_key": "metadata-key", "headers": map[string]any{"X-Metadata": "yes", "X-Overlay": "metadata"}},
		AuthAttributes: map[string]string{"api_key": "selected-key", "base_url": "https://selected.invalid/v1", "header:X-Attribute": "yes", "headers": `{"X-Overlay":"attribute"}`},
	}
	cred, err := selectedCredential(req)
	if err != nil {
		t.Fatal(err)
	}
	if cred.ID != "chosen" || cred.APIKey != "selected-key" || cred.DCAToken != "dca:storage" || cred.BaseURL != "https://selected.invalid/v1" {
		t.Fatalf("selected materialization: %+v", cred)
	}
	for key, want := range map[string]string{"X-Storage": "yes", "X-Metadata": "yes", "X-Attribute": "yes", "X-Overlay": "attribute"} {
		if cred.Headers[key] != want {
			t.Fatalf("header %s=%q, want %q", key, cred.Headers[key], want)
		}
	}
}

func TestSelectedCredentialFailsClosedWithoutDiscovery(t *testing.T) {
	host.SetTransport(func(method string, request []byte) ([]byte, error) {
		t.Fatalf("unexpected host callback %s", method)
		return nil, nil
	})
	t.Cleanup(func() { host.SetTransport(nil) })
	valid := pluginapi.ExecutorRequest{AuthID: "chosen", AuthProvider: "muse", StorageJSON: []byte(`{"api_key":"key"}`), HTTPClient: authProviderClient{}}
	for _, mutate := range []func(*pluginapi.ExecutorRequest){
		func(r *pluginapi.ExecutorRequest) { r.AuthID = "" },
		func(r *pluginapi.ExecutorRequest) { r.AuthProvider = "meta" },
		func(r *pluginapi.ExecutorRequest) { r.HTTPClient = nil },
		func(r *pluginapi.ExecutorRequest) { r.StorageJSON = nil },
		func(r *pluginapi.ExecutorRequest) { r.StorageJSON = []byte(`{"dca_token":"dca:refresh"}`) },
		func(r *pluginapi.ExecutorRequest) {
			r.StorageJSON = []byte(`{"api_key":"key","proxy_url":"socks5://proxy.invalid:1080"}`)
		},
		func(r *pluginapi.ExecutorRequest) {
			r.AuthMetadata = map[string]any{"proxy_url": "http://proxy.invalid:8080"}
		},
	} {
		req := valid
		mutate(&req)
		if _, err := selectedCredential(req); err == nil {
			t.Fatal("accepted missing selected credential/client")
		}
	}
}

func TestAuthProviderRefreshUsesSuppliedClientAndReturnsStorage(t *testing.T) {
	calls := 0
	client := authProviderClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if req.URL != "https://mint.invalid/key" || req.Method != http.MethodPost || req.Headers.Get("Authorization") != "Bearer dca:chosen" || req.Headers.Get("User-Agent") != authFlowUserAgent || string(req.Body) != `{"onboard":false}` {
			t.Fatalf("mint request: %+v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"api_key":"new-key","base_url":"https://new.invalid/v1","user_email":"chosen@example.invalid","subs_tier_name":"pro","is_subs_active":true}`)}, nil
	}}
	storage := []byte(`{"type":"meta","api_key":"old-key","dca_token":"dca:chosen","headers":{"X-Selected":"yes"},"priority":11,"prefix":"mine","expired":"old"}`)
	metadata := map[string]any{"api_key": "old-key", "access_token": "old-key", "custom": "keep"}
	resp, err := (AuthProvider{MintURL: "https://mint.invalid/key"}).RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "chosen", AuthProvider: "muse", StorageJSON: storage, Metadata: metadata, Attributes: map[string]string{"priority": "11"}, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || resp.Auth.Provider != "muse" || resp.Auth.ID != "chosen" {
		t.Fatalf("refresh result: %+v calls=%d", resp, calls)
	}
	if resp.Auth.Metadata["api_key"] != "new-key" || resp.Auth.Metadata["access_token"] != "new-key" || resp.Auth.Metadata["refresh_token"] != "dca:chosen" || resp.Auth.Metadata["custom"] != "keep" {
		t.Fatalf("refresh metadata: %v", resp.Auth.Metadata)
	}
	if gjson.GetBytes(resp.Auth.StorageJSON, "expired").Exists() || gjson.GetBytes(resp.Auth.StorageJSON, "headers.X-Selected").String() != "yes" || gjson.GetBytes(resp.Auth.StorageJSON, "type").String() != "muse" {
		t.Fatalf("refresh storage: %s", resp.Auth.StorageJSON)
	}
	if metadata["api_key"] != "old-key" || !strings.Contains(string(storage), "old-key") {
		t.Fatal("refresh mutated caller state")
	}
	var data map[string]any
	if err := json.Unmarshal(resp.Auth.StorageJSON, &data); err != nil {
		t.Fatal(err)
	}
	if data["base_url"] != "https://new.invalid/v1" || resp.Auth.Attributes["priority"] != "11" {
		t.Fatalf("refresh lost provider routing: %v %v", data, resp.Auth.Attributes)
	}
}

func TestAuthProviderRefreshFailsClosed(t *testing.T) {
	mustNotMint := authProviderClient{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("refresh minted for an unusable credential")
		return pluginapi.HTTPResponse{}, nil
	}}
	for _, req := range []pluginapi.AuthRefreshRequest{
		{AuthID: "chosen", AuthProvider: "muse", StorageJSON: []byte(`{"dca_token":"dca:chosen"}`)},
		{AuthID: "chosen", AuthProvider: "muse", HTTPClient: mustNotMint, StorageJSON: []byte(`{"api_key":"key"}`)},
		{AuthID: "chosen", AuthProvider: "other", HTTPClient: mustNotMint, StorageJSON: []byte(`{"dca_token":"dca:chosen"}`)},
		{AuthID: "chosen", AuthProvider: "muse", HTTPClient: mustNotMint, StorageJSON: []byte(`{"dca_token":"dca:chosen","proxy_url":"http://proxy.invalid:8080"}`)},
		{AuthID: "chosen", AuthProvider: "muse", HTTPClient: mustNotMint, StorageJSON: []byte(`{"dca_token":"dca:chosen"}`), Metadata: map[string]any{"proxy_url": "socks5://proxy.invalid:1080"}},
	} {
		if _, err := (AuthProvider{}).RefreshAuth(context.Background(), req); err == nil {
			t.Fatal("accepted invalid refresh context")
		}
	}
}

func TestExecutionUsesSuppliedSelectedHTTPClient(t *testing.T) {
	for _, tc := range []struct{ name, config, format, body, path string }{
		{"legacy", "", FormatCodex, `{"input":"hello"}`, "/responses"},
		{"native-generic", "meta-responses:\n  mode: native\n  header-profile: generic\n", FormatOpenAIResponse, `{"input":"hello"}`, "/responses"},
		{"native-muse", "meta-responses:\n  mode: native\n  header-profile: muse\n", FormatOpenAIResponse, `{"input":"hello"}`, "/responses"},
		{"profile", "meta-responses:\n  mode: native\nmuse-profile:\n  enabled: true\n  profile: profiles/muse-linux-exec-1.4.7.json\n", FormatOpenAIResponse, `{"input":"hello"}`, "/responses"},
		{"messages", "meta-messages:\n  mode: enabled\n", FormatClaude, `{"messages":[{"role":"user","content":"hello"}],"max_tokens":16}`, "/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.config))
			if err != nil {
				t.Fatal(err)
			}
			host.SetTransport(func(method string, _ []byte) ([]byte, error) {
				if strings.HasPrefix(method, "host.auth.") || strings.HasPrefix(method, "host.http.") {
					t.Fatalf("execution bypassed supplied client: %s", method)
				}
				return nil, errors.New("logging unavailable in selected-client test")
			})
			t.Cleanup(func() { host.SetTransport(nil) })
			calls := 0
			client := authProviderClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				calls++
				if req.URL != "https://chosen.invalid/v1"+tc.path || req.Headers.Get("Authorization") != "Bearer chosen-key" {
					t.Fatalf("wrong selected wire auth: %+v", req)
				}
				wantHeader := "chosen"
				if tc.name == "native-generic" || tc.name == "messages" {
					wantHeader = ""
				}
				if req.Headers.Get("X-Selected") != wantHeader {
					t.Fatalf("selected headers crossed protocol boundary: %v", req.Headers)
				}
				body := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_selected\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
				if tc.format == FormatClaude {
					body = []byte(`{"type":"message","id":"msg_selected","model":"muse-spark-1.3","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}`)
				}
				return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
			}}
			req := pluginapi.ExecutorRequest{AuthID: "selected-client-" + tc.name, AuthProvider: "muse", HTTPClient: client, Model: "muse-spark-1.3", Format: tc.format, SourceFormat: tc.format, Payload: []byte(tc.body), StorageJSON: []byte(`{"api_key":"chosen-key","base_url":"https://chosen.invalid/v1","headers":{"X-Selected":"chosen"}}`)}
			if _, err := NewExecutor(cfg, nil).Execute(context.Background(), req, "must-not-select-client"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("selected upstream calls=%d", calls)
			}
		})
	}
}
