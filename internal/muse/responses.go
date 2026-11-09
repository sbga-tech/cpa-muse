package muse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/sbga-tech/cpa-muse/internal/host"
	"github.com/tidwall/gjson"
)

// eventTransformer rewrites upstream Responses SSE events (data payloads).
// Implementations are per-request and stateful (apply_patch bridge, native output shaping).
type eventTransformer interface {
	// Reset discards state before an in-stream retry replays the request.
	Reset()
	// Transform maps one upstream event to zero or more downstream events.
	Transform(event []byte) ([][]byte, error)
	// Finish flushes buffered events when the upstream stream ends.
	Finish() ([][]byte, error)
	// TransformCompleted rewrites the terminal event of a non-streaming request.
	TransformCompleted(completed []byte) ([]byte, error)
}

// responsesPlan is one prepared upstream /responses request.
type responsesPlan struct {
	body []byte
	// headers builds the upstream headers for one attempt; token is the API key.
	headers func(token string) http.Header
	wire    *pluginapi.HTTPWireProfile
	// native requests keep Meta output as-is and treat a missing terminal event as truncation.
	native bool
	// decode means the plan asked for compressed responses (profile Accept-Encoding).
	decode bool
	// sseFraming emits full "event:/data:" SSE events (same-format Responses output);
	// otherwise each event is emitted as one "data: ..." line for host translation.
	sseFraming bool
	// nonStreamResponseObject returns response objects instead of terminal events.
	nonStreamResponseObject bool
	transforms              []eventTransformer
}

// planner prepares the request body/headers for one Responses mode.
type planner interface {
	plan(ctx context.Context, e *Executor, call *Call, cred Credential) (*responsesPlan, error)
	count(ctx context.Context, e *Executor, call *Call) (pluginapi.ExecutorResponse, error)
}

type responsesSurface struct {
	exec    *Executor
	planner planner
}

var (
	metaNotFoundRetryDelays = []time.Duration{300 * time.Millisecond, 900 * time.Millisecond}
	metaStreamRetryDelays   = []time.Duration{300 * time.Millisecond, 800 * time.Millisecond}
)

const (
	metaStreamBootstrapMaxBytes = 256 * 1024
	metaStreamRetryBudget       = 150 * time.Second
	scannerMaxBytes             = 50 << 20
	subscriptionUsageEvent      = "response.subscription_usage"
)

func (s *responsesSurface) count(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error) {
	return s.planner.count(ctx, s.exec, call)
}

// withSelectedAuth validates the one host-selected credential. CPA alone owns
// credential discovery, admission, refresh, and cross-credential failover.
func withSelectedAuth[T any](ctx context.Context, call *Call, eligible func(Credential) error, attempt func(*Credential) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	cred, err := call.Credential()
	if err != nil {
		return zero, err
	}
	if eligible != nil {
		if err := eligible(*cred); err != nil {
			return zero, err
		}
	}
	return attempt(cred)
}

func (s *responsesSurface) execute(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error) {
	return withSelectedAuth(ctx, call, nil, func(cred *Credential) (pluginapi.ExecutorResponse, error) {
		return s.executeOnce(ctx, call, cred)
	})
}

