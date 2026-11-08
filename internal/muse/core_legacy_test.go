package muse

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestCoreLegacyIdentitySessionAndThinking(t *testing.T) {
	const stable = "2ab8673a-422c-4a64-a966-0cdececbde27"
	for _, session := range []string{stable, "not-a-uuid"} {
		t.Run(session, func(t *testing.T) {
			e := NewExecutor(coreConfig(), nil)
			req := pluginapi.ExecutorRequest{Model: "muse-spark-1.3(high)", Format: FormatCodex, Headers: http.Header{"X-Claude-Code-Session-Id": {session}}, Payload: []byte(`{"input":[],"generate":true,"prompt_cache_retention":"24h","safety_identifier":"secret","stream_options":{},"client_metadata":{}}`)}
			call := e.newCall(selectedRequest(req, ""), "", false)
			plan, err := (legacyPlanner{}).plan(context.Background(), e, call, Credential{})
			if err != nil {
				t.Fatal(err)
			}
			h := plan.headers("upstream-key")
			if !strings.HasPrefix(h.Get("User-Agent"), "muse-build/1.4.14 ") || h.Get("X-Client-Id") != "tbh:exec" || h.Get("Authorization") != "Bearer upstream-key" {
				t.Fatalf("legacy identity: %v", h)
			}
			id := h.Get("X-Tbh-Session-Id")
			for _, name := range []string{"X-Meta-Ai-Gateway-Session-Id", "X-Tbh-Thread-Id", "X-Tbh-Root-Session-Id"} {
				if h.Get(name) != id {
					t.Fatalf("session identity diverged: %v", h)
				}
			}
			if session == stable && id != stable {
				t.Fatalf("stable UUID ignored: %q", id)
			}
			if session != stable {
				if _, err := uuid.Parse(id); err != nil || id == session {
					t.Fatalf("non-UUID accepted as identity: %q", id)
				}
			}
			if gjson.GetBytes(plan.body, "prompt_cache_key").String() != "tbh:main:"+id || gjson.GetBytes(plan.body, "reasoning.effort").String() != "high" || gjson.GetBytes(plan.body, "model").String() != "muse-spark-1.3" || !gjson.GetBytes(plan.body, "stream").Bool() {
				t.Fatalf("legacy normalization: %s", plan.body)
			}
			for _, field := range []string{"generate", "prompt_cache_retention", "safety_identifier", "stream_options", "client_metadata"} {
				if gjson.GetBytes(plan.body, field).Exists() {
					t.Errorf("dropped field %s remains: %s", field, plan.body)
				}
			}
		})
	}
}

func TestCoreContributorRoutingUsesSelectedModel(t *testing.T) {
	for _, mode := range []string{"legacy", "native-muse", "native-generic"} {
		for _, tc := range []struct {
			model string
			want  string
		}{
			{"muse-spark-1.3(high)", "0"},
			{"muse-spark-1.3-contributor(high)", "1"},
			{"muse-spark-1.3-internal", ""},
			{"muse-spark-1.3-contributor-extra", "0"},
		} {
			t.Run(mode+"/"+tc.model, func(t *testing.T) {
				e := NewExecutor(coreConfig(), nil)
				call := e.newCall(selectedRequest(pluginapi.ExecutorRequest{Model: tc.model, Format: FormatCodex, Payload: []byte(`{"input":"hello","model":"untrusted-contributor"}`)}, ""), "", false)
				var plan *responsesPlan
				var err error
				if mode == "legacy" {
					plan, err = (legacyPlanner{}).plan(context.Background(), e, call, Credential{})
				} else {
					e.cfg.Responses.HeaderProfile = strings.TrimPrefix(mode, "native-")
					plan, err = (nativePlanner{}).plan(context.Background(), e, call, Credential{})
				}
				if err != nil {
					t.Fatal(err)
				}
				want := tc.want
				if mode == "native-generic" {
					want = ""
				}
				if got := plan.headers("upstream").Get("X-Tbh-Contributor-Mode"); got != want {
					t.Fatalf("model %q contributor mode = %q, want %q", call.Model, got, want)
				}
			})
		}
	}
}

func TestCoreLegacyMuseNamespaceOnceForToolsOnly(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
		want        int
	}{
		{"absent", "", 0},
		{"empty", `,"tools":[]`, 0},
		{"function", `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`, 1},
		{"already-present", `,"tools":[{"type":"namespace","name":"muse","tools":[]}]`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExecutor(coreConfig(), nil)
			call := e.newCall(selectedRequest(pluginapi.ExecutorRequest{Model: "muse-spark-1.3", Format: FormatCodex, Payload: []byte(`{"input":[]` + tc.tools + `}`)}, ""), "", false)
			for range 2 {
				plan, err := (legacyPlanner{}).plan(context.Background(), e, call, Credential{})
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, tool := range gjson.GetBytes(plan.body, "tools").Array() {
					if tool.Get("type").String() == "namespace" && tool.Get("name").String() == "muse" {
						count++
					}
				}
				if count != tc.want {
					t.Fatalf("namespace count=%d want=%d body=%s", count, tc.want, plan.body)
				}
				call.Req.Payload = plan.body
			}
		})
	}
}

func TestCoreRouteModelCatalogPrefixesAliasesAndSuffix(t *testing.T) {
	cfg := coreConfig()
	cfg.Prefixes = []string{"meta"}
	cfg.Aliases = map[string]string{"smart": "muse-spark-1.3"}
	cfg.Models = []string{"custom-muse"}
	models := cfg.Catalog()
	if len(models) == 0 {
		t.Fatal("empty consumer catalog")
	}
	for _, model := range models {
		for _, requested := range []string{model.ID, "meta/" + model.ID, model.ID + "(high)", "meta/" + model.ID + "(high)"} {
			if got, claimed := cfg.RouteModel(requested); !claimed || got != model.ID {
				t.Errorf("catalog route %q = %q, %v", requested, got, claimed)
			}
		}
	}
	for _, alias := range []string{"smart", "meta/smart", "smart(high)"} {
		if got, claimed := cfg.RouteModel(alias); !claimed || got != "muse-spark-1.3" {
			t.Errorf("alias route %q = %q, %v", alias, got, claimed)
		}
	}
	for _, unknown := range []string{"", "unknown", "meta/unknown(high)", "other/muse-spark-1.3"} {
		if got, claimed := cfg.RouteModel(unknown); claimed || got != "" {
			t.Errorf("unknown route %q = %q, %v", unknown, got, claimed)
		}
	}
}

func TestCoreABIErrorPreservesJSONAndRoundsRetryAfter(t *testing.T) {
	const body = `{"error": {"code":"rate_limited", "message":"upstream text"}}`
	for _, retry := range []struct {
		duration time.Duration
		seconds  int
	}{{time.Nanosecond, 1}, {time.Second, 1}, {time.Second + time.Nanosecond, 2}, {1500 * time.Millisecond, 2}} {
		err := fmt.Errorf("wrapped: %w", &StatusError{Status: 429, Code: "rate_limited", Message: "do not expose this instead", Body: []byte(body), RetryAfter: retry.duration})
		got := ABIError(err)
		if got == nil || got.Message != body || got.RetryAfterSeconds != retry.seconds || got.HTTPStatus != 429 || !got.Retryable {
			t.Fatalf("ABI lost body/status/retry: %+v", got)
		}
	}
	if ABIError(nil) != nil {
		t.Fatal("nil error became terminal error")
	}
}
