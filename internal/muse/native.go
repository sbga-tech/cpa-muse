package muse

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// nativePlanner ports meta_responses_native_executor.go. Host translation and
// payload rules have already run at the plugin boundary; thinking is suffix-free.
type nativePlanner struct{}

func (nativePlanner) plan(ctx context.Context, e *Executor, call *Call, cred Credential) (*responsesPlan, error) {
	body, stable, err := normalizeNativeResponses(e, call)
	if err != nil {
		return nil, err
	}
	original := call.Req.OriginalRequest
	if len(original) == 0 {
		original = call.Req.Payload
	}
	bridge, body, err := newApplyPatchBridge(call.Req.SourceFormat, original, body)
	if err != nil {
		return nil, MetaResponsesError(400, "invalid_apply_patch", "tools", err.Error())
	}
	if err := validateNativeAdmission(body, e.cfg.Responses, call.Model, true, !isNativeCount(ctx)); err != nil {
		return nil, err
	}
	p := nativeResponsePlan(body, bridge)
	profile := e.cfg.HeaderProfile()
	identity := e.cfg.identity()
	p.headers = func(token string) http.Header {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "text/event-stream")
		if profile == "muse" {
			applyMuseIdentity(h, identity, stable, call.Model)
			h.Set("Cache-Control", "no-cache")
			for name, value := range cred.Headers {
				h.Set(name, value)
			}
		} else {
			h.Set("User-Agent", genericUserAgent)
		}
		if profile == "muse" {
			// Session identity remains derived from the normalized request.
			applyMuseIdentitySession(h, stable)
		}
		return h
	}
	return p, nil
}

func normalizeNativeResponses(e *Executor, call *Call) ([]byte, string, error) {
	if err := nativeRequestSize(call.Req.Payload, e.cfg.Responses, false); err != nil {
		return nil, "", err
	}
	if _, err := DecodeMetaMessages(call.Req.Payload); err != nil {
		return nil, "", MetaResponsesError(400, "invalid_json", "", "Request must be a JSON object without duplicate keys")
	}
	if profile := e.cfg.HeaderProfile(); profile != "generic" && profile != "muse" {
		return nil, "", MetaResponsesError(400, "invalid_configuration", "header-profile", "Header profile must be generic or muse")
	}
	body := setString(call.Req.Payload, "model", call.Model)
	body = setBool(body, "stream", true)
	body = applyThinkingLevel(body, call.ThinkingLevel)
	if !gjson.GetBytes(body, "store").Exists() {
		body = setBool(body, "store", false)
	}
	var stable string
	if e.cfg.HeaderProfile() == "muse" {
		stable = sessionID(call.Req.Headers)
	} else {
		stable, _ = stableSessionID(call.Req.Headers)
	}
	if stable != "" {
		body = ensurePromptCacheKey(body, stable)
	}
	if e.cfg.HeaderProfile() == "muse" {
		stable = strings.TrimPrefix(gjson.GetBytes(body, "prompt_cache_key").String(), "tbh:main:")
	}
	return body, stable, nil
}

func applyMuseIdentitySession(h http.Header, id string) {
	for _, name := range []string{"X-Tbh-Session-Id", "X-Meta-Ai-Gateway-Session-Id", "X-Tbh-Thread-Id", "X-Tbh-Root-Session-Id"} {
		h.Set(name, id)
	}
}

func nativeResponsePlan(body []byte, bridge eventTransformer) *responsesPlan {
	p := &responsesPlan{body: body, native: true, sseFraming: true, nonStreamResponseObject: true}
	if bridge != nil {
		p.transforms = append(p.transforms, bridge)
	}
	p.transforms = append(p.transforms, &nativeOutputTransformer{})
	return p
}

func nativeRequestSize(body []byte, cfg ResponsesConfig, final bool) error {
	limit := cfg.MaxRequestBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	if int64(len(body)) <= limit {
		return nil
	}
	reason := "Request exceeds the configured native Responses size limit"
	if final {
		reason = "Final payload exceeds the configured native Responses size limit"
	}
	return MetaResponsesError(413, "request_too_large", "", reason)
}

func validateNativeAdmission(body []byte, cfg ResponsesConfig, model string, requireSSE, checkContext bool) error {
	if err := ValidateMetaResponsesNative(body, cfg); err != nil {
		return err
	}
	if gjson.GetBytes(body, "model").String() != model {
		return MetaResponsesError(400, "model_override", "model", "Payload rules cannot change the selected upstream model")
	}
	stream := gjson.GetBytes(body, "stream")
	if stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return MetaResponsesError(400, "stream_override", "stream", "Profile stream must be a boolean when supplied")
	}
	if requireSSE && stream.Type != gjson.True {
		return MetaResponsesError(400, "stream_override", "stream", "Native Responses requires upstream SSE; payload rules cannot change the transport")
	}
	if err := nativeRequestSize(body, cfg, true); err != nil {
		return err
	}
	if checkContext && cfg.ContextLimit > 0 {
		count, err := estimateNativeResponses(body)
		if err != nil {
			return MetaResponsesError(500, "count_failed", "", "Local token estimation failed")
		}
		limit := int64(cfg.ContextLimit)
		output := gjson.GetBytes(body, "max_output_tokens").Int()
		if output >= limit || count > limit-output {
			return MetaResponsesError(400, "estimated_context_limit", "input", "The local approximate estimate plus output budget exceeds the configured context limit")
		}
	}
	return nil
}

