package muse

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func nativeFixture() (*Executor, *Call) {
	e := NewExecutor(&Config{}, nil)
	call := e.newCall(selectedRequest(pluginapi.ExecutorRequest{Model: "muse-test", Format: "openai-response", SourceFormat: "openai-response", Payload: []byte(`{"model":"client-model","input":"hello"}`), Headers: make(http.Header)}, ""), "", false)
	return e, call
}
func TestNativeGenericHeaderBoundary(t *testing.T) {
	e, call := nativeFixture()
	call.Req.Headers.Set("Authorization", "Bearer downstream")
	call.Req.Headers.Set("X-Tbh-Session-Id", "stable")
	plan, err := (nativePlanner{}).plan(context.Background(), e, call, Credential{Headers: map[string]string{"X-Custom": "secret", "Authorization": "wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	expected := http.Header{"Authorization": {"Bearer upstream"}, "Content-Type": {"application/json"}, "Accept": {"text/event-stream"}, "User-Agent": {genericUserAgent}}
	if got := plan.headers("upstream"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("headers escaped generic boundary: %v", got)
	}
	if gjson.GetBytes(plan.body, "prompt_cache_key").String() != "tbh:main:stable" || !plan.native || !plan.sseFraming || !plan.nonStreamResponseObject {
		t.Fatalf("invalid native plan: %+v %s", plan, plan.body)
	}
	call.Req.Headers.Del("X-Tbh-Session-Id")
	plan, err = (nativePlanner{}).plan(context.Background(), e, call, Credential{})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(plan.body, "prompt_cache_key").Exists() {
		t.Fatal("generic request invented an ephemeral cache key")
	}
}
func TestNativeMuseIdentityAndAdmission(t *testing.T) {
	e, call := nativeFixture()
	e.cfg.Responses.HeaderProfile = "muse"
	call.Req.Headers.Set("X-Tbh-Session-Id", "conversation")
	plan, err := (nativePlanner{}).plan(context.Background(), e, call, Credential{})
	if err != nil {
		t.Fatal(err)
	}
	h := plan.headers("upstream")
	for _, name := range []string{"X-Tbh-Session-Id", "X-Meta-Ai-Gateway-Session-Id", "X-Tbh-Thread-Id", "X-Tbh-Root-Session-Id"} {
		if h.Get(name) != "conversation" {
			t.Fatalf("unaligned %s: %v", name, h)
		}
	}
	e.cfg.Responses.MaxRequestBytes = 4
	_, err = (nativePlanner{}).plan(context.Background(), e, call, Credential{})
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 413 || status.Scope != ScopeRequest {
		t.Fatalf("size error: %v", err)
	}
	e.cfg.Responses.MaxRequestBytes = 0
	e.cfg.Responses.ContextLimit = 1
	_, err = (nativePlanner{}).plan(context.Background(), e, call, Credential{})
	if !errors.As(err, &status) || status.Code != "cpa.meta.responses.estimated_context_limit" {
		t.Fatalf("context gate missing: %v", err)
	}
	count, err := (nativePlanner{}).count(context.Background(), e, call)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(count.Payload, "object").String() != "response.input_tokens" || gjson.GetBytes(count.Payload, "input_tokens").Int() <= 0 || count.Headers.Get("X-CPA-Token-Count-Basis") != "normalized-native" {
		t.Fatalf("count contract changed: %+v", count)
	}
}
func TestProfilePlannerDefaultAndWireFreeze(t *testing.T) {
	e, call := nativeFixture()
	call.Stream = true
	call.Req.Headers.Set("Session_id", "conversation")
	call.Req.Metadata = map[string]any{"caller_scope": "caller", "request_id": "request"}
	planner := profilePlanner{selection: ProfileSelection{Enabled: true, FillDefaults: true}}
	ctx := WithRequestState(context.Background())
	first, err := planner.plan(ctx, e, call, Credential{ID: "profile-test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.plan(ctx, &Executor{cfg: &Config{}}, call, Credential{ID: "profile-test"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.body, second.body) || !reflect.DeepEqual(first.headers("a"), second.headers("a")) {
		t.Fatal("reconfiguration changed active wire plan")
	}
	h := first.headers("a")
	id := h.Get("X-Tbh-Session-Id")
	if !first.decode || !strings.Contains(h.Get("User-Agent"), "muse-build/1.4.14 (non-interactive; linux-x86_64; build "+defaultClientBuild+")") || h.Get("Accept-Encoding") != "gzip,deflate,br" || h.Get("X-Tbh-Thread-Id") != id || h.Get("X-Tbh-Root-Session-Id") != id || h.Get("X-Tbh-Contributor-Mode") != "" {
		t.Fatalf("profile default/identity mismatch: %v", h)
	}
	if gjson.GetBytes(first.body, "prompt_cache_key").String() != "tbh:main:"+id {
		t.Fatal("cache key mismatch")
	}
}
func TestResponsesToCodexResponseTransformer(t *testing.T) {
	transformer := newCodexToResponsesTransformer([]byte(`{"model":"client-model"}`))
	for _, kind := range []string{"response.created", "response.in_progress"} {
		events, err := transformer.Transform([]byte(`{"type":"` + kind + `","response":{"id":"r"}}`))
		if err != nil || len(events) != 1 || gjson.GetBytes(events[0], "response.model").String() != "client-model" {
			t.Fatalf("missing request model: %s %v", events, err)
		}
	}
	terminal := []byte(`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"web_search_call","action":{"sources":[{"url":"https://example.org"}]}}]}}`)
	got, err := transformer.TransformCompleted(terminal)
	if err != nil || !bytes.Equal(got, terminal) {
		t.Fatalf("terminal evidence changed: %s %v", got, err)
	}
	plan := &responsesPlan{nonStreamResponseObject: true}
	payload, err := (&responsesSurface{}).finishCompleted(plan, got, nil, nil)
	if err != nil || gjson.GetBytes(payload, "status").String() != "incomplete" || gjson.GetBytes(payload, "output.0.action.sources.0.url").String() != "https://example.org" {
		t.Fatalf("non-stream shape changed: %s %v", payload, err)
	}
}

func TestProfileCurrentEmbeddedPlatforms(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		profile, err := loadProfile("profiles/muse-" + platform + "-exec-1.4.14.json")
		if err != nil {
			t.Fatal(err)
		}
		want := "muse-build/1.4.14 (non-interactive; " + platform + "-x86_64; build " + defaultClientBuild + ")"
		if profile.StaticHeaders["User-Agent"] != want || profile.StaticHeaders["X-Client-Id"] != "tbh:exec" {
			t.Fatalf("unexpected %s identity: %v", platform, profile.StaticHeaders)
		}
	}
}
