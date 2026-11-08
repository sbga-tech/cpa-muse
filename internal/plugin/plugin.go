// Package plugin implements the CPA plugin RPC surface for the Muse provider.
//
// CPA selects and manages Muse credentials. The plugin translates Muse wire
// protocols, executes the selected credential, and refreshes it when CPA asks.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/DDD0s/cpa-muse/internal/host"
	"github.com/DDD0s/cpa-muse/internal/muse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Version is stamped at build time with -ldflags "-X .../internal/plugin.Version=...".
var Version = "dev"

// ExecutorIdentifier is the provider key of the plugin executor. It differs
// from the built-in "meta" provider on purpose: the host never registers a
// plugin executor over a native one.
const ExecutorIdentifier = "muse"

var state = struct {
	sync.RWMutex
	exec *muse.Executor
}{}

func current() (*muse.Executor, error) {
	state.RLock()
	defer state.RUnlock()
	if state.exec == nil {
		return nil, fmt.Errorf("muse plugin is not registered")
	}
	return state.exec, nil
}

// Shutdown drops the active configuration.
func Shutdown() {
	state.Lock()
	state.exec = nil
	state.Unlock()
}

// Dispatch handles one host RPC and returns the response envelope.
func Dispatch(method string, request []byte) []byte {
	result, err := dispatch(method, request)
	if err != nil {
		return encodeError(err)
	}
	return result
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	ModelRouter           bool                         `json:"model_router"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	QuotaProvider         bool                         `json:"quota_provider"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type modelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type executorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type quotaResetRequest struct {
	pluginapi.QuotaResetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return register(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return ok(struct{}{})
	}
	exec, err := current()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	switch method {
	case pluginabi.MethodExecutorIdentifier:
		return ok(identifierResponse{Identifier: ExecutorIdentifier})
	case pluginabi.MethodAuthIdentifier:
		return ok(identifierResponse{Identifier: ExecutorIdentifier})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode auth parse request: %w", err)
		}
		resp, err := (muse.AuthProvider{}).ParseAuth(ctx, req)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode auth refresh request: %w", err)
		}
		// Mint outside the triggering request's callback scope. Stock CPA records
		// callback-scoped HTTP response bodies in that request's log, and the mint
		// response carries the new API key. CPA still owns when refresh runs, the
		// per-auth refresh lock, persistence, and replay.
		req.HTTPClient = host.HTTPClient("")
		resp, err := exec.Config().AuthProvider().RefreshAuth(ctx, req)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodAuthLoginStart:
		var req authLoginStartRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode auth login start request: %w", err)
		}
		// Login stays callback-scoped: CPA runs it in a management context that
		// carries the login session's proxy and no downstream request log.
		req.HTTPClient = host.HTTPClient(req.HostCallbackID)
		resp, err := exec.Config().AuthProvider().StartLogin(ctx, req.AuthLoginStartRequest)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodAuthLoginPoll:
		var req authLoginPollRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode auth login poll request: %w", err)
		}
		req.HTTPClient = host.HTTPClient(req.HostCallbackID)
		resp, err := exec.Config().AuthProvider().PollLogin(ctx, req.AuthLoginPollRequest)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodQuotaIdentifier:
		return ok(identifierResponse{Identifier: muse.QuotaIdentifier()})
	case pluginabi.MethodModelStatic:
		return ok(pluginapi.ModelResponse{Provider: ExecutorIdentifier, Models: exec.Config().Catalog()})
	case pluginabi.MethodModelRoute:
		var req modelRouteRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode model route request: %w", err)
		}
		return ok(route(exec, req.ModelRouteRequest))
	case pluginabi.MethodExecutorExecute:
		req, err := decodeExecutor(request)
		if err != nil {
			return nil, err
		}
		resp, err := exec.Execute(ctx, req.ExecutorRequest, req.HostCallbackID)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodExecutorExecuteStream:
		req, err := decodeExecutor(request)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(req.StreamID) == "" {
			return nil, fmt.Errorf("host did not provide a stream id")
		}
		headers, err := exec.ExecuteStream(ctx, req.ExecutorRequest, req.HostCallbackID, req.StreamID)
		if err != nil {
			return nil, err
		}
		return ok(executorStreamResponse{Headers: headers})
	case pluginabi.MethodExecutorCountTokens:
		req, err := decodeExecutor(request)
		if err != nil {
			return nil, err
		}
		resp, err := exec.CountTokens(ctx, req.ExecutorRequest, req.HostCallbackID)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodExecutorHTTPRequest:
		return nil, &pluginabi.Error{Code: "unsupported", Message: "raw HTTP passthrough is not supported for Muse; use the standard model endpoints", HTTPStatus: http.StatusBadRequest}
	case pluginabi.MethodQuotaDescribe:
		var req pluginapi.QuotaDescribeRequest
		_ = json.Unmarshal(request, &req)
		resp, err := exec.DescribeQuota(ctx, req)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodQuotaFetch:
		var req quotaFetchRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode quota fetch request: %w", err)
		}
		resp, err := exec.FetchQuota(ctx, req.QuotaFetchRequest)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	case pluginabi.MethodQuotaReset:
		var req quotaResetRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode quota reset request: %w", err)
		}
		resp, err := exec.ResetQuota(ctx, req.QuotaResetRequest)
		if err != nil {
			return nil, err
		}
		return ok(resp)
	default:
		return nil, &pluginabi.Error{Code: "unknown_method", Message: "unknown method: " + method}
	}
}