func (s *responsesSurface) executeOnce(ctx context.Context, call *Call, lease *Credential) (pluginapi.ExecutorResponse, error) {
	plan, err := s.planner.plan(ctx, s.exec, call, *lease)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	start := time.Now()
	for sseAttempt := 0; ; sseAttempt++ {
		if sseAttempt > 0 {
			resetTransforms(plan)
			if !sleepCtx(ctx, metaStreamRetryDelays[sseAttempt-1]) {
				return pluginapi.ExecutorResponse{}, ctx.Err()
			}
		}
		resp, err := s.do(ctx, call, lease, plan)
		if err != nil {
			return pluginapi.ExecutorResponse{}, err
		}
		payload, errCompleted := s.collectCompleted(call, lease, plan, resp.Body)
		if errCompleted != nil {
			if sseAttempt < len(metaStreamRetryDelays) && time.Since(start) < metaStreamRetryBudget && ctx.Err() == nil && isRetryableSSEBody(resp.Body) {
				call.Host.Log("warn", "muse: retrying non-stream request after SSE failure", map[string]any{"attempt": sseAttempt + 1, "error": errCompleted.Error()})
				continue
			}
			return pluginapi.ExecutorResponse{}, errCompleted
		}
		headers := cloneHeader(resp.Headers)
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")
		headers.Set("Content-Type", "application/json")
		return pluginapi.ExecutorResponse{Payload: payload, Headers: headers}, nil
	}
}

// do sends a buffered request with same-credential transient retries.
// A 401 is returned to CPA for provider refresh and same-auth replay.
func (s *responsesSurface) do(ctx context.Context, call *Call, lease *Credential, plan *responsesPlan) (*host.Response, error) {
	var resp *host.Response
	err := s.retryLoop(ctx, call, lease, func(token string) (int, []byte, func(), error) {
		r, err := call.Host.Do(ctx, s.request(lease, plan, token))
		if err != nil {
			return 0, nil, nil, err
		}
		body, errDecode := decodeBody(r.Headers, r.Body)
		if errDecode != nil {
			return 0, nil, nil, &StatusError{Status: http.StatusBadGateway, Code: "invalid_encoding", Message: errDecode.Error(), Scope: ScopeModel}
		}
		r.Body = body
		resp = r
		return r.StatusCode, body, func() {}, nil
	})
	return resp, err
}

func (s *responsesSurface) request(lease *Credential, plan *responsesPlan, token string) host.Request {
	return host.Request{
		Method:      http.MethodPost,
		URL:         strings.TrimSuffix(firstNonEmpty(lease.BaseURL, DefaultBaseURL), "/") + "/responses",
		Headers:     plan.headers(token),
		Body:        plan.body,
		WireProfile: plan.wire,
	}
}

