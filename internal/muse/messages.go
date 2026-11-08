package muse

// Ported from the fork internal/runtime/executor/meta_messages_executor.go.
// The host has already applied payload rules; validation is our final semantic
// barrier. CPA supplies the selected auth and owns refresh and failover.
// Usage stays in the native payload.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DDD0s/cpa-muse/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type messagesSurface struct{ exec *Executor }

// Tracks Messages protocol availability without selecting another credential.
var metaMessagesCircuit = NewMetaMessagesCircuit(nil)

func (e *Executor) messagesPolicy(model string) MetaMessagesPolicy {
	p := MetaMessagesPolicy{Degrade: e.cfg.Messages.Degrade}
	p.Capabilities = append(p.Capabilities, e.cfg.Messages.Capabilities["*"]...)
	p.Capabilities = append(p.Capabilities, e.cfg.Messages.Capabilities[model]...)
	return p
}

func (e *Executor) prepareMessagesRequest(ctx context.Context, call *Call, count bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.cfg.MessagesMode() != "enabled" {
		return nil, MetaMessagesError(403, "disabled", "mode", "Meta Messages is disabled; explicit enabled mode is required")
	}
	switch strings.TrimSpace(e.cfg.Messages.CountTokens) {
	case "", "local", "upstream-then-local":
	default:
		return nil, MetaMessagesError(500, "invalid_config", "count-tokens", "expected local or upstream-then-local")
	}
	limit := e.cfg.Messages.MaxRequestBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	if int64(len(call.Req.Payload)) > limit {
		return nil, MetaMessagesError(413, "request_too_large", "body", "configured request byte limit exceeded")
	}
	if call.ThinkingLevel != "" || thinkingSuffix(call.Req.Model) != "" {
		return nil, MetaMessagesError(400, "unsupported", "model", "native Messages requires a plain model ID; set thinking or output_config.effort explicitly")
	}
	policy := e.messagesPolicy(call.Model)
	for _, beta := range strings.Split(MetaMessagesBetaHeader(call.Req.Headers), ",") {
		beta = strings.TrimSpace(beta)
		if beta != "" && !policy.Allows("beta."+beta) {
			return nil, MetaMessagesError(400, "capability_unverified", "anthropic-beta", "beta feature is not explicitly enabled: "+beta)
		}
	}
	root, err := DecodeMetaMessages(call.Req.Payload)
	if err != nil {
		return nil, err
	}
	if err = NormalizeMetaMessages(root, call.Model, call.Stream, count, policy); err != nil {
		return nil, err
	}
	if err = ValidateMetaMessages(root, count, policy); err != nil {
		return nil, err
	}
	body, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, MetaMessagesError(413, "request_too_large", "body", "final payload exceeds the configured request byte limit")
	}
	if MetaMessagesHasURLImage(body) {
		if _, err = MetaMessagesURLImageIdleTimeout(e.cfg.Messages.URLImageIdleTimeoutSeconds); err != nil {
			return nil, err
		}
	}
	if !count && e.cfg.Messages.ContextLimit > 0 {
		estimate, err := EstimateMetaMessagesTokens(body)
		if err != nil {
			return nil, err
		}
		maxTokens := gjson.GetBytes(body, "max_tokens").Int()
		if maxTokens >= int64(e.cfg.Messages.ContextLimit) || estimate > int64(e.cfg.Messages.ContextLimit)-maxTokens {
			return nil, MetaMessagesError(400, "estimated_context_overflow", "messages", "local approximate token estimate plus max_tokens exceeds the configured context limit")
		}
	}
	return body, nil
}

func (s *messagesSurface) selection(count bool) (func(Credential) error, map[string]*MetaMessagesTicket) {
	tickets := make(map[string]*MetaMessagesTicket)
	return func(cred Credential) error {
		if cred.Subscription() && !s.exec.cfg.Messages.AllowSubscription {
			return MetaMessagesError(403, "subscription_opt_in", "allow-subscription", "subscription credentials require explicit experimental opt-in")
		}
		if !count {
			ticket, retry := metaMessagesCircuit.Acquire(cred.ID)
			if ticket == nil {
				err := MetaMessagesError(503, "surface_unavailable", "messages", "this credential's Messages circuit is open").(*StatusError)
				err.RetryAfter = retry + time.Second
				return err
			}
			tickets[cred.ID] = ticket
		}
		return nil
	}, tickets
}

