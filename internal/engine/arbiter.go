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

import "slices"

// Intent is one claimant's desired level for a shared target.
//
// Level is an action-specific scalar — a replica count today — and the engine
// never interprets it beyond ordering: lower is more restrictive. Anything
// that cannot be expressed as such a scalar is not arbitrated at all; it is an
// edge action that fires on its own transitions and owns nothing.
type Intent struct {
	// Claimant identifies who wants this. Opaque to the engine, used only for
	// reporting which claims won and which were outvoted.
	Claimant string
	// Level is the desired value. Lower is more restrictive.
	Level int64
}

// Resolution is the arbitrated outcome for one target.
type Resolution struct {
	// Level is the resolved value: the most restrictive level asked for.
	Level int64
	// Winners asked for exactly the resolved level.
	Winners []string
	// Deferred asked for a less restrictive level and did not get it.
	Deferred []string
}

// Resolve arbitrates competing intents for one target by taking the meet: the
// most restrictive level any claimant asked for.
//
// The result is a pure function of the set of intents. It does not depend on
// the order they arrive in, nor on the order the claimants were evaluated,
// which is exactly what makes a target's value independent of reconcile
// ordering when several claimants disagree.
//
// ok is false when nobody claims the target. That is a distinct state from
// "everybody asked for zero": an unclaimed target is one nothing should be
// asserting a value for at all, and the caller must stop writing to it rather
// than hold it at some resolved level.
func Resolve(intents []Intent) (Resolution, bool) {
	if len(intents) == 0 {
		return Resolution{}, false
	}

	resolution := Resolution{Level: intents[0].Level}
	for _, intent := range intents[1:] {
		resolution.Level = min(resolution.Level, intent.Level)
	}
	for _, intent := range intents {
		if intent.Level == resolution.Level {
			resolution.Winners = append(resolution.Winners, intent.Claimant)
			continue
		}
		resolution.Deferred = append(resolution.Deferred, intent.Claimant)
	}

	// Sorted so status fields and log lines stay stable across reconciles that
	// gathered the same claims in a different order.
	slices.Sort(resolution.Winners)
	slices.Sort(resolution.Deferred)
	return resolution, true
}
