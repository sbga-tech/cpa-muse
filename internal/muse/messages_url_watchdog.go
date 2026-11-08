package muse

// Ported from the fork internal/runtime/executor/helps/meta_messages_url_watchdog.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	MetaMessagesURLImageDefaultIdleSeconds = 60
	MetaMessagesURLImageMaxIdleSeconds     = 86400
)

func MetaMessagesURLImageIdleTimeout(seconds *int) (time.Duration, error) {
	value := MetaMessagesURLImageDefaultIdleSeconds
	if seconds != nil {
		value = *seconds
	}
	if value < 0 || value > MetaMessagesURLImageMaxIdleSeconds {
		return 0, MetaMessagesError(500, "invalid_config", "url-image-idle-timeout-seconds", "expected an integer from 0 through 86400; 0 explicitly disables the watchdog")
	}
	return time.Duration(value) * time.Second, nil
}

// MetaMessagesHasURLImage examines content blocks, never schemas or arbitrary tool arguments.
func MetaMessagesHasURLImage(body []byte) bool {
	root := gjson.ParseBytes(body)
	if metaContentHasURLImage(root.Get("system")) {
		return true
	}
	found := false
	root.Get("messages").ForEach(func(_, message gjson.Result) bool {
		found = metaContentHasURLImage(message.Get("content"))
		return !found
	})
	return found
}

func metaContentHasURLImage(content gjson.Result) bool {
	if content.IsArray() {
		found := false
		content.ForEach(func(_, block gjson.Result) bool { found = metaContentHasURLImage(block); return !found })
		return found
	}
	if !content.IsObject() {
		return false
	}
	switch content.Get("type").String() {
	case "image":
		return content.Get("source.type").String() == "url" && strings.TrimSpace(content.Get("source.url").String()) != ""
	case "tool_result":
		return metaContentHasURLImage(content.Get("content"))
	case "document":
		if content.Get("source.type").String() == "content" {
			return metaContentHasURLImage(content.Get("source.content"))
		}
	}
	return false
}

// MetaMessagesURLImageTimeoutError stops retries without marking the credential or surface unhealthy.
type MetaMessagesURLImageTimeoutError struct {
	*StatusError
	Idle time.Duration
}

func newMetaMessagesURLImageTimeoutError(idle time.Duration) *MetaMessagesURLImageTimeoutError {
	base := MetaMessagesError(504, "url_image_idle_timeout", "upstream", fmt.Sprintf("URL-image request made no useful upstream progress for %s; upstream processing was canceled", idle))
	return &MetaMessagesURLImageTimeoutError{StatusError: base.(*StatusError), Idle: idle}
}

func (e *MetaMessagesURLImageTimeoutError) Error() string         { return string(e.Body) }
func (e *MetaMessagesURLImageTimeoutError) Unwrap() error         { return e.StatusError }
func (e *MetaMessagesURLImageTimeoutError) IsRequestScoped() bool { return true }

func IsMetaMessagesURLImageTimeout(err error) bool {
	var timeout *MetaMessagesURLImageTimeoutError
	return errors.As(err, &timeout)
}

type metaMessagesIdleTimer interface{ Stop() bool }
type metaMessagesAfterFunc func(time.Duration, func()) metaMessagesIdleTimer

// MetaMessagesURLImageWatchdog owns only the upstream child context, not the downstream request.
type MetaMessagesURLImageWatchdog struct {
	mu          sync.Mutex
	parent, ctx context.Context
	cancel      context.CancelCauseFunc
	idle        time.Duration
	after       metaMessagesAfterFunc
	timer       metaMessagesIdleTimer
	generation  uint64
	closed      bool
	timeout     *MetaMessagesURLImageTimeoutError
	closeBody   func()
	stopParent  func() bool
}

func NewMetaMessagesURLImageWatchdog(parent context.Context, idle time.Duration) *MetaMessagesURLImageWatchdog {
	return newMetaMessagesURLImageWatchdog(parent, idle, func(d time.Duration, fn func()) metaMessagesIdleTimer { return time.AfterFunc(d, fn) })
}

func newMetaMessagesURLImageWatchdog(parent context.Context, idle time.Duration, after metaMessagesAfterFunc) *MetaMessagesURLImageWatchdog {
	if idle <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancelCause(parent)
	watch := &MetaMessagesURLImageWatchdog{parent: parent, ctx: ctx, cancel: cancel, idle: idle, after: after}
	watch.mu.Lock()
	watch.armLocked()
	watch.stopParent = context.AfterFunc(parent, watch.Stop)
	watch.mu.Unlock()
	return watch
}

