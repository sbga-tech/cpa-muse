package muse

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type loginReply struct {
	status int
	body   string
}

// loginFixture scripts Meta's device, token, and mint endpoints behind a fake host client.
type loginFixture struct {
	t        *testing.T
	clock    time.Time
	device   string
	tokens   []loginReply
	mint     loginReply
	requests []pluginapi.HTTPRequest
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	f := &loginFixture{
		t:      t,
		clock:  time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		device: `{"device_code":"device-secret","user_code":"ABCD-EFGH","verification_uri":"https://auth.invalid/oauth/device/","verification_uri_complete":"https://auth.invalid/oauth/device/?code=ABCD-EFGH","expires_in":600,"interval":5}`,
		mint:   loginReply{200, `{"api_key":"minted-test-only","base_url":"https://api.invalid/v1","user_email":"user@example.invalid","is_subs_active":true}`},
	}
	previousNow, previous := logins.now, logins.byState
	logins.now = func() time.Time { return f.clock }
	logins.byState = make(map[string]*deviceLogin)
	t.Cleanup(func() { logins.now, logins.byState = previousNow, previous })
	return f
}

func (f *loginFixture) provider() AuthProvider {
	return AuthProvider{AuthURL: "https://auth.invalid/", MintURL: "https://mint.invalid/key"}
}

func (f *loginFixture) client() pluginapi.HostHTTPClient {
	return authProviderClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		f.requests = append(f.requests, req)
		if req.Headers.Get("User-Agent") != authFlowUserAgent {
			f.t.Errorf("%s sent without the Muse Code authflow identity: %v", req.URL, req.Headers)
		}
		reply := func(r loginReply) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: r.status, Body: []byte(r.body)}, nil
		}
		form, _ := url.ParseQuery(string(req.Body))
		switch req.URL {
		case "https://auth.invalid/oidc/device/authorization/":
			if form.Get("client_id") != metaOAuthClientID {
				f.t.Errorf("device authorization form: %v", form)
			}
			return reply(loginReply{200, f.device})
		case "https://auth.invalid/oidc/device/token/":
			if form.Get("grant_type") != deviceCodeGrantType || form.Get("device_code") != "device-secret" || form.Get("client_id") != metaOAuthClientID {
				f.t.Errorf("token form: %v", form)
			}
			if len(f.tokens) == 0 {
				f.t.Fatal("unexpected token poll")
			}
			next := f.tokens[0]
			f.tokens = f.tokens[1:]
			return reply(next)
		case "https://mint.invalid/key":
			if req.Headers.Get("Authorization") != "Bearer dca:issued-test-only" || string(req.Body) != `{"onboard":false}` {
				f.t.Errorf("mint request: %v %s", req.Headers, req.Body)
			}
			return reply(f.mint)
		}
		f.t.Fatalf("unexpected request %s", req.URL)
		return pluginapi.HTTPResponse{}, nil
	}}
}

func (f *loginFixture) start() string {
	f.t.Helper()
	resp, err := f.provider().StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{HTTPClient: f.client()})
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.State
}

func (f *loginFixture) poll(state string) pluginapi.AuthLoginPollResponse {
	f.t.Helper()
	resp, err := f.provider().PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: state, HTTPClient: f.client()})
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *loginFixture) tokenPolls() int {
	count := 0
	for _, req := range f.requests {
		if strings.HasSuffix(req.URL, "/oidc/device/token/") {
			count++
		}
	}
	return count
}

