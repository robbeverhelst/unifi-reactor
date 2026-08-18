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
	"context"
	"encoding/json"
	"testing"

	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The uplink indices behind the capture's uptime_stats keys: WAN is the
// primary, WAN2 the wired backup, WAN3 the cellular one. The tests thread them
// explicitly because that is the point of #107 — the wan value alone cannot
// name a backup, so every believed uplink is named by its index.
const (
	wiredBackupIndex    = 2
	cellularBackupIndex = 3
)

// disagreements reads reactor_provider_signal_disagreements_total for one
// signal. It gathers from the registry rather than reaching into the metrics
// package, because the counter is deliberately unexported: what a test may
// assert on is the series an operator would actually see.
func disagreements(t *testing.T, signal string) float64 {
	t.Helper()
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "reactor_provider_signal_disagreements_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "signal" && label.GetValue() == signal {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// capturedHealth loads the committed stat/health response. Every derivation
// below starts from it and edits a decoded copy, so what is being tested is
// always a transformation of a real payload rather than a hand-written one.
func capturedHealth(t *testing.T) healthResponse {
	t.Helper()
	var health healthResponse
	if err := json.Unmarshal(captured(t, "stat-health.json"), &health); err != nil {
		t.Fatalf("parsing stat-health.json: %v", err)
	}
	return health
}

// subsystem returns a pointer to one subsystem of a decoded capture, so a test
// can move a single field and leave the rest of the real response intact.
func subsystem(t *testing.T, health *healthResponse, name string) *healthSubsystem {
	t.Helper()
	for i := range health.Data {
		if health.Data[i].Subsystem == name {
			return &health.Data[i]
		}
	}
	t.Fatalf("the capture has no %q subsystem", name)
	return nil
}

// mergedHealth runs mergeHealth as Observe would: with the wan value and the
// uplink index it resolved from, which are two separate facts — "backup" alone
// cannot say which backup (#107).
func mergedHealth(t *testing.T, c *Client, health healthResponse, wan string, wanIndex int) map[string]string {
	t.Helper()
	state := map[string]string{}
	c.mergeHealth(context.Background(), state, health, wan, wanIndex)
	return state
}

// The capture was taken with the internet up and WAN1 at 100% availability,
// 16 ms average. Both keys should read the healthy value from it unedited.
func TestHealthAgainstTheCapture(t *testing.T) {
	state := mergedHealth(t, NewClient("", nil, "", false), capturedHealth(t), wanPrimary, wanPrimaryIndex)

	for key, want := range map[string]string{
		stateKeyInternet:   internetOK,
		stateKeyWANQuality: wanQualityGood,
	} {
		if state[key] != want {
			t.Errorf("state[%q] = %q, want %q", key, state[key], want)
		}
	}
}

func TestInternetFromTheWWWSubsystem(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{healthStatusOK, internetOK},
		{healthStatusWarning, internetDegraded},
		{healthStatusError, internetDown},
	}
	c := NewClient("", nil, "", false)
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			health := capturedHealth(t)
			subsystem(t, &health, healthSubsystemWWW).Status = tc.status

			if got := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)[stateKeyInternet]; got != tc.want {
				t.Errorf("state[internet] = %q, want %q", got, tc.want)
			}
		})
	}
}

// A status this provider does not recognise must not be translated into a
// value. The whole point of internet is that an Automation believes it, and a
// firmware that renamed "error" must not have it read as "the internet is
// fine" — omitting the key holds whatever was last matched instead.
func TestUnrecognisedInternetStatusOmitsTheKey(t *testing.T) {
	c := NewClient("", nil, "", false)
	for _, status := range []string{"unknown", "", "degraded-somehow"} {
		health := capturedHealth(t)
		subsystem(t, &health, healthSubsystemWWW).Status = status

		if got, present := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)[stateKeyInternet]; present {
			t.Errorf("status %q should publish no internet key, got %q", status, got)
		}
	}
}

// Per-key degradation, on the health half: a console that reports no www
// subsystem at all still reports wan.quality, and vice versa.
func TestHealthKeysDegradeIndependently(t *testing.T) {
	c := NewClient("", nil, "", false)

	t.Run("no www subsystem", func(t *testing.T) {
		health := capturedHealth(t)
		health.Data = without(health.Data, healthSubsystemWWW)

		state := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)
		if _, present := state[stateKeyInternet]; present {
			t.Error("internet should be absent when the console reports no www subsystem")
		}
		if state[stateKeyWANQuality] != wanQualityGood {
			t.Errorf("state[wan.quality] = %q, want %q", state[stateKeyWANQuality], wanQualityGood)
		}
	})

	t.Run("no wan subsystem", func(t *testing.T) {
		health := capturedHealth(t)
		health.Data = without(health.Data, healthSubsystemWAN)

		state := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)
		if state[stateKeyInternet] != internetOK {
			t.Errorf("state[internet] = %q, want %q", state[stateKeyInternet], internetOK)
		}
		if _, present := state[stateKeyWANQuality]; present {
			t.Error("wan.quality should be absent when the console reports no wan subsystem")
		}
	})
}