func (w *MetaMessagesURLImageWatchdog) Context() context.Context { return w.ctx }

func (w *MetaMessagesURLImageWatchdog) armLocked() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.generation++
	generation := w.generation
	w.timer = w.after(w.idle, func() { w.expire(generation) })
}

func (w *MetaMessagesURLImageWatchdog) expire(generation uint64) {
	w.mu.Lock()
	if w.closed || generation != w.generation || w.parent.Err() != nil {
		w.mu.Unlock()
		return
	}
	w.timeout = newMetaMessagesURLImageTimeoutError(w.idle)
	w.closed = true
	closeBody, stopParent, timeout := w.closeBody, w.stopParent, w.timeout
	w.stopParent = nil
	w.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
	w.cancel(timeout)
	if closeBody != nil {
		closeBody()
	}
}

func (w *MetaMessagesURLImageWatchdog) UsefulProgress() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.parent.Err() != nil {
		return false
	}
	w.armLocked()
	return true
}

// Error distinguishes the URL-image watchdog from caller cancellation and unrelated HTTP failures.
func (w *MetaMessagesURLImageWatchdog) Error(original error) error {
	if w == nil {
		return original
	}
	if err := w.parent.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	timeout := w.timeout
	w.mu.Unlock()
	if timeout != nil {
		return timeout
	}
	return original
}

func (w *MetaMessagesURLImageWatchdog) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.closed = true
	w.generation++
	if w.timer != nil {
		w.timer.Stop()
	}
	closeBody, stopParent := w.closeBody, w.stopParent
	w.stopParent = nil
	w.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
	w.cancel(context.Canceled)
	if closeBody != nil {
		closeBody()
	}
}

type metaMessagesWatchedBody struct {
	io.ReadCloser
	watch     *MetaMessagesURLImageWatchdog
	stream    bool
	closeOnce sync.Once
	closeErr  error
}

func (w *MetaMessagesURLImageWatchdog) WrapBody(body io.ReadCloser, stream bool) io.ReadCloser {
	if w == nil {
		return body
	}
	wrapped := &metaMessagesWatchedBody{ReadCloser: body, watch: w, stream: stream}
	w.mu.Lock()
	w.closeBody = wrapped.closeUnderlying
	closed := w.closed
	w.mu.Unlock()
	if closed {
		wrapped.closeUnderlying()
	}
	return wrapped
}

func (b *metaMessagesWatchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.stream && len(bytes.TrimSpace(p[:n])) > 0 {
		b.watch.UsefulProgress()
	}
	resolved := b.watch.Error(err)
	if err == io.EOF {
		b.watch.Stop()
	}
	return n, resolved
}

func (b *metaMessagesWatchedBody) closeUnderlying() {
	b.closeOnce.Do(func() { b.closeErr = b.ReadCloser.Close() })
}

func (b *metaMessagesWatchedBody) Close() error {
	b.watch.Stop()
	b.closeUnderlying()
	return b.closeErr
}

func MetaMessagesURLImageProgress(body io.ReadCloser) bool {
	if watched, ok := body.(*metaMessagesWatchedBody); ok {
		return watched.watch.UsefulProgress()
	}
	return false
}

// MetaMessagesSSEUsefulProgress excludes keepalives and the initial empty message envelope.
func MetaMessagesSSEUsefulProgress(data []byte) bool {
	root := gjson.ParseBytes(data)
	switch root.Get("type").String() {
	case "content_block_delta":
		delta := root.Get("delta")
		for _, field := range []string{"text", "thinking", "partial_json", "signature"} {
			if strings.TrimSpace(delta.Get(field).String()) != "" {
				return true
			}
		}
		citation := delta.Get("citation")
		return citation.IsObject() && citation.Raw != "{}"
	case "content_block_start":
		block := root.Get("content_block")
		for _, field := range []string{"text", "thinking", "data", "name"} {
			if strings.TrimSpace(block.Get(field).String()) != "" {
				return true
			}
		}
		content := block.Get("content")
		return (content.IsArray() && len(content.Array()) > 0) || (content.IsObject() && content.Raw != "{}")
	case "message_delta":
		return strings.TrimSpace(root.Get("delta.stop_reason").String()) != ""
	}
	return false
}
