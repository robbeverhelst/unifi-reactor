/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package engine holds the provider-agnostic automation core: state tracking,
// transition detection, trigger matching, and action execution. It must never
// contain provider-specific logic.
package engine

import (
	"maps"
	"sync"
	"time"

	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

// Transition is a detected change of one state key between two observations.
type Transition struct {
	Provider string
	Key      string
	// From is empty when the key was observed for the first time.
	From string
	// To is empty when the key disappeared from the observation.
	To string
}

// DebounceConfig requires a changed value to hold for several consecutive
// observations before the store reports it.
//
// Debouncing belongs here rather than in a provider or in the reconciler
// because every Automation must see the same value: if two Automations could
// disagree about the current state, a target they share would oscillate
// between their conflicting claims.
type DebounceConfig struct {
	// Default is how many consecutive observations a changed value needs
	// before it is reported. Zero or one reports immediately.
	Default int
	// PerKey overrides Default for individual keys. The engine never authors
	// these — providers supply them as data, so the core stays free of any
	// knowledge of what the keys mean.
	PerKey map[string]int
}

// StoreOption configures a StateStore at construction.
type StoreOption func(*StateStore)

// WithDebounce applies a debounce configuration to one provider's keys.
func WithDebounce(provider string, config DebounceConfig) StoreOption {
	return func(s *StateStore) {
		s.debounce[provider] = config
	}
}

// candidate is a value seen but not yet reported, and how many consecutive
// observations have backed it so far.
type candidate struct {
	value string
	count int
}

// providerState is one provider's reported state plus the values still proving
// themselves.
type providerState struct {
	stable     map[string]string
	pending    map[string]candidate
	observedAt time.Time
}

// StateStore keeps the latest observation per provider and derives
// transitions by comparing consecutive observations. Repeated identical
// observations produce no transitions.
type StateStore struct {
	mu       sync.RWMutex
	current  map[string]providerState
	debounce map[string]DebounceConfig
}

func NewStateStore(options ...StoreOption) *StateStore {
	store := &StateStore{
		current:  map[string]providerState{},
		debounce: map[string]DebounceConfig{},
	}
	for _, option := range options {
		option(store)
	}
	return store
}

// samplesFor is how many consecutive observations a changed value needs before
// this store will report it.
func (s *StateStore) samplesFor(provider, key string) int {
	config, configured := s.debounce[provider]
	if !configured {
		return 1
	}
	if samples, overridden := config.PerKey[key]; overridden {
		return samples
	}
	if config.Default < 1 {
		return 1
	}
	return config.Default
}

// Observe stores the observation and returns the transitions relative to the
// previous reported state for the same provider. The first observation for a
// provider reports every key as a transition from "".
//
// A changed value is reported only once it has held for its configured number
// of consecutive observations, so a flapping signal produces one transition
// rather than one per flap.
func (s *StateStore) Observe(o events.Observation) []Transition {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, seen := s.current[o.Provider]
	if !seen {
		current = providerState{stable: map[string]string{}, pending: map[string]candidate{}}
	}
	current.observedAt = o.ObservedAt

	var transitions []Transition
	for key, to := range o.State {
		from, known := current.stable[key]
		if known && from == to {
			// Back to the reported value: whatever was proving itself is not
			// a change any more.
			delete(current.pending, key)
			continue
		}

		// A key seen for the first time has no previous value to oscillate
		// between, and holding it back would only delay the first reaction
		// after a restart for no gain.
		samples := s.samplesFor(o.Provider, key)
		if !known || samples <= 1 {
			current.stable[key] = to
			delete(current.pending, key)
			transitions = append(transitions, Transition{Provider: o.Provider, Key: key, From: from, To: to})
			continue
		}

		proving := current.pending[key]
		if proving.value != to {
			proving = candidate{value: to}
		}
		proving.count++
		if proving.count < samples {
			current.pending[key] = proving
			continue
		}
		current.stable[key] = to
		delete(current.pending, key)
		transitions = append(transitions, Transition{Provider: o.Provider, Key: key, From: from, To: to})
	}

	// A key vanishing is not debounced. It means the hardware reporting it
	// dropped off, which callers already treat as "hold, do not act", so
	// delaying it would buy nothing.
	for key, from := range current.stable {
		if _, still := o.State[key]; still {
			continue
		}
		delete(current.stable, key)
		delete(current.pending, key)
		transitions = append(transitions, Transition{Provider: o.Provider, Key: key, From: from})
	}

	s.current[o.Provider] = current
	return transitions
}

// Proving reports whether any of a provider's keys is part-way through its
// debounce threshold: a changed value has been seen, but not yet often enough
// to be reported.
//
// It exists so that anything able to ask for an out-of-band observation cannot
// also supply the samples that promote a value. Debounce is a statement about
// how much evidence a change needs before it is believed, and evidence
// gathered on demand by whoever asked for it is not evidence. Callers that
// have no such input can ignore this entirely.
func (s *StateStore) Proving(provider string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.current[provider].pending) > 0
}

// Get returns the latest reported state for a provider. Values still proving
// themselves against the debounce threshold are deliberately not visible here:
// every caller must see the same state.
func (s *StateStore) Get(provider string) (events.Observation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	current, ok := s.current[provider]
	if !ok {
		return events.Observation{}, false
	}
	return events.Observation{
		Provider:   provider,
		State:      maps.Clone(current.stable),
		ObservedAt: current.observedAt,
	}, true
}
