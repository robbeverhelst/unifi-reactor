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

package engine

import (
	"testing"

	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

func observe(s *StateStore, state map[string]string) []Transition {
	return s.Observe(events.Observation{Provider: "unifi", State: state})
}

func TestStateStoreFirstObservationReportsAllKeys(t *testing.T) {
	s := NewStateStore()
	got := observe(s, map[string]string{"wan": "primary"})
	if len(got) != 1 || got[0] != (Transition{Provider: "unifi", Key: "wan", From: "", To: "primary"}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

func TestStateStoreRepeatedObservationIsNoOp(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{"wan": "backup"})
	for i := 0; i < 3; i++ {
		if got := observe(s, map[string]string{"wan": "backup"}); len(got) != 0 {
			t.Fatalf("repeat %d: expected no transitions, got %+v", i, got)
		}
	}
}

func TestStateStoreDetectsTransition(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{"wan": "primary"})
	got := observe(s, map[string]string{"wan": "backup"})
	if len(got) != 1 || got[0] != (Transition{Provider: "unifi", Key: "wan", From: "primary", To: "backup"}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

func TestStateStoreReportsDisappearedKey(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{"wan": "primary", "device.udm": "online"})
	got := observe(s, map[string]string{"wan": "primary"})
	if len(got) != 1 || got[0] != (Transition{Provider: "unifi", Key: "device.udm", From: "online", To: ""}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

func TestStateStoreProvidersAreIndependent(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{"wan": "primary"})
	got := s.Observe(events.Observation{Provider: "nut", State: map[string]string{"ups": "online"}})
	if len(got) != 1 || got[0] != (Transition{Provider: "nut", Key: "ups", From: "", To: "online"}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
	if _, ok := s.Get("unifi"); !ok {
		t.Fatal("unifi observation lost")
	}
}
