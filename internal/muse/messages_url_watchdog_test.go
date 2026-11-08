package muse

// Consumer-visible regression cases ported from fork helps/meta_messages_url_watchdog_test.go.

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type metaManualIdleTimer struct {
	mu             sync.Mutex
	due            time.Duration
	fn             func()
	stopped, fired bool
}

func (t *metaManualIdleTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	active := !t.stopped && !t.fired
	t.stopped = true
	return active
}

type metaManualIdleClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*metaManualIdleTimer
}

func (c *metaManualIdleClock) after(duration time.Duration, fn func()) metaMessagesIdleTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &metaManualIdleTimer{due: c.now + duration, fn: fn}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *metaManualIdleClock) advance(duration time.Duration) {
	c.mu.Lock()
	c.now += duration
	now := c.now
	timers := append([]*metaManualIdleTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, timer := range timers {
		timer.mu.Lock()
		fire := !timer.stopped && !timer.fired && timer.due <= now
		if fire {
			timer.fired = true
		}
		timer.mu.Unlock()
		if fire {
			timer.fn()
		}
	}
}

type metaCountingReadCloser struct {
	io.Reader
	closed atomic.Int32
}

func (c *metaCountingReadCloser) Close() error { c.closed.Add(1); return nil }

func TestMessagesURLImageWatchdogConfigurationAndScope(t *testing.T) {
	if duration, err := MetaMessagesURLImageIdleTimeout(nil); err != nil || duration != 60*time.Second {
		t.Fatalf("omitted default: %v %v", duration, err)
	}
	for _, value := range []int{0, 1, 86400} {
		duration, err := MetaMessagesURLImageIdleTimeout(&value)
		if err != nil || duration != time.Duration(value)*time.Second {
			t.Fatalf("configuration %d: %v %v", value, duration, err)
		}
	}
	for _, value := range []int{-1, 86401, int(^uint(0) >> 1)} {
		if _, err := MetaMessagesURLImageIdleTimeout(&value); err == nil {
			t.Fatalf("invalid duration %d accepted", value)
		}
	}
	if watch := NewMetaMessagesURLImageWatchdog(context.Background(), 0); watch != nil {
		t.Fatal("explicit zero did not disable the watchdog")
	}
	for _, tt := range []struct {
		body  string
		guard bool
	}{
		{`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}]}`, true},
		{`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}]}]}`, true},
		{`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}}]}]}`, true},
		{`{"messages":[{"role":"user","content":"https://example.invalid/image.png"}]}`, false},
		{`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"aGVsbG8="}}]}]}`, false},
		{`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"inspect","input":{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}}]}]}`, false},
		{`{"tools":[{"name":"inspect","input_schema":{"default":{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}}}],"messages":[{"role":"user","content":"text"}]}`, false},
		{`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.invalid/report.pdf"}}]}]}`, false},
		{`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image.png"}]}]}`, false},
	} {
		if got := MetaMessagesHasURLImage([]byte(tt.body)); got != tt.guard {
			t.Fatalf("guard=%v want=%v for %s", got, tt.guard, tt.body)
		}
	}
}

func TestMessagesURLImageWatchdogIdleProgressAndStaleTimers(t *testing.T) {
	clock := &metaManualIdleClock{}
	parent := context.Background()
	watch := newMetaMessagesURLImageWatchdog(parent, time.Minute, clock.after)
	defer watch.Stop()
	first := clock.timers[0]
	clock.advance(55 * time.Second)
	if !watch.UsefulProgress() {
		t.Fatal("progress was not accepted")
	}
	first.fn()
	if watch.Error(nil) != nil {
		t.Fatal("stale timer expired a refreshed window")
	}
	clock.advance(55 * time.Second)
	if watch.Error(nil) != nil {
		t.Fatal("watchdog became a blanket full-request duration limit")
	}
	clock.advance(5 * time.Second)
	err := watch.Error(errors.New("HTTP transport canceled"))
	if !IsMetaMessagesURLImageTimeout(err) || parent.Err() != nil {
		t.Fatalf("timeout escaped its upstream context: %v", err)
	}
	var terminated *StatusError
	if !errors.As(err, &terminated) || terminated.Status != 504 || !strings.Contains(string(terminated.Body), "url_image_idle_timeout") {
		t.Fatalf("wrong timeout envelope: %v", err)
	}
	var scoped interface{ IsRequestScoped() bool }
	if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
		t.Fatal("timeout is not request scoped")
	}
	if errors.Is(err, context.Canceled) || !IsMetaMessagesURLImageTimeout(context.Cause(watch.Context())) {
		t.Fatal("watchdog timeout was confused with caller cancellation")
	}
	if watch.UsefulProgress() {
		t.Fatal("progress resurrected an expired watchdog")
	}
}

