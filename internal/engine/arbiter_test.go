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
	"slices"
	"testing"
)

const (
	claimantWAN = "net/pause-on-backup-wan"
	claimantUPS = "power/shed-on-battery"
)

func TestResolveNoIntentsIsUnclaimed(t *testing.T) {
	if _, ok := Resolve(nil); ok {
		t.Fatal("an empty intent set must report unclaimed, not a resolved level")
	}
}

func TestResolveTakesTheMostRestrictiveLevel(t *testing.T) {
	got, ok := Resolve([]Intent{
		{Claimant: claimantWAN, Level: 1},
		{Claimant: claimantUPS, Level: 0},
	})
	if !ok {
		t.Fatal("expected a resolution")
	}
	if got.Level != 0 {
		t.Fatalf("level = %d, want 0", got.Level)
	}
	if !slices.Equal(got.Winners, []string{claimantUPS}) {
		t.Fatalf("winners = %v", got.Winners)
	}
	if !slices.Equal(got.Deferred, []string{claimantWAN}) {
		t.Fatalf("deferred = %v", got.Deferred)
	}
}

// The whole point of the meet: the answer cannot depend on who was evaluated
// first, because that ordering is whatever the informer cache happened to
// return.
func TestResolveIsOrderIndependent(t *testing.T) {
	intents := []Intent{
		{Claimant: "a", Level: 3},
		{Claimant: "b", Level: 0},
		{Claimant: "c", Level: 2},
	}
	want, _ := Resolve(intents)

	for _, permutation := range [][]Intent{
		{intents[2], intents[0], intents[1]},
		{intents[1], intents[2], intents[0]},
		{intents[2], intents[1], intents[0]},
	} {
		got, _ := Resolve(permutation)
		if got.Level != want.Level ||
			!slices.Equal(got.Winners, want.Winners) ||
			!slices.Equal(got.Deferred, want.Deferred) {
			t.Fatalf("permutation %v resolved to %+v, want %+v", permutation, got, want)
		}
	}
}

func TestResolveAgreementLeavesNobodyDeferred(t *testing.T) {
	got, _ := Resolve([]Intent{
		{Claimant: claimantUPS, Level: 0},
		{Claimant: claimantWAN, Level: 0},
	})
	if len(got.Deferred) != 0 {
		t.Fatalf("deferred = %v, want none when every claim agrees", got.Deferred)
	}
	if !slices.Equal(got.Winners, []string{claimantWAN, claimantUPS}) {
		t.Fatalf("winners = %v, want both claimants sorted", got.Winners)
	}
}

func TestResolveSingleIntentWins(t *testing.T) {
	got, ok := Resolve([]Intent{{Claimant: "only", Level: 5}})
	if !ok || got.Level != 5 || !slices.Equal(got.Winners, []string{"only"}) {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}