func messagesClassify(err error) (error, MetaMessagesOutcome) {
	if err == nil {
		return nil, MetaMessagesSuccess
	}
	if IsMetaMessagesURLImageTimeout(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err, MetaMessagesNeutral
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return messagesGatewayError("transport_error", err.Error()), MetaMessagesTransient
	}
	// A 401 must reach CPA for refresh and same-auth replay, not open a circuit.
	if se.Status == http.StatusUnauthorized {
		return err, MetaMessagesNeutral
	}
	outcome := MetaMessagesNeutral
	if se.Status == 402 || se.Status == 403 {
		outcome = MetaMessagesDenied
	} else if se.Status >= 500 {
		outcome = MetaMessagesTransient
	}
	if outcome != MetaMessagesNeutral {
		copy := *se
		copy.Scope = ScopeRequest
		return &copy, outcome
	}
	return err, outcome
}

func messagesComplete(ticket *MetaMessagesTicket, err error) error {
	classified, outcome := messagesClassify(err)
	ticket.Complete(outcome)
	return classified
}

func (s *messagesSurface) execute(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error) {
	body, err := s.exec.prepareMessagesRequest(ctx, call, false)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	eligible, tickets := s.selection(false)
	return withSelectedAuth(ctx, call, eligible, func(lease *Credential) (out pluginapi.ExecutorResponse, err error) {
		defer func() { err = messagesComplete(tickets[lease.ID], err) }()
		response, err := s.request(ctx, call, lease, body, false, false)
		if err != nil {
			return out, err
		}
		defer response.Body.Close()
		data, err := readMetaMessagesBody(response.Body)
		if err != nil {
			s.logMetering(call, nil, err)
			return out, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return out, messagesUpstreamError(response.StatusCode, response.Headers, data)
		}
		root, err := DecodeMetaMessages(data)
		if err != nil || root["type"] != "message" {
			return out, messagesGatewayError("invalid_response", "expected an Anthropic message")
		}
		if root["role"] != "assistant" || gjson.GetBytes(data, "id").Type != gjson.String || gjson.GetBytes(data, "model").Type != gjson.String {
			return out, messagesGatewayError("invalid_response", "message identity or assistant role is missing")
		}
		content, ok := root["content"].([]any)
		if !ok {
			return out, messagesGatewayError("invalid_response", "message content is missing")
		}
		for _, value := range content {
			block, ok := value.(map[string]any)
			if !ok {
				return out, messagesGatewayError("unmapped_block", "content block is not an object")
			}
			if err := ValidateMetaMessagesResponseBlock(block, s.exec.messagesPolicy(call.Model), false); err != nil {
				return out, messagesGatewayError("unmapped_block", "upstream emitted an unsupported content block")
			}
		}
		var metering MetaMessagesMetering
		metering.Observe(data, true)
		s.logMetering(call, &metering, nil)
		if alias := messagesAlias(call); alias != "" {
			data, _ = sjson.SetBytes(data, "model", alias)
		}
		headers := MetaMessagesDownstreamHeaders(response.Headers)
		headers.Set("X-CPA-Metering", metering.Status())
		return pluginapi.ExecutorResponse{Payload: data, Headers: headers}, nil
	})
}

func (s *messagesSurface) count(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error) {
	body, err := s.exec.prepareMessagesRequest(ctx, call, true)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if strings.TrimSpace(s.exec.cfg.Messages.CountTokens) != "upstream-then-local" {
		return countMessagesLocal(body)
	}
	eligible, _ := s.selection(true)
	return withSelectedAuth(ctx, call, eligible, func(lease *Credential) (out pluginapi.ExecutorResponse, err error) {
		defer func() { err = messagesComplete(nil, err) }()
		response, err := s.request(ctx, call, lease, body, false, true)
		if err != nil {
			return out, err
		}
		defer response.Body.Close()
		data, err := readMetaMessagesBody(response.Body)
		if err != nil {
			return out, err
		}
		if response.StatusCode == 402 {
			return countMessagesLocal(body)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return out, messagesUpstreamError(response.StatusCode, response.Headers, data)
		}
		root, err := DecodeMetaMessages(data)
		if err != nil {
			return out, messagesGatewayError("invalid_count", "upstream count response is not JSON")
		}
		count, ok := root["input_tokens"].(json.Number)
		if !ok {
			return out, messagesGatewayError("invalid_count", "upstream count response omitted input_tokens")
		}
		n, err := count.Int64()
		if err != nil || n < 0 {
			return out, messagesGatewayError("invalid_count", "upstream count response contains an invalid input_tokens")
		}
		headers := MetaMessagesDownstreamHeaders(response.Headers)
		headers.Set("X-CPA-Token-Count", "upstream")
		return pluginapi.ExecutorResponse{Payload: data, Headers: headers}, nil
	})
}

