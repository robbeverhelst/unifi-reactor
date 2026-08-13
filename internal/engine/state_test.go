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
	keyBattery   = "ups.battery"
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

func debounced(samples int, perKey map[string]int) *StateStore {
	return NewStateStore(WithDebounce(testProvider, DebounceConfig{Default: samples, PerKey: perKey}))
}

// The point of #30: a signal that oscillates must not drive one action per
// flap. Presence keys flap by nature, and kubernetes.restart or a PoE cycle
// make that actively harmful.
func TestStateStoreDebounceCollapsesFlapping(t *testing.T) {
	s := debounced(3, nil)
	observe(s, map[string]string{keyWAN: wanPrimary})

	var reported int
	for _, value := range []string{wanBackup, wanPrimary, wanBackup, wanPrimary, wanBackup} {
		reported += len(observe(s, map[string]string{keyWAN: value}))
	}
	if reported != 0 {
		t.Fatalf("a flapping value reported %d transitions, want none", reported)
	}
	if got, _ := s.Get(testProvider); got.State[keyWAN] != wanPrimary {
		t.Fatalf("state = %q, want the last settled value %q", got.State[keyWAN], wanPrimary)
	}
}

func TestStateStoreDebouncePromotesAfterEnoughSamples(t *testing.T) {
	s := debounced(3, nil)
	observe(s, map[string]string{keyWAN: wanPrimary})

	for i := range 2 {
		if got := observe(s, map[string]string{keyWAN: wanBackup}); len(got) != 0 {
			t.Fatalf("sample %d reported %+v, want nothing until the threshold", i+1, got)
		}
		if got, _ := s.Get(testProvider); got.State[keyWAN] != wanPrimary {
			t.Fatalf("sample %d exposed the unconfirmed value", i+1)
		}
	}

	got := observe(s, map[string]string{keyWAN: wanBackup})
	if len(got) != 1 || got[0] != (Transition{Provider: testProvider, Key: keyWAN, From: wanPrimary, To: wanBackup}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

// Debounce must not delay the first reaction after a restart: a key seen for
// the first time has no previous value to oscillate between.
func TestStateStoreDebounceDoesNotDelayFirstObservation(t *testing.T) {
	s := debounced(3, nil)
	if got := observe(s, map[string]string{keyWAN: wanPrimary}); len(got) != 1 {
		t.Fatalf("first observation reported %+v, want it immediately", got)
	}
}

func TestStateStoreDebounceIsPerKey(t *testing.T) {
	s := debounced(1, map[string]int{keyBattery: 2})
	observe(s, map[string]string{keyWAN: wanPrimary, keyBattery: "normal"})

	got := observe(s, map[string]string{keyWAN: wanBackup, keyBattery: "low"})
	if len(got) != 1 || got[0].Key != keyWAN {
		t.Fatalf("transitions = %+v, want only the undebounced key", got)
	}
	if got := observe(s, map[string]string{keyWAN: wanBackup, keyBattery: "low"}); len(got) != 1 {
		t.Fatalf("second sample reported %+v, want the debounced key now", got)
	}
}

// A key going missing means the hardware dropped off, which callers already
// treat as "hold, do not act", so it is reported straight away.
func TestStateStoreDebounceDoesNotDelayDisappearance(t *testing.T) {
	s := debounced(3, nil)
	observe(s, map[string]string{keyWAN: wanPrimary})
	got := observe(s, map[string]string{})
	if len(got) != 1 || got[0] != (Transition{Provider: testProvider, Key: keyWAN, From: wanPrimary}) {
		t.Fatalf("unexpected transitions: %+v", got)
	}
}

// Callers read the returned map outside the store's lock, so it must be a copy.
func TestStateStoreGetReturnsACopy(t *testing.T) {
	s := NewStateStore()
	observe(s, map[string]string{keyWAN: wanPrimary})
	got, _ := s.Get(testProvider)
	got.State[keyWAN] = wanBackup
	if again, _ := s.Get(testProvider); again.State[keyWAN] != wanPrimary {
		t.Fatal("mutating a returned observation changed the store")
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