func TestMessagesURLImageWatchdogIgnoresPingsAndMessageStart(t *testing.T) {
	clock := &metaManualIdleClock{}
	watch := newMetaMessagesURLImageWatchdog(context.Background(), time.Minute, clock.after)
	defer watch.Stop()
	for _, data := range []string{
		`{"type":"message_start","message":{"content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"ping"}`, `{"type":"ping"}`, `{"type":"content_block_start","content_block":{"type":"text","text":""}}`,
	} {
		if MetaMessagesSSEUsefulProgress([]byte(data)) {
			t.Fatalf("non-useful event reset watchdog: %s", data)
		}
		clock.advance(15 * time.Second)
	}
	if !IsMetaMessagesURLImageTimeout(watch.Error(nil)) {
		t.Fatal("keepalives prevented the timeout")
	}
	for _, data := range []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"read","input":{}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	} {
		if !MetaMessagesSSEUsefulProgress([]byte(data)) {
			t.Fatalf("useful event ignored: %s", data)
		}
	}
}

func TestMessagesURLImageWatchdogBodyProgressAndCleanup(t *testing.T) {
	for _, stream := range []bool{false, true} {
		clock := &metaManualIdleClock{}
		watch := newMetaMessagesURLImageWatchdog(context.Background(), time.Minute, clock.after)
		original := &metaCountingReadCloser{Reader: strings.NewReader(`{"content":"some data"}`)}
		body := watch.WrapBody(original, stream)
		clock.advance(55 * time.Second)
		buffer := make([]byte, 128)
		if _, err := body.Read(buffer); err != nil {
			t.Fatal(err)
		}
		clock.advance(6 * time.Second)
		if IsMetaMessagesURLImageTimeout(watch.Error(nil)) != stream {
			t.Fatalf("stream=%v wrong byte-progress policy: %v", stream, watch.Error(nil))
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		_ = body.Close()
		watch.Stop()
		if original.closed.Load() != 1 {
			t.Fatalf("body closed %d times", original.closed.Load())
		}
		if !stream {
			clock.advance(time.Hour)
			if watch.Error(nil) != nil {
				t.Fatal("cleanup left an active timer")
			}
		}
	}
	clock := &metaManualIdleClock{}
	watch := newMetaMessagesURLImageWatchdog(context.Background(), time.Minute, clock.after)
	body := watch.WrapBody(io.NopCloser(strings.NewReader("   \n")), false)
	clock.advance(55 * time.Second)
	_, _ = body.Read(make([]byte, 128))
	clock.advance(6 * time.Second)
	if !IsMetaMessagesURLImageTimeout(watch.Error(nil)) {
		t.Fatal("whitespace-only body bytes faked useful progress")
	}
	_ = body.Close()
}

func TestMessagesURLImageWatchdogCallerCancellationRemainsCancellation(t *testing.T) {
	clock := &metaManualIdleClock{}
	parent, cancel := context.WithCancel(context.Background())
	watch := newMetaMessagesURLImageWatchdog(parent, time.Minute, clock.after)
	defer watch.Stop()
	cancel()
	clock.advance(time.Minute)
	err := watch.Error(errors.New("transport failed"))
	if !errors.Is(err, context.Canceled) || IsMetaMessagesURLImageTimeout(err) {
		t.Fatalf("caller cancellation was rewritten: %v", err)
	}
}