func without(data []healthSubsystem, name string) []healthSubsystem {
	kept := make([]healthSubsystem, 0, len(data))
	for _, s := range data {
		if s.Subsystem != name {
			kept = append(kept, s)
		}
	}
	return kept
}

// wan.quality is the quality of the live uplink, so it needs to know which one
// that is. Guessing would publish the backup's numbers under the primary's
// name — in the capture, the backup reads 0% available, which would report a
// perfectly good link as degraded.
func TestWANQualityNeedsToKnowWhichUplinkIsLive(t *testing.T) {
	c := NewClient("", nil, "", false)

	if got, present := mergedHealth(t, c, capturedHealth(t), "", 0)[stateKeyWANQuality]; present {
		t.Errorf("wan.quality should be absent when wan is not derivable, got %q", got)
	}
	// The same capture read as if the backup were live: WAN2 has been down for
	// the whole window, and its monitors say so.
	if got := mergedHealth(t, c, capturedHealth(t), wanBackup, wiredBackupIndex)[stateKeyWANQuality]; got != wanQualityDegraded {
		t.Errorf("state[wan.quality] on the dead backup = %q, want %q", got, wanQualityDegraded)
	}
	// A wan that was guessed rather than resolved — the ambiguous-claim path —
	// names no uplink either. Publishing WAN2's numbers there would be the
	// same two-WAN assumption #107 removed from the cross-check.
	if got, present := mergedHealth(t, c, capturedHealth(t), wanBackup, 0)[stateKeyWANQuality]; present {
		t.Errorf("wan.quality should be absent when wan was guessed rather than resolved, got %q", got)
	}
}

func TestWANQualityThresholds(t *testing.T) {
	availability, latency := 99.5, 40.0
	tests := []struct {
		name                    string
		minAvailability, maxLat float64
		availability, latency   *float64
		want                    string
	}{
		{"good on both counts", 99, 150, &availability, &latency, wanQualityGood},
		{"packet loss past the threshold", 99.9, 150, &availability, &latency, wanQualityDegraded},
		{"latency past the threshold", 99, 20, &availability, &latency, wanQualityDegraded},
		{"exactly at both thresholds is still good", 99.5, 40, &availability, &latency, wanQualityGood},
		// A link with no latency reading is judged on availability alone: the
		// capture omits latency_average on every uplink not carrying traffic,
		// and a missing measurement is not a bad one.
		{"no latency reported", 99, 1, &availability, nil, wanQualityGood},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient("", nil, "", false)
			c.MinAvailabilityPercent, c.MaxLatencyMs = tc.minAvailability, tc.maxLat

			health := capturedHealth(t)
			entry := subsystem(t, &health, healthSubsystemWAN).UptimeStats[wanStatusKeyPrimary]
			entry.Availability, entry.LatencyAverage = tc.availability, tc.latency
			entry.Monitors, entry.AlertingMonitors = nil, nil
			subsystem(t, &health, healthSubsystemWAN).UptimeStats[wanStatusKeyPrimary] = entry

			if got := mergedHealth(t, c, health, wanPrimary, wanPrimaryIndex)[stateKeyWANQuality]; got != tc.want {
				t.Errorf("state[wan.quality] = %q, want %q", got, tc.want)
			}
		})
	}
}

// The single most dangerous way this parser could be wrong. The console omits
// these fields rather than sending zero — the capture proves it, since WAN2
// and WAN3 carry no uplink-level availability at all — so decoding an absent
// field as 0 would report a healthy link as degraded and shed a cluster's load
// on a truncated response.
func TestMissingAvailabilityIsNotZeroAvailability(t *testing.T) {
	health := capturedHealth(t)
	entry := subsystem(t, &health, healthSubsystemWAN).UptimeStats[wanStatusKeyPrimary]
	entry.Availability, entry.LatencyAverage = nil, nil
	entry.Monitors, entry.AlertingMonitors = nil, nil
	subsystem(t, &health, healthSubsystemWAN).UptimeStats[wanStatusKeyPrimary] = entry

	state := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)
	if got, present := state[stateKeyWANQuality]; present {
		t.Fatalf("an entry reporting no availability must publish no wan.quality, got %q", got)
	}
}