func TestDeviceLoginPacesPollsAndIssuesMuseCredential(t *testing.T) {
	f := newLoginFixture(t)
	f.tokens = []loginReply{
		{400, `{"error":"authorization_pending"}`},
		{400, `{"error":"slow_down"}`},
		{200, `{"access_token":"dca:issued-test-only","token_type":"bearer","expires_in":3600}`},
	}
	start, err := f.provider().StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{HTTPClient: f.client()})
	if err != nil {
		t.Fatal(err)
	}
	if start.URL != "https://auth.invalid/oauth/device/?code=ABCD-EFGH" || !start.ExpiresAt.Equal(f.clock.Add(600*time.Second)) || len(start.Metadata) != 0 {
		t.Fatalf("start response: %+v", start)
	}
	if !strings.HasPrefix(start.State, "muse-") || strings.ContainsAny(start.State, "/\\") || strings.Contains(start.State, "device-secret") {
		t.Fatalf("state is not an opaque CPA session state: %q", start.State)
	}

	steps := []struct {
		advance    time.Duration
		wantPolls  int
		wantStatus pluginapi.AuthLoginStatus
	}{
		{0, 0, pluginapi.AuthLoginStatusPending},               // before the first interval
		{5 * time.Second, 1, pluginapi.AuthLoginStatusPending}, // authorization_pending
		{4 * time.Second, 1, pluginapi.AuthLoginStatusPending}, // caller polls early
		{1 * time.Second, 2, pluginapi.AuthLoginStatusPending}, // slow_down: interval becomes 10s
		{5 * time.Second, 2, pluginapi.AuthLoginStatusPending}, // still inside the widened interval
		{5 * time.Second, 3, pluginapi.AuthLoginStatusSuccess},
	}
	var done pluginapi.AuthLoginPollResponse
	for i, step := range steps {
		f.clock = f.clock.Add(step.advance)
		done = f.poll(start.State)
		if done.Status != step.wantStatus || f.tokenPolls() != step.wantPolls {
			t.Fatalf("step %d: status=%s token polls=%d, want %s/%d (%+v)", i, done.Status, f.tokenPolls(), step.wantStatus, step.wantPolls, done)
		}
	}

	auth := done.Auth
	if auth.Provider != "muse" || auth.Label != "user@example.invalid" || !strings.HasPrefix(auth.FileName, "muse-user_example.invalid-") || !strings.HasSuffix(auth.FileName, ".json") {
		t.Fatalf("login credential identity: %+v", auth)
	}
	storage := auth.StorageJSON
	for path, want := range map[string]string{
		"type": "muse", "auth_kind": "oauth", "api_key": "minted-test-only", "access_token": "minted-test-only",
		"dca_token": "dca:issued-test-only", "refresh_token": "dca:issued-test-only", "base_url": "https://api.invalid/v1", "email": "user@example.invalid",
	} {
		if got := gjson.GetBytes(storage, path).String(); got != want {
			t.Fatalf("storage %s=%q want %q: %s", path, got, want, storage)
		}
	}
	if got := gjson.GetBytes(storage, "dca_expires_at").Int(); got != f.clock.Add(time.Hour).Unix() {
		t.Fatalf("dca_expires_at=%d", got)
	}
	parsed, err := (AuthProvider{}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: storage, FileName: auth.FileName})
	if err != nil || !parsed.Handled {
		t.Fatalf("login output is not a loadable muse credential: %+v %v", parsed, err)
	}
	cred, err := selectedCredential(pluginapi.ExecutorRequest{AuthID: auth.FileName, AuthProvider: "muse", HTTPClient: f.client(), StorageJSON: parsed.Auth.StorageJSON})
	if err != nil || cred.APIKey != "minted-test-only" {
		t.Fatalf("login output is not executable: %+v %v", cred, err)
	}
	if again := f.poll(start.State); again.Status != pluginapi.AuthLoginStatusError || f.tokenPolls() != 3 {
		t.Fatalf("completed login state was reusable: %+v", again)
	}
}

func TestDeviceLoginTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		advance     time.Duration
		token       *loginReply
		mint        *loginReply
		wantStatus  pluginapi.AuthLoginStatus
		wantMessage string
		wantAPIKey  string
	}{
		{name: "denied", advance: 5 * time.Second, token: &loginReply{400, `{"error":"access_denied"}`}, wantStatus: pluginapi.AuthLoginStatusError, wantMessage: "denied"},
		{name: "device code expired", advance: 5 * time.Second, token: &loginReply{400, `{"error":"expired_token"}`}, wantStatus: pluginapi.AuthLoginStatusError, wantMessage: "expired"},
		{name: "lifetime elapsed without polling Meta", advance: 600 * time.Second, wantStatus: pluginapi.AuthLoginStatusError, wantMessage: "expired"},
		{name: "account refused a key", advance: 5 * time.Second, token: &loginReply{200, `{"access_token":"dca:issued-test-only"}`}, mint: &loginReply{403, `{"api_key":"must-not-leak"}`}, wantStatus: pluginapi.AuthLoginStatusError, wantMessage: "Model API key"},
		{name: "transient mint failure keeps DCA token", advance: 5 * time.Second, token: &loginReply{200, `{"access_token":"dca:issued-test-only"}`}, mint: &loginReply{502, `{}`}, wantStatus: pluginapi.AuthLoginStatusSuccess, wantMessage: "first use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			if tc.token != nil {
				f.tokens = []loginReply{*tc.token}
			}
			if tc.mint != nil {
				f.mint = *tc.mint
			}
			state := f.start()
			f.clock = f.clock.Add(tc.advance)
			resp := f.poll(state)
			if resp.Status != tc.wantStatus || !strings.Contains(resp.Message, tc.wantMessage) || strings.Contains(resp.Message, "must-not-leak") {
				t.Fatalf("poll: %+v", resp)
			}
			if tc.token == nil && f.tokenPolls() != 0 {
				t.Fatal("expired login still polled Meta")
			}
			if resp.Status == pluginapi.AuthLoginStatusSuccess {
				storage := resp.Auth.StorageJSON
				if gjson.GetBytes(storage, "api_key").Exists() || gjson.GetBytes(storage, "refresh_token").String() != "dca:issued-test-only" {
					t.Fatalf("DCA-only credential: %s", storage)
				}
			}
			if again := f.poll(state); again.Status != pluginapi.AuthLoginStatusError || !strings.Contains(again.Message, "unknown") {
				t.Fatalf("terminal login state was retained: %+v", again)
			}
		})
	}
}

func TestDeviceLoginBoundsPendingLogins(t *testing.T) {
	f := newLoginFixture(t)
	for i := 0; i < maxPendingLogins; i++ {
		f.start()
	}
	if _, err := f.provider().StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{HTTPClient: f.client()}); err == nil {
		t.Fatal("pending logins grew past the bound")
	}
	f.clock = f.clock.Add(600 * time.Second)
	f.start() // expired logins are purged
}
