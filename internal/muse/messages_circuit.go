package muse

// Ported from the fork internal/runtime/executor/helps/meta_messages_circuit.go.

import (
	"sync"
	"time"
)

type MetaMessagesOutcome uint8

const (
	MetaMessagesNeutral MetaMessagesOutcome = iota
	MetaMessagesSuccess
	MetaMessagesDenied
	MetaMessagesTransient
)

type metaMessagesCircuitState struct {
	generation uint64
	lease      uint64
	until      time.Time
	failures   []time.Time
}

// MetaMessagesCircuit isolates endpoint failures from the shared provider credential.
type MetaMessagesCircuit struct {
	mu        sync.Mutex
	states    map[string]*metaMessagesCircuitState
	nextLease uint64
	now       func() time.Time
}

type MetaMessagesTicket struct {
	circuit           *MetaMessagesCircuit
	authID            string
	generation, lease uint64
	once              sync.Once
}

func NewMetaMessagesCircuit(now func() time.Time) *MetaMessagesCircuit {
	if now == nil {
		now = time.Now
	}
	return &MetaMessagesCircuit{states: map[string]*metaMessagesCircuitState{}, now: now}
}

// Acquire permits one half-open attempt and returns the remaining open duration otherwise.
func (c *MetaMessagesCircuit) Acquire(authID string) (*MetaMessagesTicket, time.Duration) {
	if authID == "" {
		return &MetaMessagesTicket{}, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[authID]
	if state == nil {
		state = &metaMessagesCircuitState{}
		c.states[authID] = state
	}
	now := c.now()
	if state.until.After(now) {
		return nil, state.until.Sub(now)
	}
	if !state.until.IsZero() {
		if state.lease != 0 {
			return nil, time.Second
		}
		c.nextLease++
		state.lease = c.nextLease
	}
	return &MetaMessagesTicket{circuit: c, authID: authID, generation: state.generation, lease: state.lease}, 0
}

// Complete is idempotent; stale generations cannot overwrite a newer result.
func (t *MetaMessagesTicket) Complete(outcome MetaMessagesOutcome) {
	if t == nil || t.circuit == nil {
		return
	}
	t.once.Do(func() {
		c := t.circuit
		c.mu.Lock()
		defer c.mu.Unlock()
		state := c.states[t.authID]
		if state == nil || state.generation != t.generation || state.lease != t.lease {
			return
		}
		now := c.now()
		if t.lease != 0 {
			state.lease = 0
		}
		switch outcome {
		case MetaMessagesSuccess:
			state.generation++
			state.until = time.Time{}
			state.failures = nil
		case MetaMessagesDenied:
			state.generation++
			state.until = now.Add(30 * time.Minute)
			state.failures = nil
		case MetaMessagesTransient:
			kept := state.failures[:0]
			for _, at := range state.failures {
				if now.Sub(at) < time.Minute {
					kept = append(kept, at)
				}
			}
			state.failures = append(kept, now)
			if t.lease != 0 || len(state.failures) >= 5 {
				state.generation++
				state.until = now.Add(time.Minute)
				state.failures = nil
			}
		}
	})
}
