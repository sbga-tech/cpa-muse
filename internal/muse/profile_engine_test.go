package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func selectedProfile() ProfileSelection {
	return ProfileSelection{Enabled: true, Profile: "profiles/muse-linux-exec-1.4.7.json"}
}

func fixtureInput(requestID, conversation string) profileInput {
	headers := http.Header{"Authorization": {"Bearer downstream-fixture"}, "User-Agent": {"ordinary-client"}}
	if conversation != "" {
		headers.Set("Session_id", conversation)
	}
	return profileInput{Body: []byte(`{"model":"fixture","input":"hello"}`), Headers: headers, CallerKey: "caller-fixture", CredentialID: "credential-fixture", RequestID: requestID}
}

func mustPrepare(t *testing.T, engine *profileEngine, selection ProfileSelection, input profileInput) *profilePlan {
	t.Helper()
	plan, err := engine.Prepare(selection, input)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func planObject(t *testing.T, plan *profilePlan) map[string]json.RawMessage {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(plan.Body(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func traceParts(t *testing.T, plan *profilePlan) []string {
	t.Helper()
	parts := strings.Split(plan.Headers().Get("Traceparent"), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || parts[3] != "01" {
		t.Fatalf("invalid trace shape: %q", parts)
	}
	return parts
}

func TestProfileProfilesContainOnlyObservedPlatformDifference(t *testing.T) {
	linux, err := loadProfile(selectedProfile().Profile)
	if err != nil {
		t.Fatal(err)
	}
	windows, err := loadProfile("profiles/muse-windows-exec-1.4.7.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(linux.StaticHeaders["User-Agent"], "linux-x86_64") || !strings.Contains(windows.StaticHeaders["User-Agent"], "windows-x86_64") {
		t.Fatal("profile platforms differ from the observations")
	}
	delete(linux.StaticHeaders, "User-Agent")
	delete(windows.StaticHeaders, "User-Agent")
	if !reflect.DeepEqual(linux.StaticHeaders, windows.StaticHeaders) || !reflect.DeepEqual(linux.Defaults, windows.Defaults) {
		t.Fatal("platform profiles contain unobserved differences")
	}
	for _, profile := range []*profile{linux, windows} {
		if profile.StaticHeaders["X-Client-Id"] != "tbh:exec" || profile.Defaults["tools"] != nil || profile.Defaults["instructions"] != nil || profile.Defaults["parallel_tool_calls"] != nil {
			t.Fatal("profile injected an unobserved default")
		}
	}
}

func TestProfileDisabledPlanPreservesBytesAndTransportCustomHeaders(t *testing.T) {
	input := fixtureInput("request", "conversation")
	input.Body = []byte("not-json and deliberately unchanged")
	input.CustomHeaders = http.Header{"Accept": {"application/json"}, "Content-Type": {"custom/type"}, "Accept-Encoding": {"identity"}}
	plan := mustPrepare(t, newProfileEngine(), ProfileSelection{Profile: "missing-profile", FillDefaults: true}, input)
	if !bytes.Equal(plan.Body(), input.Body) || !reflect.DeepEqual(plan.Headers(), input.CustomHeaders) || len(plan.Transformations()) != 0 || plan.EphemeralSession() {
		t.Fatal("disabled profile changed the request")
	}
}

func TestProfileDefaultsOffPreservesAllExplicitMemberValues(t *testing.T) {
	input := fixtureInput("request", "conversation")
	input.Body = []byte(`{ "model" : "fixture", "input" : [{ "role":"user", "content":"hello" }], "instructions":"Keep exactly", "tools":[{"type":"function","name":"mine","parameters":{"type":"object"}}], "reasoning":{"effort":"low"}, "max_output_tokens":17, "stream":false, "store":true, "parallel_tool_calls":false }`)
	var original map[string]json.RawMessage
	if err := json.Unmarshal(input.Body, &original); err != nil {
		t.Fatal(err)
	}
	plan := mustPrepare(t, newProfileEngine(), selectedProfile(), input)
	got := planObject(t, plan)
	for name, value := range original {
		if !bytes.Equal(got[name], value) {
			t.Fatalf("explicit member %s was rewritten: %s", name, got[name])
		}
	}
	if len(got) != len(original)+1 || got["include"] != nil || got["prompt_cache_key"] == nil {
		t.Fatalf("unexpected default insertion: %s", plan.Body())
	}
	if plan.Headers().Get("Authorization") != "" || plan.Headers().Get("User-Agent") == input.Headers.Get("User-Agent") || plan.Headers().Get("Accept-Encoding") != "gzip,deflate,br" {
		t.Fatal("profile headers leaked caller credentials or ignored profile data")
	}
}

func TestProfileDoesNotInferContributorRouting(t *testing.T) {
	engine := newProfileEngine()
	for _, model := range []string{"muse-spark-1.3", "muse-spark-1.3-contributor", "muse-spark-1.3-internal"} {
		input := fixtureInput("request-"+model, "conversation")
		input.Body = []byte(`{"model":"` + model + `","input":"hello"}`)
		plan := mustPrepare(t, engine, selectedProfile(), input)
		if got := plan.Headers().Get("X-Tbh-Contributor-Mode"); got != "" {
			t.Fatalf("profile inferred contributor routing for %s: %q", model, got)
		}
	}
}

func TestProfileFillDefaultsOnlyTouchesMissingMembers(t *testing.T) {
	selection := selectedProfile()
	selection.FillDefaults = true
	input := fixtureInput("request", "conversation")
	input.Body = []byte(`{"input":"hello","reasoning":{"effort":"low"},"max_output_tokens":7,"include":[],"store":true,"stream":false}`)
	plan := mustPrepare(t, newProfileEngine(), selection, input)
	got := planObject(t, plan)
	for name, value := range map[string]string{"max_output_tokens": "7", "include": "[]", "store": "true", "stream": "false"} {
		if string(got[name]) != value {
			t.Fatalf("explicit %s changed", name)
		}
	}
	if string(got["reasoning"]) != `{"effort":"low","summary":"auto"}` {
		t.Fatalf("nested defaults incorrect: %s", got["reasoning"])
	}
	input.Body = []byte(`{"input":"hello","reasoning":null,"prompt_cache_key":null}`)
	plan = mustPrepare(t, newProfileEngine(), selection, input)
	got = planObject(t, plan)
	if string(got["reasoning"]) != "null" || string(got["prompt_cache_key"]) != "null" || string(got["max_output_tokens"]) != "128000" {
		t.Fatalf("explicit null or observed default changed: %s", plan.Body())
	}
}

func TestProfileConversationAndRequestLifetimesAreSeparate(t *testing.T) {
	engine := newProfileEngine()
	first := mustPrepare(t, engine, selectedProfile(), fixtureInput("request-1", "conversation"))
	second := mustPrepare(t, engine, selectedProfile(), fixtureInput("request-2", "conversation"))
	retry := mustPrepare(t, engine, selectedProfile(), fixtureInput("request-1", "conversation"))
	id := first.Headers().Get("X-Tbh-Session-Id")
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 7 || first.EphemeralSession() {
		t.Fatalf("session is not a scoped UUIDv7: %v", err)
	}
	if second.Headers().Get("X-Tbh-Session-Id") != id || first.Headers().Get("X-Meta-Ai-Gateway-Session-Id") != id {
		t.Fatal("conversation session changed")
	}
	firstTrace, secondTrace := traceParts(t, first), traceParts(t, second)
	if firstTrace[1] != secondTrace[1] || firstTrace[2] == secondTrace[2] || firstTrace[1] == strings.ReplaceAll(id, "-", "") {
		t.Fatal("trace lifetime or independence is wrong")
	}
	if !bytes.Equal(first.Body(), retry.Body()) || !reflect.DeepEqual(first.Headers(), retry.Headers()) {
		t.Fatal("retry plan changed")
	}
	var cacheKey string
	if err := json.Unmarshal(planObject(t, first)["prompt_cache_key"], &cacheKey); err != nil || cacheKey != "tbh:main:"+id {
		t.Fatal("generated cache key is not aligned")
	}
}

func TestProfileScopeIsolationAndIdentityPriority(t *testing.T) {
	engine := newProfileEngine()
	input := fixtureInput("request", "codex-low-priority")
	input.Headers.Set("X-Tbh-Session-Id", "caller-owned-session")
	input.Body = []byte(`{"input":"hello","prompt_cache_key":"explicit-cache-A"}`)
	first := mustPrepare(t, engine, selectedProfile(), input)
	input.RequestID = "next-request"
	input.Headers.Set("Session_id", "different-low-priority")
	input.Body = []byte(`{"input":"hello","prompt_cache_key":"explicit-cache-B"}`)
	second := mustPrepare(t, engine, selectedProfile(), input)
	id := first.Headers().Get("X-Tbh-Session-Id")
	if id == "caller-owned-session" || second.Headers().Get("X-Tbh-Session-Id") != id {
		t.Fatal("explicit sessions were not scoped or priority changed")
	}
	if string(planObject(t, second)["prompt_cache_key"]) != `"explicit-cache-B"` {
		t.Fatal("explicit cache key changed")
	}
	input.CallerKey = "different-caller"
	otherCaller := mustPrepare(t, engine, selectedProfile(), input)
	input.CallerKey = "caller-fixture"
	input.CredentialID = "different-credential"
	otherCredential := mustPrepare(t, engine, selectedProfile(), input)
	if otherCaller.Headers().Get("X-Tbh-Session-Id") == id || otherCredential.Headers().Get("X-Tbh-Session-Id") == id {
		t.Fatal("caller or credential isolation failed")
	}
	for _, name := range []string{"Session_id", "Session-Id", "X-Codex-Session-Id", "X-Conversation-Id", "Conversation_id", "Conversation-Id", "X-Session-Id"} {
		input := fixtureInput("", "")
		input.Headers.Set(name, "conversation")
		plan := mustPrepare(t, engine, selectedProfile(), input)
		next := mustPrepare(t, engine, selectedProfile(), input)
		if plan.EphemeralSession() || plan.Headers().Get("X-Tbh-Session-Id") != next.Headers().Get("X-Tbh-Session-Id") || plan.Headers().Get("Traceparent") == next.Headers().Get("Traceparent") {
			t.Fatalf("Codex conversation header %s failed", name)
		}
	}
}

func TestProfileAnonymousConversationDoesNotMergeAndRetriesAreExplicit(t *testing.T) {
	engine := newProfileEngine()
	input := fixtureInput("", "")
	first := mustPrepare(t, engine, selectedProfile(), input)
	second := mustPrepare(t, engine, selectedProfile(), input)
	if !first.EphemeralSession() || !second.EphemeralSession() || first.Headers().Get("X-Tbh-Session-Id") == second.Headers().Get("X-Tbh-Session-Id") {
		t.Fatal("missing conversation input merged requests")
	}
	input.RequestID = "explicit-retry-id"
	first = mustPrepare(t, engine, selectedProfile(), input)
	second = mustPrepare(t, engine, selectedProfile(), input)
	if !bytes.Equal(first.Body(), second.Body()) || !reflect.DeepEqual(first.Headers(), second.Headers()) {
		t.Fatal("ephemeral retry changed")
	}
	input.Body = []byte(`{"model":"fixture","input":"changed"}`)
	third := mustPrepare(t, engine, selectedProfile(), input)
	if third.Headers().Get("Traceparent") == second.Headers().Get("Traceparent") {
		t.Fatal("request ID reused a plan for a different body")
	}
}

func TestProfileCacheKeyPrecedesCodexHeadersAndAliasesHaveDefinedPriority(t *testing.T) {
	engine := newProfileEngine()
	input := fixtureInput("one", "first-codex-id")
	input.Body = []byte(`{"input":"hello","prompt_cache_key":"cache-conversation"}`)
	first := mustPrepare(t, engine, selectedProfile(), input)
	input.RequestID = "two"
	input.Headers.Set("Session_id", "different-codex-id")
	second := mustPrepare(t, engine, selectedProfile(), input)
	if first.Headers().Get("X-Tbh-Session-Id") != second.Headers().Get("X-Tbh-Session-Id") {
		t.Fatal("body cache key did not precede Codex headers")
	}
	names := []string{"Session_id", "Session-Id", "X-Codex-Session-Id", "X-Conversation-Id", "Conversation_id", "Conversation-Id", "X-Session-Id"}
	for index, winner := range names {
		input := fixtureInput("", "")
		for _, name := range names[index:] {
			input.Headers.Set(name, name+"-original")
		}
		first := mustPrepare(t, engine, selectedProfile(), input)
		for _, name := range names[index+1:] {
			input.Headers.Set(name, name+"-changed")
		}
		second := mustPrepare(t, engine, selectedProfile(), input)
		if first.Headers().Get("X-Tbh-Session-Id") != second.Headers().Get("X-Tbh-Session-Id") {
			t.Fatalf("priority below %s changed the conversation", winner)
		}
	}
}

func TestProfilePlanAccessorsAndInputsAreIsolated(t *testing.T) {
	engine := newProfileEngine()
	input := fixtureInput("request", "conversation")
	input.CustomHeaders = http.Header{"X-Fixture": {"original"}}
	plan := mustPrepare(t, engine, selectedProfile(), input)
	body, headers, transforms := plan.Body(), plan.Headers(), plan.Transformations()
	body[0] = '!'
	headers.Set("User-Agent", "mutated")
	transforms[0] = "mutated"
	input.Body[0] = '!'
	input.CustomHeaders.Set("X-Fixture", "mutated")
	if plan.Body()[0] != '{' || plan.Headers().Get("User-Agent") == "mutated" || plan.Headers().Get("X-Fixture") != "original" || plan.Transformations()[0] == "mutated" {
		t.Fatal("plan shares mutable inputs or accessors")
	}
	retryInput := fixtureInput("request", "conversation")
	retryInput.CustomHeaders = http.Header{"X-Fixture": {"original"}}
	retry := mustPrepare(t, engine, selectedProfile(), retryInput)
	if !bytes.Equal(plan.Body(), retry.Body()) || !reflect.DeepEqual(plan.Headers(), retry.Headers()) {
		t.Fatal("accessors mutated the cached template")
	}
}

func TestProfileConflictsAreLocalAndDoNotDiscloseValues(t *testing.T) {
	secret := "fixture-secret-that-must-not-appear"
	for _, name := range []string{"Authorization", "proxy-authorization", "X-API-KEY", "Host", "User-Agent", "X-Client-Id", "X-Tbh-Agent-Role", "X-Tbh-Session-Id", "X-Meta-Ai-Gateway-Session-Id", "Traceparent"} {
		input := fixtureInput("", "conversation")
		input.CustomHeaders = http.Header{name: {secret}}
		_, err := newProfileEngine().Prepare(selectedProfile(), input)
		var profileError *profileError
		if !errors.As(err, &profileError) || profileError.StatusCode() != 400 || profileError.Code() != "cpa.meta.profile.identity_conflict" || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe conflict error for %s: %v", name, err)
		}
	}
	for _, name := range []string{"Accept", "Content-Type", "Accept-Encoding", "Host"} {
		input := fixtureInput("", "conversation")
		input.CustomHeaders = http.Header{name: {"same-or-different"}}
		if err := ValidateCustomHeaders(input.CustomHeaders); err != nil {
			t.Fatalf("disabled hardening widened to %s", name)
		}
		if _, err := newProfileEngine().Prepare(selectedProfile(), input); err == nil {
			t.Fatalf("enabled profile transport override accepted: %s", name)
		}
	}
	for _, headers := range []http.Header{
		{"X-Tbh-Session-Id": {"one"}, "X-Meta-Ai-Gateway-Session-Id": {"two"}},
		{"X-Tbh-Session-Id": {"one", "two"}},
		{"X-Client-Id": {"tbh:tui"}},
	} {
		input := fixtureInput("", "")
		input.Headers = headers
		if _, err := newProfileEngine().Prepare(selectedProfile(), input); err == nil {
			t.Fatal("conflicting inbound identity accepted")
		}
	}
}

func TestProfileRetryPreparationIsConcurrentAndBounded(t *testing.T) {
	engine := newProfileEngine()
	input := fixtureInput("request", "conversation")
	var wg sync.WaitGroup
	start := make(chan struct{})
	plans := make(chan *profilePlan, 32)
	errorsFound := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			plan, err := engine.Prepare(selectedProfile(), input)
			if err != nil {
				errorsFound <- err
				return
			}
			plans <- plan
		}()
	}
	close(start)
	wg.Wait()
	close(plans)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	var first *profilePlan
	for plan := range plans {
		if first == nil {
			first = plan
		} else if !bytes.Equal(first.Body(), plan.Body()) || !reflect.DeepEqual(first.Headers(), plan.Headers()) {
			t.Fatal("concurrent retries produced distinct plans")
		}
	}
	if len(engine.sessions.entries) != 1 || len(engine.requests.entries) != 1 {
		t.Fatal("concurrent retries duplicated state")
	}
	engine.sessions.capacity, engine.requests.capacity = 2, 2
	for i := range 6 {
		mustPrepare(t, engine, selectedProfile(), fixtureInput(fmt.Sprintf("request-%d", i), fmt.Sprintf("conversation-%d", i)))
	}
	if len(engine.sessions.entries) != 2 || len(engine.requests.entries) != 2 {
		t.Fatal("cache capacity was exceeded")
	}
}

func TestProfileSessionAndRequestExpiryUseExplicitClock(t *testing.T) {
	engine := newProfileEngine()
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return now }
	input := fixtureInput("request", "conversation")
	first := mustPrepare(t, engine, selectedProfile(), input)
	now = now.Add(11 * time.Minute)
	second := mustPrepare(t, engine, selectedProfile(), input)
	if first.Headers().Get("X-Tbh-Session-Id") != second.Headers().Get("X-Tbh-Session-Id") || first.Headers().Get("Traceparent") == second.Headers().Get("Traceparent") {
		t.Fatal("retry TTL did not expire independently")
	}
	now = now.Add(25 * time.Hour)
	third := mustPrepare(t, engine, selectedProfile(), input)
	if third.Headers().Get("X-Tbh-Session-Id") == second.Headers().Get("X-Tbh-Session-Id") {
		t.Fatal("conversation TTL did not expire")
	}
}

func TestProfileRequestStatePreservesRetryAcrossGlobalEvictionAndExpiry(t *testing.T) {
	engine := newProfileEngine()
	engine.sessions.capacity, engine.requests.capacity = 1, 1
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return now }
	input := fixtureInput("request", "conversation")
	input.State = &profileRequestState{}
	first := mustPrepare(t, engine, selectedProfile(), input)
	for i := range 3 {
		mustPrepare(t, engine, selectedProfile(), fixtureInput(fmt.Sprintf("other-%d", i), fmt.Sprintf("other-conversation-%d", i)))
	}
	now = now.Add(48 * time.Hour)
	retry := mustPrepare(t, engine, selectedProfile(), input)
	if !bytes.Equal(first.Body(), retry.Body()) || !reflect.DeepEqual(first.Headers(), retry.Headers()) {
		t.Fatal("active request plan was affected by global eviction or expiry")
	}
	input.State = &profileRequestState{}
	nextRequest := mustPrepare(t, engine, selectedProfile(), input)
	if first.Headers().Get("Traceparent") == nextRequest.Headers().Get("Traceparent") {
		t.Fatal("separate HTTP request states shared a retry plan")
	}
}

