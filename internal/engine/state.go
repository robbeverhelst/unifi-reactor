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
	"strings"
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
	//
	// An entry may end in "*", which matches every key with that prefix:
	// "device.*" covers a fleet whose key names are only known at runtime,
	// which is otherwise impossible to configure. Exact entries win over
	// patterns, and the longest matching prefix wins between patterns, so a
	// specific key can always be pulled out of the group it belongs to. This
	// is still opaque data — the engine matches strings and learns nothing
	// about what a prefix means.
	PerKey map[string]int
}

// patternSamples is the sample count of the longest "prefix*" entry matching
// key, and whether any did.
func (d DebounceConfig) patternSamples(key string) (int, bool) {
	best, samples := -1, 0
	for pattern, configured := range d.PerKey {
		prefix, wildcard := strings.CutSuffix(pattern, "*")
		if !wildcard || !strings.HasPrefix(key, prefix) || len(prefix) <= best {
			continue
		}
		best, samples = len(prefix), configured
	}
	return samples, best >= 0
}

// StoreOption configures a StateStore at construction.
type StoreOption func(*StateStore)

// WithDebounce applies a debounce configuration to one provider's keys.
func WithDebounce(provider string, config DebounceConfig) StoreOption {
	return func(s *StateStore) {
		s.debounce[provider] = config
	}
}

// WithStaleAfter bounds how old one provider's reported state may be before the
// store calls it stale. Zero or less leaves it unbounded, which is the default.
//
// It bounds what is SAID, never what is done. The store goes on reporting the
// last state it has whatever its age, because the alternative — withdrawing
// state Reactor can no longer confirm — would release claims during exactly the
// incident that made the console unreachable. This is the same rule the
// reconciler applies to a key that stops being reported, and the same reason:
// losing sight of a thing is not evidence about the thing.
//
// The duration is opaque here in the way DebounceConfig's sample counts are.
// What a sensible bound is depends on how often a provider polls and how long
// its console may plausibly be unreachable, and the core knows neither.
func WithStaleAfter(provider string, bound time.Duration) StoreOption {
	return func(s *StateStore) {
		if bound > 0 {
			s.staleAfter[provider] = bound
		}
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
	mu         sync.RWMutex
	current    map[string]providerState
	debounce   map[string]DebounceConfig
	staleAfter map[string]time.Duration
}

func NewStateStore(options ...StoreOption) *StateStore {
	store := &StateStore{
		current:    map[string]providerState{},
		debounce:   map[string]DebounceConfig{},
		staleAfter: map[string]time.Duration{},
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
	if samples, matched := config.patternSamples(key); matched {
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

// Freshness is how current a provider's reported state is: when it was read,
// how long ago that was, and whether that is longer than the install allows.
//
// It exists because two very different windows both look like "Reactor is
// acting on something that is no longer true", and only one of them is bounded.
// A value that CHANGED reaches every Automation within one poll interval times
// the samples its key has to hold for — a window the operator set, on both
// terms. A provider that stops answering has no such window: the store keeps
// reporting what it last saw, for as long as that lasts. This reports the
// second one so it can be said out loud rather than inferred from a graph.
type Freshness struct {
	// ObservedAt is when the reported state was read from the provider.
	ObservedAt time.Time
	// Age is how long ago that was.
	Age time.Duration
	// Bound is the configured maximum age, or zero when none is configured.
	Bound time.Duration
	// Stale reports an Age past Bound. Always false when Bound is zero: an
	// install that set no bound is not being told its state is too old.
	Stale bool
}

// Freshness reports how current a provider's reported state is. ok is false
// when the provider has never reported anything, which is a different state
// from stale and is reported differently: nothing has been observed at all, so
// there is no decision being taken against something out of date.
func (s *StateStore) Freshness(provider string) (Freshness, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	current, ok := s.current[provider]
	if !ok {
		return Freshness{}, false
	}
	freshness := Freshness{
		ObservedAt: current.observedAt,
		Age:        time.Since(current.observedAt),
		Bound:      s.staleAfter[provider],
	}
	freshness.Stale = freshness.Bound > 0 && freshness.Age > freshness.Bound
	return freshness, true
}
