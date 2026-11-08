package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/DDD0s/cpa-muse/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func pluginResult[T any](t *testing.T, method string, request any) T {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(Dispatch(method, raw), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("%s failed: %+v", method, envelope.Error)
	}
	var result T
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPluginRegisterFormatsAndSchemaNegotiation(t *testing.T) {
	t.Cleanup(Shutdown)
	for _, tc := range []struct {
		name, yaml string
		formats    []string
	}{
		{"default", "", []string{"codex", "openai-response"}},
		{"messages-enabled", "meta-messages:\n  mode: enabled\n", []string{"codex", "openai-response", "claude"}},
		{"messages-disabled", "meta-messages:\n  mode: disabled\n", []string{"codex", "openai-response"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, hostVersion := range []uint32{1, pluginabi.SchemaVersion, pluginabi.SchemaVersion + 1} {
				got := pluginResult[registration](t, "plugin.register", lifecycleRequest{ConfigYAML: []byte(tc.yaml), SchemaVersion: hostVersion})
				if got.SchemaVersion == 0 || got.SchemaVersion > hostVersion || got.SchemaVersion > pluginabi.SchemaVersion {
					t.Fatalf("plugin schema %d exceeds host=%d/plugin=%d", got.SchemaVersion, hostVersion, pluginabi.SchemaVersion)
				}
				if !reflect.DeepEqual(got.Capabilities.ExecutorInputFormats, tc.formats) || !got.Capabilities.Executor || !got.Capabilities.ModelRouter || !got.Capabilities.AuthProvider {
					t.Fatalf("wrong registration capabilities: %+v", got.Capabilities)
				}
			}
		})
	}
}

func TestPluginModelRouteTargetsMuseProvider(t *testing.T) {
	t.Cleanup(Shutdown)
	pluginResult[registration](t, "plugin.register", lifecycleRequest{ConfigYAML: []byte("prefixes: [meta]\naliases:\n  smart: muse-spark-1.3\n")})
	for _, tc := range []struct{ requested, resolved string }{
		{"muse-spark-1.3", "muse-spark-1.3"},
		{"meta/muse-spark-1.3", "muse-spark-1.3"},
		{"smart(high)", "muse-spark-1.3(high)"},
		{"meta/smart(high)", "muse-spark-1.3(high)"},
		{" meta/smart(8192) ", "muse-spark-1.3(8192)"},
	} {
		got := pluginResult[pluginapi.ModelRouteResponse](t, "model.route", pluginapi.ModelRouteRequest{RequestedModel: tc.requested})
		if !got.Handled || got.TargetKind != pluginapi.ModelRouteTargetProvider || got.Target != "muse" || got.TargetModel != tc.resolved {
			t.Errorf("%q must select muse auth for %q: %+v", tc.requested, tc.resolved, got)
		}
	}
	got := pluginResult[pluginapi.ModelRouteResponse](t, "model.route", pluginapi.ModelRouteRequest{RequestedModel: "unknown-model"})
	if got.Handled || got.TargetKind != "" {
		t.Fatalf("unknown model claimed: %+v", got)
	}
}

func TestPluginAuthLifecycleDispatch(t *testing.T) {
	t.Cleanup(Shutdown)
	t.Cleanup(func() { host.SetTransport(nil) })
	pluginResult[registration](t, "plugin.register", lifecycleRequest{ConfigYAML: []byte("mint-url: https://example.invalid/mint\n")})
	if got := pluginResult[identifierResponse](t, "auth.identifier", struct{}{}); got.Identifier != "muse" {
		t.Fatalf("auth provider identifier: %+v", got)
	}
	parsed := pluginResult[pluginapi.AuthParseResponse](t, "auth.parse", pluginapi.AuthParseRequest{Provider: "muse", FileName: "fixture.json", RawJSON: []byte(`{"type":"muse","api_key":"expired","dca_token":"dca:test-only","keep":"preserved"}`)})
	if !parsed.Handled || parsed.Auth.Provider != "muse" || parsed.Auth.FileName != "fixture.json" {
		t.Fatalf("parsed auth: %+v", parsed)
	}
	calls, opened := 0, 0
	host.SetTransport(func(method string, raw []byte) ([]byte, error) {
		var req struct {
			URL         string      `json:"url"`
			Method      string      `json:"method"`
			Headers     http.Header `json:"headers"`
			CallbackID  string      `json:"host_callback_id"`
			OperationID string      `json:"operation_id"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		// A request callback id would put the minted key in that request's log.
		if req.CallbackID != "" {
			t.Errorf("%s bound to downstream request callback %q", method, req.CallbackID)
		}
		var result any = struct{}{}
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			opened++
			result = map[string]string{"operation_id": "mint-operation"}
		case pluginabi.MethodHostHTTPCancel:
		case pluginabi.MethodHostHTTPDo:
			calls++
			if req.URL != "https://example.invalid/mint" || req.Method != http.MethodPost || req.Headers.Get("Authorization") != "Bearer dca:test-only" || req.OperationID != "mint-operation" {
				t.Errorf("refresh lost host transport/config/deadline operation: %+v", req)
			}
			result = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"api_key":"fresh-test-only"}`)}
		default:
			return nil, fmt.Errorf("unexpected refresh callback %s", method)
		}
		return json.Marshal(map[string]any{"ok": true, "result": result})
	})
	refreshed := pluginResult[pluginapi.AuthRefreshResponse](t, "auth.refresh", map[string]any{"AuthID": "selected", "AuthProvider": "muse", "StorageJSON": parsed.Auth.StorageJSON, "host_callback_id": "request-callback"})
	var storage map[string]any
	if err := json.Unmarshal(refreshed.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || opened != 1 || refreshed.Auth.Provider != "muse" || refreshed.Auth.ID != "selected" || storage["api_key"] != "fresh-test-only" || storage["dca_token"] != "dca:test-only" || storage["keep"] != "preserved" {
		t.Fatalf("refresh persistence response: calls=%d auth=%+v", calls, refreshed.Auth)
	}
}

