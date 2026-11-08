package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestStreamBodyCloseCancelsBlockedRead(t *testing.T) {
	reading := make(chan struct{})
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	var mu sync.Mutex
	var cancels []operationCancelRequest
	var closed []streamIDRequest
	SetTransport(func(method string, raw []byte) ([]byte, error) {
		var result any = struct{}{}
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			result = operationOpenResponse{OperationID: "operation-test"}
		case pluginabi.MethodHostHTTPDoStream:
			var req httpRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			if req.OperationID != "operation-test" || req.HostCallbackID != "callback-test" {
				t.Errorf("stream not associated with cancellation operation: %+v", req)
			}
			result = streamOpenResponse{StatusCode: 200, StreamID: "stream-test"}
		case pluginabi.MethodHostHTTPStreamRead:
			close(reading)
			<-cancelled
			result = streamReadResponse{Done: true}
		case pluginabi.MethodHostHTTPCancel:
			var req operationCancelRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			mu.Lock()
			cancels = append(cancels, req)
			mu.Unlock()
			cancelOnce.Do(func() { close(cancelled) })
		case pluginabi.MethodHostHTTPStreamClose:
			var req streamIDRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			mu.Lock()
			closed = append(closed, req)
			mu.Unlock()
		default:
			t.Errorf("unexpected callback %s", method)
		}
		return json.Marshal(map[string]any{"ok": true, "result": result})
	})
	t.Cleanup(func() { SetTransport(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := NewClient("callback-test").DoStream(ctx, Request{Method: "POST", URL: "https://upstream.invalid/responses"})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-reading:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not enter host callback")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- resp.Body.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancelOnce.Do(func() { close(cancelled) })
		<-readDone
		<-closeDone
		t.Fatal("Close deadlocked behind a blocked stream_read instead of cancelling it")
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closed read error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operation cancellation did not unblock stream_read")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cancels) != 1 || cancels[0].OperationID != "operation-test" || cancels[0].HostCallbackID != "callback-test" || len(closed) != 1 || closed[0].StreamID != "stream-test" {
		t.Fatalf("wrong/non-idempotent cancellation: cancels=%+v closes=%+v", cancels, closed)
	}
}

// A plugin-scoped request (no downstream callback) must still be cancellable,
// or a hung credential mint would hold the host's refresh lock indefinitely.
func TestCallbackFreeDoCancelsHostOperation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	var mu sync.Mutex
	var cancels []operationCancelRequest
	SetTransport(func(method string, raw []byte) ([]byte, error) {
		var scoped struct {
			CallbackID string `json:"host_callback_id"`
		}
		if err := json.Unmarshal(raw, &scoped); err != nil {
			return nil, err
		}
		if scoped.CallbackID != "" {
			t.Errorf("%s attributed to downstream callback %q", method, scoped.CallbackID)
		}
		var result any = struct{}{}
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			result = operationOpenResponse{OperationID: "plugin-operation"}
		case pluginabi.MethodHostHTTPDo:
			var req httpRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			if req.OperationID != "plugin-operation" {
				t.Errorf("request not associated with cancellation operation: %+v", req)
			}
			close(started)
			<-cancelled
			return nil, errors.New("operation cancelled")
		case pluginabi.MethodHostHTTPCancel:
			var req operationCancelRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			mu.Lock()
			cancels = append(cancels, req)
			mu.Unlock()
			cancelOnce.Do(func() { close(cancelled) })
		default:
			t.Errorf("unexpected callback %s", method)
		}
		return json.Marshal(map[string]any{"ok": true, "result": result})
	})
	t.Cleanup(func() { SetTransport(nil) })
	t.Cleanup(func() { cancelOnce.Do(func() { close(cancelled) }) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewClient("").Do(ctx, Request{Method: "POST", URL: "https://mint.invalid/key"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach host callback")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not cancel the plugin-scoped host operation")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cancels) != 1 || cancels[0].OperationID != "plugin-operation" {
		t.Fatalf("wrong cancellation: %+v", cancels)
	}
}