func countMessagesLocal(body []byte) (pluginapi.ExecutorResponse, error) {
	estimate, err := EstimateMetaMessagesTokenDetails(body)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-CPA-Token-Count", "approximate-local")
	headers.Set("X-CPA-Token-Count-Method", MetaMessagesTokenEstimateMethod)
	headers.Set("X-CPA-Token-Count-Uncertainty", estimate.Uncertainty())
	return pluginapi.ExecutorResponse{Payload: []byte(fmt.Sprintf(`{"input_tokens":%d}`, estimate.InputTokens)), Headers: headers}, nil
}

// request deliberately does not use the Responses retry loop: the fork makes
// one Messages request, with only the conductor's single refresh after 401.
func (s *messagesSurface) request(ctx context.Context, call *Call, lease *Credential, body []byte, stream, count bool) (*host.StreamResponse, error) {
	token := lease.APIKey
	var err error
	if strings.HasPrefix(token, "LLM|") && !s.exec.cfg.Messages.AllowSubscription {
		return nil, MetaMessagesError(403, "subscription_opt_in", "allow-subscription", "subscription credentials require explicit experimental opt-in")
	}
	path := "/messages"
	if count {
		path += "/count_tokens"
	}
	req := host.Request{Method: http.MethodPost, URL: strings.TrimSuffix(firstNonEmpty(lease.BaseURL, DefaultBaseURL), "/") + path, Body: body}
	hreq := &http.Request{}
	ApplyMetaMessagesHeaders(hreq, token, stream, call.Req.Headers)
	req.Headers = hreq.Header
	if beta := MetaMessagesBetaHeader(call.Req.Headers); beta != "" {
		req.Headers.Set("Anthropic-Beta", beta)
	}
	var watch *MetaMessagesURLImageWatchdog
	upstreamCtx := ctx
	if MetaMessagesHasURLImage(body) {
		idle, err := MetaMessagesURLImageIdleTimeout(s.exec.cfg.Messages.URLImageIdleTimeoutSeconds)
		if err != nil {
			return nil, err
		}
		watch = NewMetaMessagesURLImageWatchdog(ctx, idle)
		if watch != nil {
			upstreamCtx = watch.Context()
		}
	}
	var response *host.StreamResponse
	// URL media needs incremental reads even for a nonstream response so real
	// byte progress can refresh the watchdog and cancellation closes its body.
	if stream || watch != nil {
		response, err = call.Host.DoStream(upstreamCtx, req)
	} else {
		var buffered *host.Response
		buffered, err = call.Host.Do(upstreamCtx, req)
		if err == nil {
			response = &host.StreamResponse{StatusCode: buffered.StatusCode, Headers: buffered.Headers, Body: io.NopCloser(bytes.NewReader(buffered.Body))}
		}
	}
	if watch != nil {
		err = watch.Error(err)
		if err != nil {
			watch.Stop()
			if response != nil {
				response.Body.Close()
			}
		} else {
			response.Body = watch.WrapBody(response.Body, stream)
		}
	}
	if err != nil {
		if MetaMessagesHasURLImage(body) && !count {
			s.logMetering(call, nil, err)
		}
		return nil, err
	}
	return response, nil
}

func messagesAlias(call *Call) string {
	return firstNonEmpty(gjson.GetBytes(call.Req.OriginalRequest, "model").String(), call.Req.Model)
}

