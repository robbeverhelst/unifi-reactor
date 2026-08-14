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

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	testProvider = "testprovider"

	keyLink    = "link"
	keyPower   = "power"
	keyCarrier = "carrier"

	linkPrimary = "primary"
	linkBackup  = "backup"
	powerOnline = "online"
	powerBatt   = "on-battery"
)

// testVocabulary mirrors the shape of a real provider's: closed value sets for
// the keys that have them, and nothing at all for the key whose values are an
// open set.
func testVocabulary() map[string][]string {
	return map[string][]string{
		keyLink:  {linkPrimary, linkBackup},
		keyPower: {powerOnline, powerBatt},
	}
}

// reset clears the package-level state so each test starts from a process that
// has just booted.
func reset(t *testing.T) {
	t.Helper()
	stateInfo.Reset()
	transitions.Reset()
	automationMatching.Reset()
	automationReady.Reset()
	lastObservation.Reset()
	reactionLatency.Reset()

	vocabulary.Lock()
	defer vocabulary.Unlock()
	vocabulary.declared = map[string]map[string][]string{}
	vocabulary.seen = map[string]map[string]bool{}
}

// stateValue reads one published series. Reading creates it if it is absent, so
// every test that cares about which series exist asserts the count first.
func stateValue(t *testing.T, key, value string) float64 {
	t.Helper()
	return testutil.ToFloat64(stateInfo.WithLabelValues(testProvider, key, value))
}

// TestOnlyDeclaredValuesBecomeSeries is the cardinality guard, and the reason
// the vocabulary exists at all: a key whose values are an open set — a carrier
// name, a hostname, anything derived from the outside world — must never reach
// a label, or one observation per new value becomes one time series per new
// value, kept for a whole retention period.
func TestOnlyDeclaredValuesBecomeSeries(t *testing.T) {
	reset(t)
	SetVocabulary(testProvider, testVocabulary())

	// The undeclared key takes a different value on every observation, which is
	// exactly the shape that blows an instance up if it is ever labelled.
	for _, carrier := range []string{"carrier-a", "carrier-b", "carrier-c", "unknown"} {
		ObservationSucceeded(testProvider, map[string]string{
			keyLink:    linkPrimary,
			keyPower:   powerOnline,
			keyCarrier: carrier,
		}, time.Now())
	}

	// Four observations, four distinct undeclared values, and still exactly the
	// two declared keys times their two declared values.
	if got := testutil.CollectAndCount(stateInfo); got != 4 {
		t.Fatalf("expected exactly the 4 declared values to be series, got %d", got)
	}
}

// TestActiveValueIsOneAndTheRestAreZero covers why the vocabulary is needed at
// all rather than just publishing the current value: without the zeroes, the
// series for a value a key no longer holds goes stale at 1 instead of dropping,
// and every graph built on it lies.
func TestActiveValueIsOneAndTheRestAreZero(t *testing.T) {
	reset(t)
	SetVocabulary(testProvider, testVocabulary())

	ObservationSucceeded(testProvider, map[string]string{keyLink: linkPrimary, keyPower: powerOnline}, time.Now())
	if got := stateValue(t, keyLink, linkPrimary); got != 1 {
		t.Errorf("the value the key holds reports %v, want 1", got)
	}
	if got := stateValue(t, keyLink, linkBackup); got != 0 {
		t.Errorf("a value the key does not hold reports %v, want 0", got)
	}

	ObservationSucceeded(testProvider, map[string]string{keyLink: linkBackup, keyPower: powerBatt}, time.Now())
	if got := stateValue(t, keyLink, linkPrimary); got != 0 {
		t.Errorf("the previous value stayed at %v after a transition, want 0", got)
	}
	if got := stateValue(t, keyLink, linkBackup); got != 1 {
		t.Errorf("the new value reports %v, want 1", got)
	}
	if got := stateValue(t, keyPower, powerBatt); got != 1 {
		t.Errorf("the second key did not follow: %v, want 1", got)
	}
}

