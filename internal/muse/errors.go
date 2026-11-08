package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/tidwall/gjson"
)

// Scope classifies a failure's effect without owning host credential policy.
type Scope int

const (
	// ScopeRequest identifies request-local or surface-specific failures.
	ScopeRequest Scope = iota
	// ScopeModel identifies an upstream model availability failure.
	ScopeModel
	// ScopeCredential identifies an account-wide credential failure.
	ScopeCredential
)

// StatusError is an upstream or plugin failure surfaced to the host with an HTTP status.
type StatusError struct {
	Status     int
	Code       string
	Message    string
	Body       []byte
	RetryAfter time.Duration
	Scope      Scope
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if len(e.Body) > 0 {
		return string(e.Body)
	}
	return fmt.Sprintf("meta upstream status %d", e.Status)
}

// StatusCode reports the HTTP status attached to the failure.
func (e *StatusError) StatusCode() int { return e.Status }

// errorf builds a plugin-side error with a status.
func errorf(status int, code, format string, args ...any) *StatusError {
	return &StatusError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ABIError converts any error into the host error envelope payload.
func ABIError(err error) *pluginabi.Error {
	if err == nil {
		return nil
	}
	var se *StatusError
	if errors.As(err, &se) {
		// The host writes a JSON error text verbatim as the client error body, so a
		// JSON Body (upstream error or a surface-shaped envelope) is sent as-is.
		message := se.Error()
		if trimmed := bytes.TrimSpace(se.Body); len(trimmed) > 0 && json.Valid(trimmed) {
			message = string(trimmed)
		}
		out := &pluginabi.Error{Code: se.Code, Message: message, HTTPStatus: se.Status}
		if out.Code == "" {
			out.Code = "upstream_error"
		}
		if se.RetryAfter > 0 {
			out.RetryAfterSeconds = int(math.Ceil(se.RetryAfter.Seconds()))
		}
		if se.Status == http.StatusTooManyRequests || se.Status >= 500 {
			out.Retryable = true
		}
		return out
	}
	var coded interface{ StatusCode() int }
	status := 0
	if errors.As(err, &coded) {
		status = coded.StatusCode()
	}
	return &pluginabi.Error{Code: "plugin_error", Message: err.Error(), HTTPStatus: status}
}

const metaNotFoundCooldown = 30 * time.Second

// upstreamError classifies a non-2xx Meta response. now is injectable for tests.
func upstreamError(status int, body []byte, now time.Time) *StatusError {
	se := &StatusError{Status: status, Body: append([]byte(nil), body...), Code: "upstream_error"}
	switch {
	case status == http.StatusTooManyRequests:
		se.Code = "rate_limited"
		se.Scope = ScopeModel
		if retry, ok := metaRetryAfter(body, now); ok {
			se.RetryAfter = retry
		}
		if isSubscriptionQuota(status, body) {
			se.Code = "subscription_quota"
			se.Scope = ScopeCredential
		}
	case status == http.StatusNotFound:
		se.Code = "model_not_found"
		se.Scope = ScopeModel
		se.RetryAfter = metaNotFoundCooldown
		if retry, ok := metaRetryAfter(body, now); ok {
			se.RetryAfter = retry
		}
	case status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden:
		se.Code = "credential_rejected"
		se.Scope = ScopeCredential
	case status >= 500:
		se.Scope = ScopeModel
	default:
		se.Scope = ScopeRequest
	}
	return se
}

// metaRetryAfter reads error.resets_at (unix seconds) from a Meta error body.
func metaRetryAfter(body []byte, now time.Time) (time.Duration, bool) {
	for _, path := range []string{"error.resets_at", "response.error.resets_at"} {
		if resetsAt := gjson.GetBytes(body, path).Int(); resetsAt > 0 {
			at := time.Unix(resetsAt, 0)
			if at.After(now) {
				return at.Sub(now), true
			}
		}
	}
	return 0, false
}

// isSubscriptionQuota reports a credential-wide Muse subscription quota exhaustion.
func isSubscriptionQuota(status int, body []byte) bool {
	if status != http.StatusTooManyRequests || len(body) == 0 {
		return false
	}
	for _, prefix := range []string{"error", "response.error"} {
		msg := strings.ToLower(gjson.GetBytes(body, prefix+".message").String())
		code := strings.ToLower(gjson.GetBytes(body, prefix+".code").String())
		if strings.Contains(msg, "subscription quota") || strings.Contains(msg, "quota exhausted") {
			return true
		}
		if (code == "rate_limit_exceeded" || strings.Contains(code, "quota")) && gjson.GetBytes(body, prefix+".resets_at").Exists() {
			return true
		}
	}
	return false
}

// streamEventError converts an SSE error / response.failed event into a StatusError.
func streamEventError(eventData []byte, now time.Time) *StatusError {
	eventType := gjson.GetBytes(eventData, "type").String()
	if eventType != "error" && eventType != "response.failed" {
		return nil
	}
	status := http.StatusBadGateway
	for _, path := range []string{"error.code", "response.error.code"} {
		if code := int(gjson.GetBytes(eventData, path).Int()); code >= 400 && code <= 599 {
			status = code
			break
		}
	}
	if status == http.StatusBadGateway && isSubscriptionQuota(http.StatusTooManyRequests, eventData) {
		status = http.StatusTooManyRequests
	}
	return upstreamError(status, eventData, now)
}