// Login must stay bound to CPA's login callback: that context carries the
// operator's session proxy, unlike refresh, which deliberately mints unscoped.
func TestPluginLoginStartUsesLoginCallbackAndConfiguredAuthHost(t *testing.T) {
	t.Cleanup(Shutdown)
	t.Cleanup(func() { host.SetTransport(nil) })
	pluginResult[registration](t, "plugin.register", lifecycleRequest{ConfigYAML: []byte("auth-url: https://auth.invalid\n")})
	var devices int
	host.SetTransport(func(method string, raw []byte) ([]byte, error) {
		var req struct {
			URL        string `json:"url"`
			CallbackID string `json:"host_callback_id"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.CallbackID != "login-callback" {
			t.Errorf("%s left the login callback scope: %q", method, req.CallbackID)
		}
		var result any = struct{}{}
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			result = map[string]string{"operation_id": "login-operation"}
		case pluginabi.MethodHostHTTPCancel:
		case pluginabi.MethodHostHTTPDo:
			devices++
			if req.URL != "https://auth.invalid/oidc/device/authorization/" {
				t.Errorf("device authorization URL: %s", req.URL)
			}
			result = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"device_code":"device-secret","user_code":"ABCD-EFGH","verification_uri_complete":"https://auth.invalid/oauth/device/?code=ABCD-EFGH","expires_in":600,"interval":5}`)}
		default:
			return nil, fmt.Errorf("unexpected login callback %s", method)
		}
		return json.Marshal(map[string]any{"ok": true, "result": result})
	})
	started := pluginResult[pluginapi.AuthLoginStartResponse](t, pluginabi.MethodAuthLoginStart, map[string]any{"Provider": "muse", "host_callback_id": "login-callback"})
	if devices != 1 || started.URL != "https://auth.invalid/oauth/device/?code=ABCD-EFGH" || started.State == "" {
		t.Fatalf("login start: devices=%d response=%+v", devices, started)
	}
}
