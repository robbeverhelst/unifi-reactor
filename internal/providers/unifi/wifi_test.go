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

package unifi

import (
	"testing"
)

// counts sets the wlan subsystem's AP counts on a decoded capture.
func counts(t *testing.T, health *healthResponse, adopted, disconnected, connected int) {
	t.Helper()
	wlan := subsystem(t, health, healthSubsystemWLAN)
	wlan.NumAdopted, wlan.NumDisconnected, wlan.NumAP = &adopted, &disconnected, &connected
}

// The capture carries 4 APs adopted, 1 disconnected and 3 connected, with the
// console calling the subsystem "warning". Both signals agree on that reading,
// which is the whole reason the counts are trusted as the sharper of the two
// rather than as a contradiction of it.
//
// The numbers themselves are a placeholder site — hack/capture-unifi.sh scales
// every count in a capture, because how many access points somebody owns is
// not API shape. What survives the scaling is what this test is about: the
// arithmetic, and that none/some/all still lands where it did.
func TestWiFiAgainstTheCapture(t *testing.T) {
	health := capturedHealth(t)
	wlan := subsystem(t, &health, healthSubsystemWLAN)
	if wlan.Status != healthStatusWarning {
		t.Fatalf("the capture's wlan status is %q, expected %q — this test is written against it",
			wlan.Status, healthStatusWarning)
	}
	if wlan.NumAdopted == nil || *wlan.NumAdopted != 4 ||
		wlan.NumDisconnected == nil || *wlan.NumDisconnected != 1 ||
		wlan.NumAP == nil || *wlan.NumAP != 3 {
		t.Fatalf("the capture's wlan counts are not the 4/1/3 this test is written against: %+v", wlan)
	}

	before := disagreements(t, signalWiFiStatusDisagrees)
	state := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)

	if state[stateKeyWiFi] != wifiWarning {
		t.Errorf("state[wifi] = %q, want %q", state[stateKeyWiFi], wifiWarning)
	}
	if after := disagreements(t, signalWiFiStatusDisagrees); after != before {
		t.Errorf("the capture's two signals agree, so nothing should be counted: %v -> %v", before, after)
	}
}

func TestWiFiFromTheAPCounts(t *testing.T) {
	tests := []struct {
		name                  string
		adopted, disconnected int
		want                  string
	}{
		{"every AP connected", 3, 0, wifiOK},
		{"one of three gone", 3, 1, wifiWarning},
		{"two of three gone", 3, 2, wifiWarning},
		{"every AP gone", 3, 3, wifiError},
		{"the only AP gone", 1, 1, wifiError},
		{"a single healthy AP", 1, 0, wifiOK},
	}
	c := NewClient("", nil, "", false)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			health := capturedHealth(t)
			counts(t, &health, tc.adopted, tc.disconnected, tc.adopted-tc.disconnected)
			// The console's own status is moved out of the way, so this test is
			// about the derivation rather than about the cross-check.
			subsystem(t, &health, healthSubsystemWLAN).Status = ""

			if got := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)[stateKeyWiFi]; got != tc.want {
				t.Errorf("state[wifi] = %q with %d adopted and %d disconnected, want %q",
					got, tc.adopted, tc.disconnected, tc.want)
			}
		})
	}
}

// A site with no access points has no WiFi to be healthy or unhealthy. Omit
// what you cannot see: zero adopted is not ok.
func TestNoAccessPointsPublishesNoWiFiKey(t *testing.T) {
	health := capturedHealth(t)
	counts(t, &health, 0, 0, 0)

	if got, present := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)[stateKeyWiFi]; present {
		t.Errorf("a site with no adopted AP should publish no wifi key, got %q", got)
	}
}

// Absent is not zero, on the health half: a subsystem reporting no counts is
// not a subsystem with no APs, and it must not be read as either "ok" or "every
// AP is down".
func TestMissingAPCountsPublishNoWiFiKey(t *testing.T) {
	c := NewClient("", nil, "", false)
	for _, drop := range []struct {
		name                  string
		adopted, disconnected bool
	}{
		{"no adopted count", true, false},
		{"no disconnected count", false, true},
		{"neither count", true, true},
	} {
		t.Run(drop.name, func(t *testing.T) {
			health := capturedHealth(t)
			wlan := subsystem(t, &health, healthSubsystemWLAN)
			if drop.adopted {
				wlan.NumAdopted = nil
			}
			if drop.disconnected {
				wlan.NumDisconnected = nil
			}

			if got, present := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)[stateKeyWiFi]; present {
				t.Errorf("missing counts should publish no wifi key, got %q", got)
			}
		})
	}
}

// A console whose wlan subsystem disappears entirely — the same per-key
// degradation a UPS dropping off produces. internet and wan.quality are
// unaffected, and an Automation matching wifi holds its last known state.
func TestWiFiDegradesWithoutTakingTheOtherHealthKeys(t *testing.T) {
	health := capturedHealth(t)
	var kept []healthSubsystem
	for _, s := range health.Data {
		if s.Subsystem != healthSubsystemWLAN {
			kept = append(kept, s)
		}
	}
	health.Data = kept

	state := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)
	if got, present := state[stateKeyWiFi]; present {
		t.Errorf("wifi should be absent when the wlan subsystem is, got %q", got)
	}
	for key, want := range map[string]string{
		stateKeyInternet:   internetOK,
		stateKeyWANQuality: wanQualityGood,
	} {
		if state[key] != want {
			t.Errorf("state[%q] = %q, want %q: the other health keys are a separate observation", key, state[key], want)
		}
	}
}

// The console's own status is a second, independent opinion. When it and the
// counts disagree the counts win — they are the half that can be explained —
// and the disagreement is counted rather than swallowed, so a firmware whose
// "warning" means something else announces itself.
func TestTheConsolesOwnStatusIsCrossCheckedRatherThanTrusted(t *testing.T) {
	before := disagreements(t, signalWiFiStatusDisagrees)

	health := capturedHealth(t)
	counts(t, &health, 3, 0, 3)
	subsystem(t, &health, healthSubsystemWLAN).Status = healthStatusWarning

	state := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)
	if state[stateKeyWiFi] != wifiOK {
		t.Errorf("state[wifi] = %q, want %q: every adopted AP is connected", state[stateKeyWiFi], wifiOK)
	}
	if after := disagreements(t, signalWiFiStatusDisagrees); after <= before {
		t.Errorf("the disagreement should be counted, %v -> %v", before, after)
	}
}

// An unfamiliar status is not a disagreement — it is a vocabulary this provider
// has never seen — and the counts stand on their own regardless.
func TestAnUnfamiliarWiFiStatusIsNotADisagreement(t *testing.T) {
	before := disagreements(t, signalWiFiStatusDisagrees)

	health := capturedHealth(t)
	counts(t, &health, 2, 0, 2)
	subsystem(t, &health, healthSubsystemWLAN).Status = "degraded-somehow"

	if got := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)[stateKeyWiFi]; got != wifiOK {
		t.Errorf("state[wifi] = %q, want %q", got, wifiOK)
	}
	if after := disagreements(t, signalWiFiStatusDisagrees); after != before {
		t.Errorf("an unrecognised status is not a disagreement, %v -> %v", before, after)
	}
}
