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
)

// poweredSwitch is an adopted switch with a PoE budget and one port per wattage
// given, each powering something.
func poweredSwitch(name string, budget float64, watts ...float64) deviceRecord {
	d := adoptedDevice(name, deviceStateOnline)
	d.TotalMaxPower = &budget
	enabled := true
	for i, w := range watts {
		index := i + 1
		d.PortTable = append(d.PortTable, devicePort{
			PortIdx:   &index,
			PoEEnable: &enabled,
			PoEPower:  flexibleNumber{Value: w, Known: true},
			PoEClass:  "4",
		})
	}
	return d
}

// switchWithPortsOff is an adopted switch whose ports are all PoE-disabled:
// nothing plugged in, which is a draw of zero rather than a missing reading.
func switchWithPortsOff(name string, budget float64, ports int) deviceRecord {
	d := adoptedDevice(name, deviceStateOnline)
	d.TotalMaxPower = &budget
	off := false
	for i := range ports {
		index := i + 1
		d.PortTable = append(d.PortTable, devicePort{PortIdx: &index, PoEEnable: &off})
	}
	return d
}

// poeState derives the poe key with a given threshold.
func poeState(t *testing.T, threshold float64, devices ...deviceRecord) map[string]string {
	t.Helper()
	c := NewClient("", nil, "", false)
	c.MaxPoEUtilizationPercent = threshold
	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: devices})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	return state
}

func TestPoEBuckets(t *testing.T) {
	tests := []struct {
		name    string
		devices []deviceRecord
		want    string
	}{
		{"plenty of headroom", []deviceRecord{
			poweredSwitch("Switch 48", 60, 6.5, 7.2, 4.1),
		}, poeOK},
		{"exactly at the threshold", []deviceRecord{
			poweredSwitch("Switch 48", 60, 54),
		}, poeInsufficient},
		{"over the threshold", []deviceRecord{
			poweredSwitch("Switch 8", 60, 30, 28),
		}, poeInsufficient},
		// A switch with a budget and nothing plugged into it draws nothing, and
		// that is a measurement rather than an absence.
		{"nothing powered at all", []deviceRecord{
			switchWithPortsOff("Switch 48", 60, 4),
		}, poeOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := poeState(t, DefaultMaxPoEUtilizationPercent, tc.devices...)[stateKeyPoE]; got != tc.want {
				t.Errorf("state[poe] = %q, want %q", got, tc.want)
			}
		})
	}
}

// The worst switch decides. One switch out of headroom drops the cameras on
// that switch, whatever the rest of the rack has spare.
func TestTheWorstSwitchDecides(t *testing.T) {
	state := poeState(t, DefaultMaxPoEUtilizationPercent,
		poweredSwitch("Switch 48", 400, 20, 15),
		poweredSwitch("Switch 8", 60, 57))

	if got := state[stateKeyPoE]; got != poeInsufficient {
		t.Errorf("state[poe] = %q, want %q", got, poeInsufficient)
	}
}

func TestCustomPoEThreshold(t *testing.T) {
	device := poweredSwitch("Switch 48", 100, 70)

	if got := poeState(t, 60, device)[stateKeyPoE]; got != poeInsufficient {
		t.Errorf("state[poe] = %q at 70%% with a 60%% threshold, want %q", got, poeInsufficient)
	}
	if got := poeState(t, 95, device)[stateKeyPoE]; got != poeOK {
		t.Errorf("state[poe] = %q at 70%% with a 95%% threshold, want %q", got, poeOK)
	}
}

// Absent is not zero, in the direction that hides the failure: a port that is
// powering something and will not say how much makes the whole switch
// unreadable, because counting it as 0W would report headroom that is not there
// — the exact situation this key exists to catch.
func TestAPoweredPortWithNoWattageMakesTheSwitchUnreadable(t *testing.T) {
	device := poweredSwitch("Switch 8", 60, 55)
	enabled := true
	index := 2
	device.PortTable = append(device.PortTable, devicePort{
		PortIdx: &index, PoEEnable: &enabled, PoEClass: "4",
	})

	state := poeState(t, DefaultMaxPoEUtilizationPercent, device)
	if got, present := state[stateKeyPoE]; present {
		t.Errorf("an unreadable switch should publish no poe key, got %q", got)
	}

	// And it must not drag a readable switch down with it: the other switch is
	// still measured, so the key is published from what can be read.
	both := poeState(t, DefaultMaxPoEUtilizationPercent, device, poweredSwitch("Switch 48", 400, 20))
	if got := both[stateKeyPoE]; got != poeOK {
		t.Errorf("state[poe] = %q, want %q from the switch that can be read", got, poeOK)
	}
}