// retryLoop implements metaDoWithRetry: one transport retry, one 502/503/504
// retry, and up to two 404 model_not_found retries. CPA handles 401 refresh.
// send returns the status, a small error body for non-2xx inspection and a
// discard function that releases a non-final response.
func (s *responsesSurface) retryLoop(ctx context.Context, call *Call, lease *Credential, send func(token string) (int, []byte, func(), error)) error {
	token := lease.APIKey
	transportRetried, gatewayRetried, notFoundRetries := false, false, 0
	for {
		status, body, discard, errSend := send(token)
		if errSend != nil {
			var se *StatusError
			if errors.As(errSend, &se) {
				return se
			}
			if !transportRetried && ctx.Err() == nil {
				transportRetried = true
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &StatusError{Status: http.StatusBadGateway, Code: "upstream_unreachable", Message: "meta upstream request failed: " + errSend.Error(), Scope: ScopeModel}
		}
		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
			if !gatewayRetried && ctx.Err() == nil {
				gatewayRetried = true
				discard()
				call.Host.Log("info", "muse: transient upstream status, retrying once", map[string]any{"status": status})
				continue
			}
		case status == http.StatusNotFound && bytes.Contains(body, []byte("model_not_found")) && notFoundRetries < len(metaNotFoundRetryDelays):
			delay := metaNotFoundRetryDelays[notFoundRetries]
			notFoundRetries++
			discard()
			call.Host.Log("info", "muse: upstream 404 model_not_found, retrying", map[string]any{"retry": notFoundRetries})
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
		discard()
		return upstreamError(status, body, time.Now())
	}
}

func (s *responsesSurface) open(ctx context.Context, call *Call) (*openStream, error) {
	return withSelectedAuth(ctx, call, nil, func(lease *Credential) (*openStream, error) {
		return s.openOnce(ctx, call, lease)
	})
}

func (s *responsesSurface) openStreamResponse(ctx context.Context, call *Call, lease *Credential, plan *responsesPlan) (*host.StreamResponse, error) {
	var opened *host.StreamResponse
	err := s.retryLoop(ctx, call, lease, func(token string) (int, []byte, func(), error) {
		r, err := call.Host.DoStream(ctx, s.request(lease, plan, token))
		if err != nil {
			return 0, nil, nil, err
		}
		body, errDecode := decodeStream(r.Headers, r.Body)
		if errDecode != nil {
			_ = r.Body.Close()
			return 0, nil, nil, &StatusError{Status: http.StatusBadGateway, Code: "invalid_encoding", Message: errDecode.Error(), Scope: ScopeModel}
		}
		r.Body = body
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			if plan.decode && !strings.Contains(strings.ToLower(r.Headers.Get("Content-Type")), "text/event-stream") {
				_ = r.Body.Close()
				return 0, nil, nil, &StatusError{Status: http.StatusBadGateway, Code: "cpa.meta.profile.invalid_stream", Message: "[cpa.meta.profile.invalid_stream] expected an upstream SSE stream", Scope: ScopeModel}
			}
			opened = r
			return r.StatusCode, nil, func() {}, nil
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		return r.StatusCode, data, func() {}, nil
	})
	return opened, err
}

func (s *responsesSurface) openOnce(ctx context.Context, call *Call, lease *Credential) (*openStream, error) {
	plan, err := s.planner.plan(ctx, s.exec, call, *lease)
	if err != nil {
		return nil, err
	}
	resp, err := s.openStreamResponse(ctx, call, lease, plan)
	if err != nil {
		return nil, err
	}
	headers := cloneHeader(resp.Headers)
	headers.Del("Content-Length")
	headers.Del("Content-Encoding")
	current := resp
	stream := &openStream{headers: headers}
	stream.release = func() {
		if current != nil {
			_ = current.Body.Close()
		}
	}
	stream.run = func(sink func([]byte) error) error {
		return s.pump(ctx, call, lease, plan, &current, sink)
	}
	return stream, nil
}

// pump ports the fork's ExecuteStream loop: subscription_usage filtering,
// pre-answer buffering with bounded in-stream retries, created/in_progress
// de-duplication across retries, and truncation detection for native plans.
func (s *responsesSurface) pump(ctx context.Context, call *Call, lease *Credential, plan *responsesPlan, current **host.StreamResponse, sink func([]byte) error) error {
	start := time.Now()
	createdEmitted, inProgressEmitted, answerCommitted := false, false, false

	type bufferedEvent struct {
		name string
		data []byte
	}

	for streamAttempt := 0; ; streamAttempt++ {
		if streamAttempt > 0 {
			resetTransforms(plan)
			if !sleepCtx(ctx, metaStreamRetryDelays[streamAttempt-1]) {
				return ctx.Err()
			}
			next, err := s.openStreamResponse(ctx, call, lease, plan)
			if err != nil {
				return err
			}
			*current = next
		}
		body := (*current).Body

		terminal := false
		emit := func(name string, data []byte) error {
			events, errTransform := applyTransforms(plan, data)
			for _, event := range events {
				typ := gjson.GetBytes(event, "type").String()
				if typ == "response.completed" || typ == "response.incomplete" {
					terminal = true
				}
				if errSink := sink(frameEvent(plan, firstNonEmpty(nameFor(name, data, event), typ), event)); errSink != nil {
					return errSink
				}
			}
			if errTransform != nil {
				return transformError(errTransform)
			}
			return nil
		}
		var pre []bufferedEvent
		preBytes := 0
		flush := func() error {
			for _, ev := range pre {
				if err := emit(ev.name, ev.data); err != nil {
					return err
				}
			}
			pre, preBytes = nil, 0
			return nil
		}

		retry := false
		scanner := bufio.NewScanner(body)
		scanner.Buffer(nil, scannerMaxBytes)
		var eventName string
		filtering := false
		var runErr error
	scan:
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				eventName, filtering = "", false
				continue
			}
			if bytes.HasPrefix(line, []byte("event:")) {
				eventName = string(bytes.TrimSpace(line[len("event:"):]))
				filtering = eventName == subscriptionUsageEvent
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(data, []byte("[DONE]")) {
				if plan.native && !terminal {
					_ = flush()
					runErr = &StatusError{Status: http.StatusBadGateway, Code: "stream_truncated", Message: "Meta Responses stream ended before a terminal response event"}
				}
				break scan
			}
			typ := gjson.GetBytes(data, "type").String()
			if isSubscriptionUsageEvent(typ, data) || (filtering && typ == "") {
				s.exec.quota.observe(*lease, data)
				continue
			}
			name := eventName
			eventName = ""
			if errEvent := streamEventError(data, time.Now()); errEvent != nil {
				if !answerCommitted && isRetryableSSEError(data) && streamAttempt < len(metaStreamRetryDelays) && time.Since(start) < metaStreamRetryBudget && ctx.Err() == nil {
					call.Host.Log("warn", "muse: retrying stream after pre-answer SSE failure", map[string]any{"attempt": streamAttempt + 1, "error": errEvent.Error()})
					retry = true
					break scan
				}
				_ = flush()
				runErr = errEvent
				break scan
			}
			switch typ {
			case "response.created":
				if createdEmitted {
					continue
				}
				createdEmitted = true
				if runErr = emit(name, data); runErr != nil {
					break scan
				}
				continue
			case "response.in_progress":
				if inProgressEmitted {
					continue
				}
				inProgressEmitted = true
				if runErr = emit(name, data); runErr != nil {
					break scan
				}
				continue
			}
			if !answerCommitted {
				if isPreAnswerBufferable(typ, data) && preBytes+len(data) <= metaStreamBootstrapMaxBytes {
					pre = append(pre, bufferedEvent{name: name, data: append([]byte(nil), data...)})
					preBytes += len(data)
					continue
				}
				answerCommitted = true
				if runErr = flush(); runErr != nil {
					break scan
				}
			}
			if runErr = emit(name, data); runErr != nil {
				break scan
			}
		}
		_ = body.Close()
		if retry {
			continue
		}
		if runErr != nil {
			return runErr
		}
		if err := flush(); err != nil {
			return err
		}
		tail, errFinish := finishTransforms(plan)
		for _, event := range tail {
			if err := sink(frameEvent(plan, gjson.GetBytes(event, "type").String(), event)); err != nil {
				return err
			}
		}
		if errFinish != nil {
			return transformError(errFinish)
		}
		if errScan := scanner.Err(); errScan != nil {
			if terminal && errors.Is(errScan, context.Canceled) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &StatusError{Status: http.StatusBadGateway, Code: "stream_read_failed", Message: errScan.Error()}
		}
		if plan.native && !terminal && ctx.Err() == nil {
			return &StatusError{Status: http.StatusBadGateway, Code: "stream_truncated", Message: "Meta Responses stream ended before a terminal response event"}
		}
		return nil
	}
}

