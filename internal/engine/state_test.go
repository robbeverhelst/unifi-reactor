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

const (
	testProvider = "unifi"
	wanPrimary   = "primary"
	wanBackup    = "backup"
	deviceOnline = "online"
	keyWAN       = "wan"
)

func observe(s *StateStore, state map[string]string) []Transition {
	return s.Observe(events.Observation{Provider: testProvider, State: state})
}

func TestStateStoreFirstObservationReportsAllKeys(t *testing.T) {
	s := NewStateStore()
	got := observe(s, map[string]string{keyWAN: wanPrimary})
	if len(got) != 1 || got[0] != (Transition{Provider: testProvider, Key: keyWAN, From: "", To: wanPrimary}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

func TestStateStoreRepeatedObservationIsNoOp(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{keyWAN: wanBackup})
	for i := range 3 {
		if got := observe(s, map[string]string{keyWAN: wanBackup}); len(got) != 0 {
			t.Fatalf("repeat %d: expected no transitions, got %+v", i, got)
		}
	}
}

func TestStateStoreDetectsTransition(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{keyWAN: wanPrimary})
	got := observe(s, map[string]string{keyWAN: wanBackup})
	if len(got) != 1 || got[0] != (Transition{Provider: testProvider, Key: keyWAN, From: wanPrimary, To: wanBackup}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

func TestStateStoreReportsDisappearedKey(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{keyWAN: wanPrimary, "device.udm": deviceOnline})
	got := observe(s, map[string]string{keyWAN: wanPrimary})
	if len(got) != 1 || got[0] != (Transition{Provider: testProvider, Key: "device.udm", From: deviceOnline, To: ""}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

func TestStateStoreProvidersAreIndependent(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{keyWAN: wanPrimary})
	got := s.Observe(events.Observation{Provider: "nut", State: map[string]string{"ups": deviceOnline}})
	if len(got) != 1 || got[0] != (Transition{Provider: "nut", Key: "ups", From: "", To: deviceOnline}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
	if _, ok := s.Get(testProvider); !ok {
		t.Fatal("unifi observation lost")
	}
}
