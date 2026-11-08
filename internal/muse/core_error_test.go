package muse

import (
	"errors"
	"testing"
	"time"
)

func coreStatus(t *testing.T, err error, status int, code string) *StatusError {
	t.Helper()
	var se *StatusError
	if !errors.As(err, &se) || se.Status != status || se.Code != code {
		t.Fatalf("want status=%d code=%s; got %#v", status, code, err)
	}
	return se
}

func TestCoreUpstreamErrorScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		scope  Scope
	}{
		{"subscription", 429, `{"error":{"code":"quota_exhausted","message":"subscription quota exhausted"}}`, ScopeCredential},
		{"plain-rate-limit", 429, `{"error":{"message":"too many requests"}}`, ScopeModel},
		{"missing-model", 404, `{"error":{"code":"model_not_found"}}`, ScopeModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var se *StatusError
			if err := upstreamError(tc.status, []byte(tc.body), time.Now()); !errors.As(err, &se) || se.Status != tc.status || se.Scope != tc.scope || string(se.Body) != tc.body {
				t.Fatalf("upstream classification: %#v", err)
			}
		})
	}
}
