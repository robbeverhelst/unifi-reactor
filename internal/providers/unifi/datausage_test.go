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
	"strings"
	"testing"
)

// Every record in this file is built in code rather than committed to
// testdata/. That is a policy decision, not a shortcut: the interesting cases
// are combinations of four booleans, not a realistic device, and a real mbb
// block carries the SIM's iccid, the modem's imei and the PIN retry counters —
// so every committed fixture carrying one is a standing risk for nothing a
// hand-built record does not already prove. No committed fixture has an mbb
// block, and this file is the reason none is needed.

// modem returns a record whose mbb block holds exactly the given SIM slots.
func modem(sims ...simSlot) deviceRecord {
	return deviceRecord{
		Model:           gatewayModel,
		Type:            "udm",
		dataUsageFields: dataUsageFields{MBB: &mbbBlock{SIM: sims}},
	}
}

// planned is a healthy active SIM: card in the slot, plan on the card. The
// tests below flip one field at a time off this baseline.
func planned() simSlot {
	return simSlot{Active: true, Slot: 1, CardPresent: true, HasDataPlan: true}
}

func TestDataUsageFromTheActiveSIM(t *testing.T) {
	warned := planned()
	warned.DataWarning = true
	limited := planned()
	limited.DataLimited = true
	both := planned()
	both.DataWarning, both.DataLimited = true, true

	tests := []struct {
		name string
		sims []simSlot
		want string
	}{
		{"neither flag set is under", []simSlot{planned()}, dataUsageUnder},
		{"data_warning is warning", []simSlot{warned}, dataUsageWarning},
		{"data_limited is over", []simSlot{limited}, dataUsageOver},
		// A reached limit is not also a warning about approaching one.
		{"data_limited wins when both are set", []simSlot{both}, dataUsageOver},
		// A spare SIM's flags must not leak into the answer: the active slot
		// decides, whatever the other one reports.
		{"the inactive SIM's flags are ignored", []simSlot{
			planned(), {Slot: 2, CardPresent: true, HasDataPlan: true, DataLimited: true},
		}, dataUsageUnder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := logged(t)
			if got := dataUsageFrom(ctx, modem(tt.sims...)); got != tt.want {
				t.Errorf("dataUsageFrom = %q, want %q", got, tt.want)
			}
		})
	}
}

// The cases where the key must be absent rather than under. under means
// "there is an allowance and it is not close", and none of these records has
// an allowance — publishing under for any of them would report headroom for a
// site that has none.
func TestDataUsageIsAbsentWhenThereIsNothingToBeUnder(t *testing.T) {
	noCard := planned()
	noCard.CardPresent = false
	noPlan := planned()
	noPlan.HasDataPlan = false

	tests := []struct {
		name string
		sims []simSlot
	}{
		{"no SIM entries at all", nil},
		{"no SIM is active", []simSlot{{Slot: 1, CardPresent: true, HasDataPlan: true}}},
		{"the active slot has no card", []simSlot{noCard}},
		{"the active SIM has no data plan", []simSlot{noPlan}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := logged(t)
			if got := dataUsageFrom(ctx, modem(tt.sims...)); got != "" {
				t.Errorf("dataUsageFrom = %q, want the key not to be published", got)
			}
		})
	}
}

// Two SIMs both claiming to be active is the console contradicting itself,
// and which slot carries the traffic is exactly what it has failed to say.
// Guessing could report the idle slot's headroom while the live one is over
// its cap, so the contradiction is a log line and no key at all.
func TestTwoActiveSIMsPublishNothingAndSaySo(t *testing.T) {
	ctx, logs := logged(t)
	second := planned()
	second.Slot = 2

	if got := dataUsageFrom(ctx, modem(planned(), second)); got != "" {
		t.Errorf("dataUsageFrom = %q, want nothing while the SIMs contradict each other", got)
	}
	if !strings.Contains(logs(), "More than one SIM") {
		t.Errorf("a contradiction between SIMs must be reported, not resolved silently; logged:\n%s", logs())
	}
}

// Through stateFromDevices: a modem record publishes the key alongside the
// rest of the observation, and a record with nothing to say costs the
// observation nothing.
func TestDataUsageThroughTheDeviceList(t *testing.T) {
	ctx, _ := logged(t)
	c := NewClient("", nil, "", false)

	warned := planned()
	warned.DataWarning = true
	state, err := c.stateFromDevices(ctx, deviceStatResponse{Data: []deviceRecord{modem(warned)}})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyDataUsage] != dataUsageWarning {
		t.Errorf("state[data.usage] = %q, want %q", state[stateKeyDataUsage], dataUsageWarning)
	}

	// A gateway with a modem but no plan on the SIM: the UPS record keeps the
	// observation non-empty, and data.usage must be absent rather than under.
	noPlan := planned()
	noPlan.HasDataPlan = false
	ups := deviceRecord{VBMS: &vbmsTable{}}
	state, err = c.stateFromDevices(ctx, deviceStatResponse{Data: []deviceRecord{modem(noPlan), ups}})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if got, present := state[stateKeyDataUsage]; present {
		t.Errorf("state[data.usage] = %q, want the key to be absent for a SIM with no plan", got)
	}
}