// Where the uplink-level summary is missing, the per-monitor availabilities
// stand in — they are present on every uplink in the capture, including the
// dead ones, so a link bad enough to lose its summary still produces a reading.
func TestAvailabilityFallsBackToTheMonitors(t *testing.T) {
	health := capturedHealth(t)
	wanSubsystem := subsystem(t, &health, healthSubsystemWAN)
	entry := wanSubsystem.UptimeStats[wanStatusKeyPrimary]
	entry.Availability, entry.LatencyAverage = nil, nil
	wanSubsystem.UptimeStats[wanStatusKeyPrimary] = entry

	// The capture's WAN monitors all read 100%, so it stays good...
	state := mergedHealth(t, NewClient("", nil, "", false), health, wanPrimary, wanPrimaryIndex)
	if state[stateKeyWANQuality] != wanQualityGood {
		t.Errorf("state[wan.quality] = %q, want %q", state[stateKeyWANQuality], wanQualityGood)
	}
	// ...and the captured WAN2 entry, which has no uplink-level fields at all,
	// still reads degraded off its monitors alone.
	if got := mergedHealth(t, NewClient("", nil, "", false), health, wanBackup, wiredBackupIndex)[stateKeyWANQuality]; got != wanQualityDegraded {
		t.Errorf("state[wan.quality] from monitors alone = %q, want %q", got, wanQualityDegraded)
	}
}

// The third opinion on issue #34. Uptime is traffic the console watched pass,
// where is_uplink and uplink.name are both statements about configuration, so
// uptime accumulating on a port other than the one wan names is the strongest
// evidence available that the mapping is wrong.
func TestUplinkHealthCrossCheck(t *testing.T) {
	seconds := func(v int) *int { return &v }
	tests := []struct {
		name          string
		wan           string
		wanIndex      int
		uptime        map[string]*int
		wantDisagrees bool
	}{
		{"the capture, believed correctly", wanPrimary, wanPrimaryIndex, nil, false},
		{"believed to be on the backup while the primary has the uptime", wanBackup, wiredBackupIndex, nil, true},
		{
			name:     "believed to be on the primary while the backup has the uptime",
			wan:      wanPrimary,
			wanIndex: wanPrimaryIndex,
			uptime: map[string]*int{
				wanStatusKeyPrimary: nil,
				wanStatusKeyBackup:  seconds(98787),
			},
			wantDisagrees: true,
		},
		{
			// Mid-switchover: nothing has uptime yet. There is no evidence
			// either way, so this must stay quiet rather than cry wolf on
			// every failover.
			name:     "no uplink has uptime",
			wan:      wanPrimary,
			wanIndex: wanPrimaryIndex,
			uptime: map[string]*int{
				wanStatusKeyPrimary: nil,
				wanStatusKeyBackup:  nil,
			},
		},
		{
			// Both up: the believed uplink is among them, so nothing is wrong.
			name:     "both uplinks have uptime",
			wan:      wanBackup,
			wanIndex: wiredBackupIndex,
			uptime: map[string]*int{
				wanStatusKeyPrimary: seconds(98787),
				wanStatusKeyBackup:  seconds(4200),
			},
		},
		{
			// The false positive #107 is about, as it fired live: wan resolved
			// to backup FROM THE THIRD UPLINK, and the health endpoint agrees —
			// the uptime sits under WAN3. Deriving the believed key from the
			// value compared WAN2 against {WAN3} and reported the mapping as
			// broken at the exact moment it was right, once per poll.
			name:     "the live backup is the third uplink and health agrees",
			wan:      wanBackup,
			wanIndex: cellularBackupIndex,
			uptime: map[string]*int{
				wanStatusKeyPrimary: nil,
				"WAN3":              seconds(75),
			},
		},
		{
			// The disagreement worth keeping — the one the fix must not weaken
			// away: wan names the third uplink while only the primary has
			// uptime. "Any backup with uptime counts" would stay quiet here,
			// and here is precisely a mapping pointing at the wrong backup.
			name:          "believed to be on the third uplink while the primary has the uptime",
			wan:           wanBackup,
			wanIndex:      cellularBackupIndex,
			wantDisagrees: true,
		},
		{
			// wan was guessed off an ambiguous claim, so no index resolved.
			// There is no believed uplink precise enough to check against, and
			// the guess was already reported where it was made.
			name:     "wan was guessed and no index resolved",
			wan:      wanBackup,
			wanIndex: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			health := capturedHealth(t)
			stats := subsystem(t, &health, healthSubsystemWAN).UptimeStats
			for key, uptime := range tc.uptime {
				entry := stats[key]
				entry.Uptime = uptime
				stats[key] = entry
			}

			before := disagreements(t, signalWANHealthDisagrees)
			mergedHealth(t, NewClient("", nil, "", false), health, tc.wan, tc.wanIndex)
			after := disagreements(t, signalWANHealthDisagrees)

			if got := after > before; got != tc.wantDisagrees {
				t.Errorf("disagreement reported = %v, want %v", got, tc.wantDisagrees)
			}
		})
	}
}