// nameFor keeps the upstream event name only when the transform did not change the event type.
func nameFor(upstreamName string, original, transformed []byte) string {
	if upstreamName == "" {
		return ""
	}
	if gjson.GetBytes(original, "type").String() != gjson.GetBytes(transformed, "type").String() {
		return ""
	}
	return upstreamName
}

func frameEvent(plan *responsesPlan, name string, data []byte) []byte {
	if !plan.sseFraming {
		out := make([]byte, 0, len(data)+6)
		out = append(out, "data: "...)
		return append(out, data...)
	}
	out := make([]byte, 0, len(name)+len(data)+16)
	if name != "" {
		out = append(out, "event: "...)
		out = append(out, name...)
		out = append(out, '\n')
	}
	out = append(out, "data: "...)
	out = append(out, data...)
	return append(out, "\n\n"...)
}

// collectCompleted assembles a non-streaming answer from a buffered SSE body.
func (s *responsesSurface) collectCompleted(call *Call, lease *Credential, plan *responsesPlan, body []byte) ([]byte, error) {
	itemsByIndex := make(map[int64][]byte)
	var itemsFallback [][]byte
	filtering := false
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			filtering = false
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			filtering = string(bytes.TrimSpace(line[len("event:"):])) == subscriptionUsageEvent
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		typ := gjson.GetBytes(data, "type").String()
		if isSubscriptionUsageEvent(typ, data) || (filtering && typ == "") {
			s.exec.quota.observe(*lease, data)
			continue
		}
		if errEvent := streamEventError(data, time.Now()); errEvent != nil {
			return nil, errEvent
		}
		events, err := applyTransforms(plan, data)
		if err != nil {
			return nil, transformError(err)
		}
		for _, event := range events {
			switch gjson.GetBytes(event, "type").String() {
			case "response.output_item.done":
				if !plan.native {
					collectOutputItemDone(event, itemsByIndex, &itemsFallback)
				}
			case "response.completed", "response.incomplete":
				return s.finishCompleted(plan, event, itemsByIndex, itemsFallback)
			}
		}
	}
	if completed, ok := asCompletedEvent(body); ok {
		completed, err := completedTransforms(plan, completed)
		if err != nil {
			return nil, transformError(err)
		}
		return s.finishCompleted(plan, completed, itemsByIndex, itemsFallback)
	}
	if _, err := finishTransforms(plan); err != nil {
		return nil, transformError(err)
	}
	return nil, &StatusError{Status: http.StatusRequestTimeout, Code: "stream_truncated", Message: "meta stream error: stream disconnected before response.completed or response.incomplete", Scope: ScopeModel}
}