func TestProfileRequestStateIsConcurrentAndDoesNotLimitManagerAttempts(t *testing.T) {
	engine := newProfileEngine()
	input := fixtureInput("", "")
	input.State = &profileRequestState{}
	var wg sync.WaitGroup
	plans := make(chan *profilePlan, 16)
	errorsFound := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plan, err := engine.Prepare(selectedProfile(), input)
			if err != nil {
				errorsFound <- err
				return
			}
			plans <- plan
		}()
	}
	wg.Wait()
	close(errorsFound)
	close(plans)
	for err := range errorsFound {
		t.Fatal(err)
	}
	var first *profilePlan
	for plan := range plans {
		if first == nil {
			first = plan
		} else if !bytes.Equal(first.Body(), plan.Body()) || !reflect.DeepEqual(first.Headers(), plan.Headers()) {
			t.Fatal("request state generated conflicting concurrent retry plans")
		}
	}
	if len(input.State.templates) != 1 || len(engine.requests.entries) != 0 {
		t.Fatal("request state leaked into global request cache")
	}
	for i := range 96 {
		variant := input
		variant.CredentialID = fmt.Sprintf("credential-%d", i)
		mustPrepare(t, engine, selectedProfile(), variant)
	}
	retry := mustPrepare(t, engine, selectedProfile(), input)
	if !bytes.Equal(first.Body(), retry.Body()) || !reflect.DeepEqual(first.Headers(), retry.Headers()) {
		t.Fatal("new credential attempts evicted an active plan")
	}
}

