// Package host wraps the CPA host callbacks available to a dynamic plugin.
//
// Every call carries the host callback id of the plugin call that triggered it,
// so the host can resolve request-scoped state (request logging, proxy policy,
// cancellation) for the right downstream request.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Transport performs one raw host callback and returns the host envelope bytes.
type Transport func(method string, request []byte) ([]byte, error)

var transport = struct {
	sync.RWMutex
	fn Transport
}{}

// SetTransport installs the host callback transport. Nil disables callbacks.
func SetTransport(fn Transport) {
	transport.Lock()
	transport.fn = fn
	transport.Unlock()
}

// Error is a host callback failure that preserves the host-reported HTTP status.
type Error struct {
	Method     string
	Code       string
	Message    string
	HTTPStatus int
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("host %s: %s", e.Method, e.Message)
	}
	return fmt.Sprintf("host %s failed", e.Method)
}

// StatusCode reports the HTTP status the host attached to the failure.
func (e *Error) StatusCode() int { return e.HTTPStatus }

// Call invokes a host callback and decodes its result into T.
func Call[T any](method string, request any) (T, error) {
	var zero T
	transport.RLock()
	fn := transport.fn
	transport.RUnlock()
	if fn == nil {
		return zero, &Error{Method: method, Message: "host callback is unavailable"}
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return zero, fmt.Errorf("encode host %s request: %w", method, errMarshal)
	}
	resp, errCall := fn(method, raw)
	if errCall != nil {
		return zero, &Error{Method: method, Message: errCall.Error()}
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(resp, &envelope); errDecode != nil {
		return zero, fmt.Errorf("decode host %s envelope: %w", method, errDecode)
	}
	if !envelope.OK {
		herr := &Error{Method: method, Message: "callback failed"}
		if envelope.Error != nil {
			herr.Code = envelope.Error.Code
			herr.Message = envelope.Error.Message
			herr.HTTPStatus = envelope.Error.HTTPStatus
		}
		return zero, herr
	}
	var out T
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return out, nil
	}
	if errDecode := json.Unmarshal(envelope.Result, &out); errDecode != nil {
		return zero, fmt.Errorf("decode host %s result: %w", method, errDecode)
	}
	return out, nil
}

type empty struct{}

// Client issues host callbacks on behalf of one plugin call.
type Client struct {
	CallbackID string
	Upstream   pluginapi.HostHTTPClient
}

// NewClient binds callbacks to the host callback id of the active plugin call.
func NewClient(callbackID string) Client { return Client{CallbackID: strings.TrimSpace(callbackID)} }

// NewHTTPClient binds execution to the client supplied with the selected auth.
func NewHTTPClient(client pluginapi.HostHTTPClient, callbackID string) Client {
	if callback, ok := client.(callbackHTTPClient); ok {
		return callback.client
	}
	return Client{CallbackID: strings.TrimSpace(callbackID), Upstream: client}
}

type httpRequest struct {
	HostCallbackID string                     `json:"host_callback_id,omitempty"`
	OperationID    string                     `json:"operation_id,omitempty"`
	Method         string                     `json:"method,omitempty"`
	URL            string                     `json:"url,omitempty"`
	Headers        http.Header                `json:"headers,omitempty"`
	Body           []byte                     `json:"body,omitempty"`
	WireProfile    *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
}

// Request describes one upstream HTTP request sent through the host transport.
type Request struct {
	Method      string
	URL         string
	Headers     http.Header
	Body        []byte
	WireProfile *pluginapi.HTTPWireProfile
}

func (c Client) wire(req Request, operationID string) httpRequest {
	return httpRequest{
		HostCallbackID: c.CallbackID,
		OperationID:    operationID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
		WireProfile:    req.WireProfile,
	}
}

type operationOpenRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type operationOpenResponse struct {
	OperationID string `json:"operation_id"`
}

type operationCancelRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id"`
}

// operation ties a host HTTP call to ctx: when ctx ends, the host cancels the
// in-flight request (host.http.cancel), which also unblocks pending stream reads.
// Without a callback id the operation is plugin-scoped: still cancellable, but
// not attributed to any downstream request.
type operation struct {
	id       string
	done     chan struct{}
	stopOnce sync.Once
}

func (c Client) startOperation(ctx context.Context) *operation {
	op := &operation{done: make(chan struct{})}
	if ctx == nil || ctx.Done() == nil {
		return op
	}
	resp, err := Call[operationOpenResponse](pluginabi.MethodHostHTTPOperationOpen, operationOpenRequest{HostCallbackID: c.CallbackID})
	if err != nil || strings.TrimSpace(resp.OperationID) == "" {
		// Older hosts without operations: the call still works, only without
		// mid-flight cancellation.
		return op
	}
	op.id = resp.OperationID
	go func() {
		select {
		case <-ctx.Done():
			c.cancelOperation(op.id)
		case <-op.done:
		}
	}()
	return op
}

func (c Client) cancelOperation(id string) {
	if id == "" {
		return
	}
	_, _ = Call[empty](pluginabi.MethodHostHTTPCancel, operationCancelRequest{HostCallbackID: c.CallbackID, OperationID: id})
}

// stop ends the ctx watcher without cancelling the request.
func (op *operation) stop() {
	op.stopOnce.Do(func() { close(op.done) })
}

