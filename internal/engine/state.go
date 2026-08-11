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
	"sync"

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

// StateStore keeps the latest observation per provider and derives
// transitions by comparing consecutive observations. Repeated identical
// observations produce no transitions.
type StateStore struct {
	mu      sync.RWMutex
	current map[string]events.Observation
}

func NewStateStore() *StateStore {
	return &StateStore{current: map[string]events.Observation{}}
}

// Observe stores the observation and returns the transitions relative to the
// previous observation for the same provider. The first observation for a
// provider reports every key as a transition from "".
func (s *StateStore) Observe(o events.Observation) []Transition {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.current[o.Provider].State
	var transitions []Transition
	for key, to := range o.State {
		if from := prev[key]; from != to {
			transitions = append(transitions, Transition{Provider: o.Provider, Key: key, From: from, To: to})
		}
	}
	for key, from := range prev {
		if _, still := o.State[key]; !still {
			transitions = append(transitions, Transition{Provider: o.Provider, Key: key, From: from})
		}
	}
	s.current[o.Provider] = o
	return transitions
}

// Get returns the latest observation for a provider.
func (s *StateStore) Get(provider string) (events.Observation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.current[provider]
	return o, ok
}