// TestAnUnobservedKeyPublishesNothing keeps the metric honest about hardware
// that is not there. A provider declares its whole vocabulary even on a site
// with no UPS adopted; a row of zeroes would read as "observed, none of these",
// which is a different claim from "cannot see it".
func TestAnUnobservedKeyPublishesNothing(t *testing.T) {
	reset(t)
	SetVocabulary(testProvider, testVocabulary())

	ObservationSucceeded(testProvider, map[string]string{keyLink: linkPrimary}, time.Now())

	if got := testutil.CollectAndCount(stateInfo); got != 2 {
		t.Fatalf("expected only the observed key's 2 values, got %d series", got)
	}
}

// TestAVanishedKeyDropsToZero is the metric side of StateKeyUnavailable: the
// hardware reporting a key dropped off the controller, so nothing should still
// be reporting the value it last held as the one it currently holds.
func TestAVanishedKeyDropsToZero(t *testing.T) {
	reset(t)
	SetVocabulary(testProvider, testVocabulary())

	ObservationSucceeded(testProvider, map[string]string{keyLink: linkPrimary, keyPower: powerBatt}, time.Now())
	ObservationSucceeded(testProvider, map[string]string{keyLink: linkPrimary}, time.Now())

	if got := stateValue(t, keyPower, powerBatt); got != 0 {
		t.Errorf("a key that vanished still reports %v, want 0", got)
	}
	if got := stateValue(t, keyLink, linkPrimary); got != 1 {
		t.Errorf("the key still being observed reports %v, want 1", got)
	}
}

// TestObservationFailureLeavesTheTimestampAlone protects the one metric the
// whole staleness alert rests on. A failed poll must not look like a fresh
// observation: how long Reactor has been blind is measured from the last time
// it could actually see.
func TestObservationFailureLeavesTheTimestampAlone(t *testing.T) {
	reset(t)

	at := time.Unix(1_700_000_000, 0)
	ObservationSucceeded(testProvider, nil, at)
	ObservationFailed(testProvider)

	if got := testutil.ToFloat64(lastObservation.WithLabelValues(testProvider)); got != float64(at.Unix()) {
		t.Fatalf("a failed observation moved the freshness gauge to %v", got)
	}
}

// TestForgettingAnAutomationDropsItsSeries is what keeps namespace/name a
// self-limiting label set rather than an unbounded one: a deleted policy stops
// being reported instead of reporting a condition it no longer has forever.
func TestForgettingAnAutomationDropsItsSeries(t *testing.T) {
	reset(t)

	AutomationEvaluated("media", "pause-on-backup-wan", true, true)
	if got := testutil.CollectAndCount(automationMatching); got != 1 {
		t.Fatalf("expected 1 series, got %d", got)
	}

	ForgetAutomation("media", "pause-on-backup-wan")
	if got := testutil.CollectAndCount(automationMatching); got != 0 {
		t.Fatalf("a deleted Automation still reports %d series", got)
	}
}

// TestReactionLatencyIgnoresAnUnobservedTime stops an Automation that acts on
// state it never saw from recording decades of latency into the histogram.
func TestReactionLatencyIgnoresAnUnobservedTime(t *testing.T) {
	reset(t)

	ReactionCompleted(testProvider, time.Time{})
	if got := testutil.CollectAndCount(reactionLatency); got != 0 {
		t.Fatalf("a zero observation time was recorded as a reaction, giving %d series", got)
	}
}

// TestStateIsNotPublishedForAnUndeclaredProvider keeps a provider that has not
// declared a vocabulary from publishing value-labelled series by accident,
// which is the failure mode this whole mechanism exists to make impossible.
func TestStateIsNotPublishedForAnUndeclaredProvider(t *testing.T) {
	reset(t)

	ObservationSucceeded("undeclared", map[string]string{keyLink: linkPrimary}, time.Now())

	if got := testutil.CollectAndCount(stateInfo); got != 0 {
		t.Fatalf("a provider with no declared vocabulary published %d series", got)
	}
}