// A port that is off draws nothing, and that is a reading rather than an
// absence — so a switch with disabled ports is measurable.
func TestDisabledPortsAreNotUnreadable(t *testing.T) {
	device := poweredSwitch("Switch 8", 60, 12)
	off, index := false, 2
	device.PortTable = append(device.PortTable, devicePort{PortIdx: &index, PoEEnable: &off})

	if got := poeState(t, DefaultMaxPoEUtilizationPercent, device)[stateKeyPoE]; got != poeOK {
		t.Errorf("state[poe] = %q, want %q", got, poeOK)
	}
}

// A switch reporting no budget is not a switch with no budget, and it is
// certainly not a division by zero. It contributes nothing.
func TestMissingBudgetIsNotAZeroBudget(t *testing.T) {
	device := poweredSwitch("Switch 8", 60, 12)
	device.TotalMaxPower = nil

	if got, present := poeState(t, DefaultMaxPoEUtilizationPercent, device)[stateKeyPoE]; present {
		t.Errorf("a switch reporting no budget should publish no poe key, got %q", got)
	}

	zero := 0.0
	device.TotalMaxPower = &zero
	if got, present := poeState(t, DefaultMaxPoEUtilizationPercent, device)[stateKeyPoE]; present {
		t.Errorf("a zero budget is not a denominator, got %q", got)
	}
}

// Neither committed capture carries a port_table at all — the UPS 2U is a
// switch-type device with no ports — so the key must be absent rather than ok.
func TestNoSwitchPublishesNoPoEKey(t *testing.T) {
	c := serve(t, merged(t, "stat-device-gateway.json", "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got, present := state[stateKeyPoE]; present {
		t.Errorf("no capture reports a PoE budget, so poe must be absent, got %q", got)
	}
}

// UniFi is documented to report poe_power as a STRING on several firmwares and
// as a number on others. Both have to decode, because getting it wrong makes an
// entire switch's draw unreadable on half the world's consoles — and neither
// null nor an unparseable value may become 0W.
func TestPoEPowerDecodesAsBothAStringAndANumber(t *testing.T) {
	tests := []struct {
		json  string
		value float64
		known bool
	}{
		{`"3.90"`, 3.9, true},
		{`3.9`, 3.9, true},
		{`0`, 0, true},
		{`"0.00"`, 0, true},
		{jsonNull, 0, false},
		{`""`, 0, false},
		{`"n/a"`, 0, false},
		{`{}`, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.json, func(t *testing.T) {
			var got flexibleNumber
			if err := json.Unmarshal([]byte(tc.json), &got); err != nil {
				t.Fatalf("decoding %s: %v", tc.json, err)
			}
			if got.Known != tc.known || got.Value != tc.value {
				t.Errorf("%s decoded to %+v, want {%v %v}", tc.json, got, tc.value, tc.known)
			}
		})
	}
}

// The PoE fields have never been captured, so this asserts the decode of the
// shape UniFi documents, with poe_power in its string form. It is a HYPOTHESIS
// in code rather than a fixture, for the reason the failover variants are.
func TestPoEDecodesTheDocumentedShape(t *testing.T) {
	const documented = `{"data":[{
		"model": "USW48P", "type": "usw", "name": "Switch 48", "state": 1, "adopted": true,
		"total_max_power": 195,
		"port_table": [
			{"port_idx": 1, "poe_enable": true, "poe_power": "6.52", "poe_class": "Class 3", "poe_mode": "auto"},
			{"port_idx": 2, "poe_enable": true, "poe_power": "13.20", "poe_class": "Class 4", "poe_mode": "auto"},
			{"port_idx": 3, "poe_enable": false, "poe_power": "0.00", "poe_class": "Unknown", "poe_mode": "off"},
			{"port_idx": 4}
		]
	}]}`

	var parsed deviceStatResponse
	if err := json.Unmarshal([]byte(documented), &parsed); err != nil {
		t.Fatalf("parsing the documented shape: %v", err)
	}
	draw := parsed.Data[0].poe()
	if !draw.measurable || draw.budget != 195 {
		t.Fatalf("the documented shape did not decode: %+v", draw)
	}
	if draw.watts != 19.72 {
		t.Errorf("watts = %v, want 19.72: only the powered ports count", draw.watts)
	}
	if draw.enabled != 2 {
		t.Errorf("enabled = %d, want 2: a port with no poe_enable is not powering anything", draw.enabled)
	}

	c := NewClient("", nil, "", false)
	state, err := c.stateFromDevices(context.Background(), parsed)
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyPoE] != poeOK {
		t.Errorf("state[poe] = %q, want %q at 19.72W of 195W", state[stateKeyPoE], poeOK)
	}
}
