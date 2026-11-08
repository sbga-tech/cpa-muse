package muse

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const maxQuotaEventBytes = 64 << 10

// quotaWindow is one Meta subscription usage window.
type quotaWindow struct {
	usedPercent float64
	hasUsed     bool
	resetsAt    time.Time
	duration    time.Duration
}

type quotaSnapshot struct {
	weekly    quotaWindow
	window    quotaWindow
	updatedAt time.Time
}

// quotaStore keeps the last response.subscription_usage seen per credential.
// Meta streams it alongside answers; it is captured passively, never fetched.
type quotaStore struct {
	mu   sync.Mutex
	byID map[string]quotaSnapshot
}

func (q *quotaStore) observe(cred Credential, payload []byte) {
	if q == nil || len(payload) == 0 || len(payload) > maxQuotaEventBytes {
		return
	}
	subscription := gjson.GetBytes(payload, "subscription")
	if !subscription.IsObject() {
		return
	}
	snap := quotaSnapshot{
		weekly:    parseQuotaWindow(subscription.Get("weekly")),
		window:    parseQuotaWindow(subscription.Get("window")),
		updatedAt: time.Now(),
	}
	if !snap.weekly.hasUsed && !snap.window.hasUsed && snap.weekly.resetsAt.IsZero() && snap.window.resetsAt.IsZero() {
		return
	}
	q.mu.Lock()
	if q.byID == nil {
		q.byID = make(map[string]quotaSnapshot)
	}
	q.byID[cred.ID] = snap
	q.mu.Unlock()
}

func parseQuotaWindow(node gjson.Result) quotaWindow {
	var w quotaWindow
	if used := node.Get("used_percent"); used.Type == gjson.Number {
		v := used.Float()
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100000 {
			w.usedPercent, w.hasUsed = v, true
		}
	}
	if resets := node.Get("resets_at"); resets.Type == gjson.Number && resets.Int() > 0 {
		w.resetsAt = time.Unix(resets.Int(), 0)
	}
	if mins := node.Get("window_duration_mins"); mins.Type == gjson.Number && mins.Int() > 0 {
		w.duration = time.Duration(mins.Int()) * time.Minute
	}
	return w
}

func (q *quotaStore) get(id string) (quotaSnapshot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	snap, ok := q.byID[id]
	return snap, ok
}

// QuotaIdentifier is the provider key handled by the quota provider.
func QuotaIdentifier() string { return "muse" }

// DescribeQuota reports the quota provider metadata.
func (e *Executor) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"muse"}, DisplayName: "Muse subscription"}, nil
}

// FetchQuota returns the last observed Muse subscription windows for a host credential.
func (e *Executor) FetchQuota(_ context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	id := strings.TrimSpace(req.AuthID)
	snap, ok := e.quota.get(id)
	if !ok {
		return pluginapi.QuotaFetchResponse{}, nil
	}
	group := pluginapi.QuotaGroup{DisplayName: "Muse subscription"}
	if bucket, ok := quotaBucket("weekly", snap.weekly); ok {
		group.Buckets = append(group.Buckets, bucket)
	}
	label := "window"
	if snap.window.duration > 0 {
		label = snap.window.duration.String()
	}
	if bucket, ok := quotaBucket(label, snap.window); ok {
		group.Buckets = append(group.Buckets, bucket)
	}
	if len(group.Buckets) == 0 {
		return pluginapi.QuotaFetchResponse{}, nil
	}
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{group}}, nil
}

func quotaBucket(name string, w quotaWindow) (pluginapi.QuotaBucket, bool) {
	if !w.hasUsed {
		return pluginapi.QuotaBucket{}, false
	}
	remaining := 1 - w.usedPercent/100
	if remaining < 0 {
		remaining = 0
	}
	bucket := pluginapi.QuotaBucket{Window: name, RemainingFraction: remaining}
	if !w.resetsAt.IsZero() {
		bucket.ResetTime = w.resetsAt.UTC().Format(time.RFC3339)
	}
	return bucket, true
}

// ResetQuota is unsupported: Muse quota is server-side.
func (e *Executor) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "Muse subscription quota cannot be reset from CPA"}, nil
}