type nativeCountContextKey struct{}

func isNativeCount(ctx context.Context) bool {
	v, _ := ctx.Value(nativeCountContextKey{}).(bool)
	return v
}
func countNativePlan(ctx context.Context, e *Executor, call *Call, p planner) (pluginapi.ExecutorResponse, error) {
	ctx = context.WithValue(ctx, nativeCountContextKey{}, true)
	prepared, err := p.plan(ctx, e, call, Credential{ID: "muse-local-count"})
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	count, err := estimateNativeResponses(prepared.body)
	if err != nil {
		return pluginapi.ExecutorResponse{}, MetaResponsesError(500, "count_failed", "", "Local token estimation failed")
	}
	return nativeCountResponse(count), nil
}
func (p nativePlanner) count(ctx context.Context, e *Executor, call *Call) (pluginapi.ExecutorResponse, error) {
	return countNativePlan(ctx, e, call, p)
}
func nativeCountResponse(count int64) pluginapi.ExecutorResponse {
	body, _ := json.Marshal(map[string]any{"object": "response.input_tokens", "input_tokens": count})
	return pluginapi.ExecutorResponse{Payload: body, Headers: http.Header{"Content-Type": {"application/json"}, "X-Cpa-Token-Count": {"estimate"}, "X-Cpa-Token-Count-Method": {"o200k-semantic-json-vision-heuristic-v2"}, "X-Cpa-Token-Count-Uncertainty": {"uncalibrated"}, "X-Cpa-Token-Count-Basis": {"normalized-native"}}}
}

// The process-wide engine retains caller/credential conversation affinity across
// plugin reconfiguration. Only templates and generated identity enter its caches.
var museProfileEngine = newProfileEngine()

type profilePlanner struct{ selection ProfileSelection }

func (p profilePlanner) plan(ctx context.Context, e *Executor, call *Call, cred Credential) (*responsesPlan, error) {
	if err := nativeRequestSize(call.Req.Payload, e.cfg.Responses, false); err != nil {
		return nil, err
	}
	if call.ThinkingLevel != "" {
		return nil, profileStatusError(&profileError{400, "unsupported_model_suffix", "Profiles require a plain model name; use explicit Responses fields instead of CPA thinking suffix syntax"})
	}
	if cred.ID == "" {
		return nil, profileStatusError(&profileError{400, "missing_credential_id", "Selected credential needs a stable ID"})
	}
	caller := museProfileCaller(ctx, e, call)
	selection := FreezeSelection(ctx, caller, p.selection)
	key := digestStrings(caller, cred.ID, call.Model, call.Req.Format, call.Req.SourceFormat, string(call.Req.Payload))
	body, headers, err := FreezeWirePlan(ctx, key, func() ([]byte, http.Header, error) {
		custom := make(http.Header, len(cred.Headers))
		for name, value := range cred.Headers {
			custom.Set(name, value)
		}
		prepared, err := museProfileEngine.Prepare(selection, profileInput{Body: setString(call.Req.Payload, "model", call.Model), Headers: call.Req.Headers, CustomHeaders: custom, CallerKey: caller, CredentialID: cred.ID, RequestID: profileRequestID(call), State: RequestStateFromContext(ctx)})
		if err != nil {
			return nil, nil, err
		}
		return prepared.Body(), prepared.Headers(), nil
	})
	if err != nil {
		return nil, profileStatusError(err)
	}
	if err := validateNativeAdmission(body, e.cfg.Responses, call.Model, call.Stream, !isNativeCount(ctx)); err != nil {
		return nil, err
	}
	plan := nativeResponsePlan(body, nil)
	plan.decode = true
	plan.headers = func(token string) http.Header {
		h := headers.Clone()
		h.Set("Authorization", "Bearer "+token)
		return h
	}
	return plan, nil
}
func (p profilePlanner) count(ctx context.Context, e *Executor, call *Call) (pluginapi.ExecutorResponse, error) {
	return countNativePlan(ctx, e, call, p)
}
func profileStatusError(err error) error {
	if p, ok := err.(*profileError); ok {
		return &StatusError{Status: p.status, Code: p.Code(), Body: []byte(p.Error())}
	}
	return err
}
func profileRequestID(call *Call) string {
	for _, key := range []string{"request_id", "request-id"} {
		if value, ok := call.Req.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Caller scoping replaces the fork's gin userApiKey lookup with SDK metadata.
func museProfileCaller(ctx context.Context, e *Executor, call *Call) string {
	if principal := downstreamPrincipal(e.cfg, call.Req); principal != "" {
		return session.CallerScope(principal)
	}
	if scope, ok := call.Req.Metadata["caller_scope"].(string); ok && strings.TrimSpace(scope) != "" {
		return scope
	}
	if key := bearerKey(call.Req.Headers); key != "" {
		return session.CallerScope(key)
	}
	if state := RequestStateFromContext(ctx); state != nil {
		return state.AnonymousCaller()
	}
	if id := profileRequestID(call); id != "" {
		return "anonymous:" + id
	}
	return "anonymous:" + uuid.NewString()
}