func (s *responsesSurface) finishCompleted(plan *responsesPlan, completed []byte, itemsByIndex map[int64][]byte, itemsFallback [][]byte) ([]byte, error) {
	if !plan.native {
		completed = patchCompletedOutput(completed, itemsByIndex, itemsFallback)
	}
	if plan.nonStreamResponseObject {
		response := gjson.GetBytes(completed, "response")
		if !response.IsObject() {
			return nil, &StatusError{Status: http.StatusBadGateway, Code: "invalid_response", Message: "Meta terminal event has no response object"}
		}
		return []byte(response.Raw), nil
	}
	return completed, nil
}

// asCompletedEvent accepts a bare JSON response body from a non-SSE upstream.
func asCompletedEvent(body []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !gjson.ValidBytes(trimmed) {
		return nil, false
	}
	root := gjson.ParseBytes(trimmed)
	switch root.Get("type").String() {
	case "response.completed", "response.incomplete":
		return trimmed, true
	}
	if root.Get("object").String() == "response" {
		status := root.Get("status").String()
		typ := "response.completed"
		if status == "incomplete" {
			typ = "response.incomplete"
		}
		out := make([]byte, 0, len(trimmed)+48)
		out = append(out, `{"type":"`...)
		out = append(out, typ...)
		out = append(out, `","response":`...)
		out = append(out, trimmed...)
		out = append(out, '}')
		return out, true
	}
	return nil, false
}

func isSubscriptionUsageEvent(eventType string, data []byte) bool {
	if eventType == subscriptionUsageEvent {
		return true
	}
	return eventType == "" && len(data) > 0 && gjson.GetBytes(data, "subscription").IsObject()
}