// Response is a buffered upstream HTTP response.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Do sends a request and buffers the whole response body. Cancelling ctx
// aborts the in-flight host request.
func (c Client) Do(ctx context.Context, req Request) (*Response, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if c.Upstream != nil {
		resp, err := c.Upstream.Do(ctx, pluginapi.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body, WireProfile: req.WireProfile})
		if err != nil {
			return nil, err
		}
		return &Response{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
	}
	op := c.startOperation(ctx)
	defer op.stop()
	resp, err := Call[pluginapi.HTTPResponse](pluginabi.MethodHostHTTPDo, c.wire(req, op.id))
	if err != nil {
		if ctxErr(ctx) != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

type streamOpenResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
	Chunks     []struct {
		Payload []byte
	} `json:"chunks,omitempty"`
}

type streamIDRequest struct {
	StreamID string `json:"stream_id"`
}

type streamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// StreamResponse is an upstream response whose body is read incrementally.
type StreamResponse struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
}

// DoStream opens a streaming request. The caller must close Body. Cancelling
// ctx aborts the request and any blocked Body read.
func (c Client) DoStream(ctx context.Context, req Request) (*StreamResponse, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if c.Upstream != nil {
		streamCtx, cancel := context.WithCancel(ctx)
		resp, err := c.Upstream.DoStream(streamCtx, pluginapi.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body, WireProfile: req.WireProfile})
		if err != nil {
			cancel()
			return nil, err
		}
		return &StreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: &channelBody{ctx: streamCtx, cancel: cancel, chunks: resp.Chunks}}, nil
	}
	op := c.startOperation(ctx)
	resp, err := Call[streamOpenResponse](pluginabi.MethodHostHTTPDoStream, c.wire(req, op.id))
	if err != nil {
		op.stop()
		if ctxErr(ctx) != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	body := &streamBody{ctx: ctx, id: resp.StreamID, op: op, client: c}
	for _, chunk := range resp.Chunks {
		body.pending = append(body.pending, chunk.Payload...)
	}
	if resp.StreamID == "" {
		body.done = true
	}
	return &StreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: body}, nil
}

// streamBody reads a host HTTP stream. The mutex guards state only; host
// reads run unlocked so Close can cancel a blocked Read.
type streamBody struct {
	ctx     context.Context
	id      string
	op      *operation
	client  Client
	mu      sync.Mutex
	pending []byte
	done    bool
	err     error
	closed  bool
}

func (b *streamBody) Read(p []byte) (int, error) {
	for {
		b.mu.Lock()
		if len(b.pending) > 0 {
			n := copy(p, b.pending)
			b.pending = b.pending[n:]
			b.mu.Unlock()
			return n, nil
		}
		switch {
		case b.closed:
			b.mu.Unlock()
			return 0, io.ErrClosedPipe
		case b.err != nil:
			err := b.err
			b.mu.Unlock()
			return 0, err
		case b.done:
			b.mu.Unlock()
			return 0, io.EOF
		}
		id := b.id
		b.mu.Unlock()
		if err := ctxErr(b.ctx); err != nil {
			b.fail(err)
			continue
		}
		chunk, err := Call[streamReadResponse](pluginabi.MethodHostHTTPStreamRead, streamIDRequest{StreamID: id})
		b.mu.Lock()
		switch {
		case err != nil:
			if ctxErr(b.ctx) != nil {
				err = b.ctx.Err()
			}
			if b.err == nil {
				b.err = err
			}
		default:
			b.pending = append(b.pending, chunk.Payload...)
			if chunk.Error != "" && b.err == nil {
				if ctxErr(b.ctx) != nil {
					b.err = b.ctx.Err()
				} else {
					b.err = errors.New(chunk.Error)
				}
			}
			if chunk.Done {
				b.done = true
			}
		}
		b.mu.Unlock()
	}
}

func (b *streamBody) fail(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
}

func (b *streamBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	finished := b.done
	id := b.id
	b.mu.Unlock()
	if b.op != nil {
		if !finished {
			b.client.cancelOperation(b.op.id)
		}
		b.op.stop()
	}
	if id == "" || finished {
		return nil
	}
	_, err := Call[empty](pluginabi.MethodHostHTTPStreamClose, streamIDRequest{StreamID: id})
	return err
}

type streamEmitRequest struct {
	StreamID    string           `json:"stream_id"`
	Payload     []byte           `json:"payload,omitempty"`
	Error       string           `json:"error,omitempty"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

// Emitter writes chunks to a host-owned downstream stream.
type Emitter struct {
	StreamID string
}

// Emit sends one downstream chunk. An error means the downstream stream is gone.
func (e Emitter) Emit(payload []byte) error {
	_, err := Call[empty](pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: e.StreamID, Payload: payload})
	return err
}

// Close ends the downstream stream, optionally with a structured error.
func (e Emitter) Close(detail *pluginabi.Error) {
	req := streamEmitRequest{StreamID: e.StreamID, ErrorDetail: detail}
	if detail != nil {
		req.Error = detail.Message
	}
	_, _ = Call[empty](pluginabi.MethodHostStreamClose, req)
}

type logRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// Log writes a message to the host log, attributed to the active request.
func (c Client) Log(level, message string, fields map[string]any) {
	_, _ = Call[empty](pluginabi.MethodHostLog, logRequest{HostCallbackID: c.CallbackID, Level: level, Message: message, Fields: fields})
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