func TestProfileRequestStateKeepsProfileSnapshotForRetry(t *testing.T) {
	data, err := museProfiles.ReadFile(selectedProfile().Profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	selection := ProfileSelection{Enabled: true, Profile: path, FillDefaults: true}
	input := fixtureInput("request", "conversation")
	input.State = &profileRequestState{}
	engine := newProfileEngine()
	first := mustPrepare(t, engine, selection, input)
	if err := os.WriteFile(path, []byte(`{"schema_version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	retry := mustPrepare(t, engine, selection, input)
	if !bytes.Equal(first.Body(), retry.Body()) || !reflect.DeepEqual(first.Headers(), retry.Headers()) {
		t.Fatal("profile reload altered an active retry")
	}
	input.State = &profileRequestState{}
	if _, err := engine.Prepare(selection, input); err == nil {
		t.Fatal("a new request did not validate its profile")
	}
}

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) {
	return 0, errors.New("private-fixture-entropy-error")
}

func TestProfileIdentityFailureDoesNotLeakOrLeavePartialState(t *testing.T) {
	for _, entropy := range []interface{ Read([]byte) (int, error) }{failingEntropy{}, bytes.NewReader(make([]byte, 128))} {
		engine := newProfileEngine()
		engine.entropy = entropy
		_, err := engine.Prepare(selectedProfile(), fixtureInput("request", "conversation"))
		var profileError *profileError
		if !errors.As(err, &profileError) || profileError.StatusCode() != 500 || strings.Contains(err.Error(), "private-fixture") {
			t.Fatalf("unsafe identity failure: %v", err)
		}
		if len(engine.sessions.entries) != 0 || len(engine.requests.entries) != 0 {
			t.Fatal("failed identity generation retained partial state")
		}
	}
}

func TestProfileInvalidBodiesAndProfileFailuresAreSanitized(t *testing.T) {
	for _, body := range []string{"", "null", "[]", `{"input":"one","input":"two"}`, `{"input":{"x":1,"x":2}}`, `{} {}`, `{"private-fixture-secret":`} {
		input := fixtureInput("request", "conversation")
		input.Body = []byte(body)
		_, err := newProfileEngine().Prepare(selectedProfile(), input)
		var profileError *profileError
		if !errors.As(err, &profileError) || profileError.StatusCode() != 400 || strings.Contains(err.Error(), "private-fixture-secret") {
			t.Fatalf("unexpected body error: %v", err)
		}
	}
	selection := selectedProfile()
	selection.Profile = filepath.Join(t.TempDir(), "private-fixture-secret.json")
	_, err := newProfileEngine().Prepare(selection, fixtureInput("request", "conversation"))
	var profileError *profileError
	if !errors.As(err, &profileError) || profileError.StatusCode() != 500 || strings.Contains(err.Error(), "private-fixture-secret") {
		t.Fatalf("profile path was exposed: %v", err)
	}
}

func TestProfileInvalidProfileDocumentsAreRejected(t *testing.T) {
	original, err := museProfiles.ReadFile(selectedProfile().Profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["schema_version"] = 2 },
		func(p map[string]any) { p["unexpected"] = true },
		func(p map[string]any) { p["static_headers"].(map[string]any)["Authorization"] = "fixture-secret" },
		func(p map[string]any) { p["static_headers"].(map[string]any)["User-Agent"] = "duplicate" },
		func(p map[string]any) { p["defaults"].(map[string]any)["tools"] = []any{} },
		func(p map[string]any) {
			p["static_headers"].(map[string]any)["user-agent"] = strings.Repeat("x", maxProfileBytes)
		},
	} {
		var document map[string]any
		if err := json.Unmarshal(original, &document); err != nil {
			t.Fatal(err)
		}
		mutate(document)
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "profile.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadProfile(path); err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("invalid profile accepted or disclosed: %v", err)
		}
	}
}