// isPreAnswerBufferable reports events that may be held back while an
// in-stream retry is still possible: lifecycle frames and empty reasoning.
// Reasoning with visible text is committed immediately.
func isPreAnswerBufferable(eventType string, data []byte) bool {
	switch eventType {
	case "response.created", "response.in_progress", "keepalive":
		return true
	case "response.output_item.added", "response.output_item.done":
		return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(data, "item.type").String()), "reasoning") && !reasoningHasVisibleText(data)
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return strings.TrimSpace(gjson.GetBytes(data, "part.text").String()) == ""
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done":
		return strings.TrimSpace(gjson.GetBytes(data, "delta").String()) == "" &&
			strings.TrimSpace(gjson.GetBytes(data, "text").String()) == ""
	default:
		return false
	}
}

func reasoningHasVisibleText(data []byte) bool {
	for _, path := range []string{"item.summary", "item.content"} {
		for _, part := range gjson.GetBytes(data, path).Array() {
			if strings.TrimSpace(part.Get("text").String()) != "" {
				return true
			}
		}
	}
	return false
}

// isRetryableSSEError matches Meta's transient in-stream failures, never quota.
func isRetryableSSEError(data []byte) bool {
	if len(data) == 0 || isSubscriptionQuota(http.StatusTooManyRequests, data) {
		return false
	}
	for _, path := range []string{"error.code", "response.error.code", "error.type", "response.error.type"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, path).String())) {
		case "server_error", "service_overloaded", "gateway_timeout", "internal_error", "overloaded_error":
			return true
		}
	}
	msg := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.error.message").String()))
	if msg == "" {
		msg = strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "error.message").String()))
	}
	return strings.Contains(msg, "failed to generate a response") || strings.Contains(msg, "temporarily overloaded")
}

func isRetryableSSEBody(body []byte) bool {
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if bytes.HasPrefix(line, []byte("data:")) && isRetryableSSEError(bytes.TrimSpace(line[len("data:"):])) {
			return true
		}
	}
	return false
}

// transformError keeps a transformer's own StatusError (e.g. the apply_patch
// bridge's request-scoped 502) and wraps anything else as a 502.
func transformError(err error) error {
	var se *StatusError
	if errors.As(err, &se) {
		return se
	}
	return &StatusError{Status: http.StatusBadGateway, Code: "transform_failed", Message: err.Error()}
}

// applyTransforms runs event through every stage. When a stage fails, the
// events it returned alongside the error still pass through the later stages
// and are returned with the error, so a failure frame reaches the client
// before the stream terminates.
func applyTransforms(plan *responsesPlan, event []byte) ([][]byte, error) {
	return runStages(plan.transforms, [][]byte{event})
}

func runStages(stages []eventTransformer, events [][]byte) ([][]byte, error) {
	for i, t := range stages {
		var next [][]byte
		for _, ev := range events {
			out, err := t.Transform(ev)
			next = append(next, out...)
			if err != nil {
				rest, _ := runStages(stages[i+1:], next)
				return rest, err
			}
		}
		events = next
	}
	return events, nil
}

// finishTransforms flushes each stage and pushes its tail through later stages.
func finishTransforms(plan *responsesPlan) ([][]byte, error) {
	var tail [][]byte
	for i, t := range plan.transforms {
		through, err := runStages(plan.transforms[i:i+1], tail)
		if err != nil {
			rest, _ := runStages(plan.transforms[i+1:], through)
			return rest, err
		}
		flushed, err := t.Finish()
		tail = append(through, flushed...)
		if err != nil {
			rest, _ := runStages(plan.transforms[i+1:], tail)
			return rest, err
		}
	}
	return tail, nil
}

func completedTransforms(plan *responsesPlan, completed []byte) ([]byte, error) {
	var err error
	for _, t := range plan.transforms {
		if completed, err = t.TransformCompleted(completed); err != nil {
			return nil, err
		}
	}
	return completed, nil
}

func resetTransforms(plan *responsesPlan) {
	for _, t := range plan.transforms {
		t.Reset()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return make(http.Header)
	}
	return h.Clone()
}
