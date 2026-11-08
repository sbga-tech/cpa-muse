package muse

import (
	"bytes"
	"context"
	"net/http"

	"github.com/google/uuid"
)

type requestStateContextKey struct{}

// WithRequestState owns retry plans for one execution without exposing them to plugin metadata cloning.
func WithRequestState(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestStateContextKey{}, &profileRequestState{})
}

func RequestStateFromContext(ctx context.Context) *profileRequestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(requestStateContextKey{}).(*profileRequestState)
	return state
}

func (state *profileRequestState) AnonymousCaller() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.anonymous == "" {
		state.anonymous = "anonymous:" + uuid.NewString()
	}
	return state.anonymous
}

// FreezeSelection keeps hot reloads from switching the mode halfway through an HTTP request.
func FreezeSelection(ctx context.Context, caller string, selection ProfileSelection) ProfileSelection {
	state := RequestStateFromContext(ctx)
	if state == nil {
		return selection
	}
	key := digestStrings(caller)
	state.mu.Lock()
	defer state.mu.Unlock()
	if prior, found := state.selections[key]; found {
		return prior
	}
	if state.selections == nil {
		state.selections = make(map[[32]byte]ProfileSelection)
	}
	state.selections[key] = selection
	return selection
}

// FreezeWirePlan retains post-rule bytes only in the owning request, never in a global cache.
// Authentication is injected separately from this frozen wire plan.
func FreezeWirePlan(ctx context.Context, key [32]byte, build func() ([]byte, http.Header, error)) ([]byte, http.Header, error) {
	state := RequestStateFromContext(ctx)
	if state == nil {
		return build()
	}
	state.wireMu.Lock()
	defer state.wireMu.Unlock()
	if prior := state.wirePlans[key]; prior != nil {
		return prior.Body(), prior.Headers(), nil
	}
	body, headers, err := build()
	if err != nil {
		return nil, nil, err
	}
	if state.wirePlans == nil {
		state.wirePlans = make(map[[32]byte]*profilePlan)
	}
	plan := &profilePlan{body: bytes.Clone(body), headers: headers.Clone()}
	state.wirePlans[key] = plan
	return plan.Body(), plan.Headers(), nil
}