func decodeExecutor(raw []byte) (executorRequest, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return executorRequest{}, fmt.Errorf("decode executor request: %w", err)
	}
	req.HTTPClient = host.HTTPClient(req.HostCallbackID)
	return req, nil
}

func register(raw []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode plugin lifecycle request: %w", err)
		}
	}
	cfg, err := muse.ParseConfig(req.ConfigYAML)
	if err != nil {
		return nil, err
	}
	state.Lock()
	if previous := state.exec; previous != nil {
		state.exec = muse.NewExecutor(cfg, previous.QuotaStore())
	} else {
		state.exec = muse.NewExecutor(cfg, nil)
	}
	state.Unlock()
	formats := cfg.InputFormats()
	schema := pluginabi.SchemaVersion
	if req.SchemaVersion > 0 && req.SchemaVersion < schema {
		schema = req.SchemaVersion
	}
	return ok(registration{
		SchemaVersion: schema,
		Metadata: pluginapi.Metadata{
			Name:             "Muse (Meta) provider",
			Version:          Version,
			Author:           "DDD0s",
			GitHubRepository: "https://github.com/DDD0s/cpa-muse",
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			ModelRouter:           true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			QuotaProvider:         true,
		},
	})
}

// route claims Muse models for CPA's selected-auth provider execution path.
// Models outside the configured catalog fall through to the host.
func route(exec *muse.Executor, req pluginapi.ModelRouteRequest) pluginapi.ModelRouteResponse {
	model, claimed := exec.Config().RouteModel(req.RequestedModel)
	if !claimed {
		return pluginapi.ModelRouteResponse{}
	}
	// Select auth against the upstream model, retaining CPA's thinking suffix.
	requested := strings.TrimSpace(req.RequestedModel)
	model += requested[len(muse.UpstreamModel(requested)):]
	return pluginapi.ModelRouteResponse{
		Handled:     true,
		TargetKind:  pluginapi.ModelRouteTargetProvider,
		Target:      ExecutorIdentifier,
		TargetModel: model,
		Reason:      "muse plugin owns " + model,
	}
}

func ok(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func encodeError(err error) []byte {
	abiErr, isABI := err.(*pluginabi.Error)
	if !isABI {
		abiErr = muse.ABIError(err)
	}
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: abiErr})
	return raw
}

// ErrorEnvelope builds a failed envelope for ABI-level failures.
func ErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Extra Muse model IDs to claim besides the built-in catalog."},
		{Name: "prefixes", Type: pluginapi.ConfigFieldTypeArray, Description: "Client model prefixes to accept, e.g. meta for meta/muse-spark-1.3."},
		{Name: "aliases", Type: pluginapi.ConfigFieldTypeObject, Description: "Client model name to upstream Muse model."},
		{Name: "mint-url", Type: pluginapi.ConfigFieldTypeString, Description: "Override the DCA mint endpoint used by login and refresh (not credential selection)."},
		{Name: "auth-url", Type: pluginapi.ConfigFieldTypeString, Description: "Override the Meta OAuth host used by device login (tests, staging)."},
		{Name: "client", Type: pluginapi.ConfigFieldTypeObject, Description: "Muse Code identity override: version, build, surface, os, arch."},
		{Name: "muse-namespace", Type: pluginapi.ConfigFieldTypeBoolean, Default: true, Description: "Append the canonical muse tool namespace to tool-bearing requests."},
		{Name: "meta-messages", Type: pluginapi.ConfigFieldTypeObject, Description: "Native Messages surface: mode (legacy|enabled|disabled), allow-subscription, count-tokens, capabilities, degrade, limits."},
		{Name: "meta-responses", Type: pluginapi.ConfigFieldTypeObject, Description: "Responses surface: mode (legacy|native|disabled), header-profile (generic|muse), capabilities, limits."},
		{Name: "muse-profile", Type: pluginapi.ConfigFieldTypeObject, Description: "Muse wire profile engine: enabled, profile, fill-defaults, keys."},
	}
}