func (s *messagesSurface) logMetering(call *Call, metering *MetaMessagesMetering, terminal error) {
	if metering == nil {
		metering = &MetaMessagesMetering{}
	}
	fields := metering.logFields()
	fields["provider"], fields["surface"], fields["model"] = "meta", "messages", call.Model
	fields["failed"], fields["canceled"] = terminal != nil, errors.Is(terminal, context.Canceled)
	if IsMetaMessagesURLImageTimeout(terminal) {
		fields["timeout_kind"] = "url_image_idle"
	}
	// The host callback carries the request ID; no internal logging import needed.
	call.Host.Log("info", "Meta Messages metering", fields)
}

func readMetaMessagesBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, 64<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<20 {
		return nil, messagesGatewayError("response_too_large", "upstream response exceeds 64 MiB")
	}
	return data, nil
}
func messagesGatewayError(code, reason string) error {
	return MetaMessagesError(502, code, "upstream", reason)
}

func messagesUpstreamError(code int, headers http.Header, body []byte) error {
	// Classify the original body before synthesizing the public Anthropic shape:
	// quota/model identifiers in a non-Anthropic Meta response still matter.
	se := upstreamError(code, body, time.Now())
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "error.type").Exists() {
		typ := "api_error"
		switch code {
		case 400:
			typ = "invalid_request_error"
		case 401:
			typ = "authentication_error"
		case 402, 403:
			typ = "permission_error"
		case 404:
			typ = "not_found_error"
		case 429:
			typ = "rate_limit_error"
		case 529:
			typ = "overloaded_error"
		}
		body, _ = json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": fmt.Sprintf("Meta Messages returned HTTP %d", code)}})
	}
	se.Body = body
	se.Message = ""
	if seconds, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && seconds >= 0 {
		se.RetryAfter = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(headers.Get("Retry-After")); err == nil && time.Until(at) > 0 {
		se.RetryAfter = time.Until(at)
	}
	return se
}
func messagesStreamError(body []byte) error {
	code := 502
	switch gjson.GetBytes(body, "error.type").String() {
	case "invalid_request_error":
		code = 400
	case "authentication_error":
		code = 401
	case "permission_error":
		code = 403
	case "not_found_error":
		code = 404
	case "rate_limit_error":
		code = 429
	case "overloaded_error":
		code = 529
	}
	return messagesUpstreamError(code, nil, body)
}

func messagesEventIndex(root map[string]any) (int64, bool) {
	value, ok := root["index"].(json.Number)
	if !ok {
		return 0, false
	}
	index, err := value.Int64()
	return index, err == nil && index >= 0
}

func (s *messagesSurface) open(ctx context.Context, call *Call) (*openStream, error) {
	body, err := s.exec.prepareMessagesRequest(ctx, call, false)
	if err != nil {
		return nil, err
	}
	eligible, tickets := s.selection(false)
	return withSelectedAuth(ctx, call, eligible, func(lease *Credential) (*openStream, error) {
		response, err := s.request(ctx, call, lease, body, true, false)
		fail := func(err error) (*openStream, error) {
			if response != nil {
				response.Body.Close()
			}
			return nil, messagesComplete(tickets[lease.ID], err)
		}
		if err != nil {
			return fail(err)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			data, err := readMetaMessagesBody(response.Body)
			if err != nil {
				return fail(err)
			}
			return fail(messagesUpstreamError(response.StatusCode, response.Headers, data))
		}
		contentType, _, _ := mime.ParseMediaType(response.Headers.Get("Content-Type"))
		if contentType != "text/event-stream" {
			return fail(messagesGatewayError("invalid_stream", "upstream did not return text/event-stream"))
		}
		var terminal error = context.Canceled
		stream := &openStream{headers: MetaMessagesDownstreamHeaders(response.Headers)}
		stream.release = func() { response.Body.Close(); messagesComplete(tickets[lease.ID], terminal) }
		stream.run = func(sink func([]byte) error) (err error) {
			stop := context.AfterFunc(ctx, func() { response.Body.Close() })
			defer stop()
			defer stream.close()
			var metering MetaMessagesMetering
			defer func() { terminal = err; s.logMetering(call, &metering, err) }()
			return s.pumpMessages(ctx, call, response.Body, &metering, sink)
		}
		return stream, nil
	})
}
