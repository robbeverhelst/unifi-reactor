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

// upgradableDevice is an adopted device that answers the upgrade question.
func upgradableDevice(name string, upgradable bool) deviceRecord {
	d := adoptedDevice(name, deviceStateOnline)
	d.firmwareFields = firmwareFields{
		Version:    "1.2.3",
		Upgradable: &upgradable,
	}
	if upgradable {
		d.UpgradeToFirmware = "1.3.0"
	}
	return d
}

func TestFirmwareAcrossTheFleet(t *testing.T) {
	tests := []struct {
		name    string
		devices []deviceRecord
		want    string
	}{
		{"nothing to upgrade", []deviceRecord{
			upgradableDevice("Switch 48", false),
			upgradableDevice("AP Kitchen", false),
		}, firmwareCurrent},
		{"one device behind", []deviceRecord{
			upgradableDevice("Switch 48", false),
			upgradableDevice("AP Kitchen", true),
		}, firmwareUpdatesAvailable},
		{"everything behind", []deviceRecord{
			upgradableDevice("Switch 48", true),
		}, firmwareUpdatesAvailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fleetState(t, false, tc.devices...)[stateKeyFirmware]; got != tc.want {
				t.Errorf("state[firmware] = %q, want %q", got, tc.want)
			}
		})
	}
}

// Absent is not false, and here that matters in the direction of a missed
// update rather than a false alarm: the committed captures carry no upgradable
// field at all, so a console that reports none must publish no key rather than
// "everything is current".
func TestMissingUpgradableIsNotUpToDate(t *testing.T) {
	c := serve(t, merged(t, "stat-device-gateway.json", "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got, present := state[stateKeyFirmware]; present {
		t.Errorf("the captures report no upgradable field, so firmware must be absent, got %q", got)
	}
}

// A fleet where only some devices answer is the realistic case — the field is
// per device type — and the ones that do answer are enough to publish the key.
// The silent ones are named in the diagnostic line rather than assumed current.
func TestFirmwareIsDerivedFromTheDevicesThatAnswer(t *testing.T) {
	silent := adoptedDevice("Old Switch", deviceStateOnline)

	state := fleetState(t, false, silent, upgradableDevice("AP Kitchen", true))
	if got := state[stateKeyFirmware]; got != firmwareUpdatesAvailable {
		t.Errorf("state[firmware] = %q, want %q", got, firmwareUpdatesAvailable)
	}

	onlySilent := fleetState(t, false, silent)
	if got, present := onlySilent[stateKeyFirmware]; present {
		t.Errorf("a fleet where nothing answers should publish no firmware key, got %q", got)
	}
}

// Unadopted devices are excluded from every fleet key, and firmware is no
// exception: a neighbour's out-of-date AP is not an update you can apply.
func TestFirmwareIgnoresUnadoptedDevices(t *testing.T) {
	neighbour := upgradableDevice("Neighbour AP", true)
	neighbour.Adopted = nil

	state := fleetState(t, false, upgradableDevice("Switch 48", false), neighbour)

	if got := state[stateKeyFirmware]; got != firmwareCurrent {
		t.Errorf("state[firmware] = %q, want %q", got, firmwareCurrent)
	}
}

// The firmware fields have never been captured, so the shape they are parsed
// from is the shape UniFi documents. This asserts the decode of that documented
// shape end to end, and it is a HYPOTHESIS rather than ground truth: it lives
// here in code, deliberately, because a file under testdata/ claims to have
// come off a console. See the live-verification list in testdata/unifi/README.md.
func TestFirmwareDecodesTheDocumentedShape(t *testing.T) {
	const documented = `{"data":[{
		"model": "USW48", "type": "usw", "name": "Switch 48",
		"state": 1, "adopted": true,
		"version": "6.6.65.14856", "displayable_version": "6.6.65",
		"upgradable": true, "upgrade_to_firmware": "7.0.50.15613",
		"required_version": "6.0.0", "safe_for_autoupgrade": true,
		"model_in_eol": false, "model_in_lts": false
	}]}`

	var parsed deviceStatResponse
	if err := json.Unmarshal([]byte(documented), &parsed); err != nil {
		t.Fatalf("parsing the documented shape: %v", err)
	}
	d := parsed.Data[0]
	if d.Upgradable == nil || !*d.Upgradable {
		t.Fatalf("upgradable did not decode: %+v", d.Upgradable)
	}
	if d.UpgradeToFirmware != "7.0.50.15613" || d.Version != "6.6.65.14856" {
		t.Errorf("version fields did not decode: %q -> %q", d.Version, d.UpgradeToFirmware)
	}
	if d.ModelInEOL == nil || *d.ModelInEOL {
		t.Errorf("model_in_eol did not decode: %+v", d.ModelInEOL)
	}

	c := NewClient("", nil, "", false)
	state, err := c.stateFromDevices(context.Background(), parsed)
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyFirmware] != firmwareUpdatesAvailable {
		t.Errorf("state[firmware] = %q, want %q", state[stateKeyFirmware], firmwareUpdatesAvailable)
	}
}

// model_in_eol is read and deliberately not published as a key. It is an
// inventory fact rather than a state — it does not transition, so an Automation
// matching it would hold a permanent condition — and this asserts the decision
// rather than the derivation, so nobody quietly turns it into a sixth key
// without arguing for it.
func TestEndOfLifeIsADiagnosticRatherThanAKey(t *testing.T) {
	eol, no := true, false
	device := upgradableDevice("Old Switch", false)
	device.ModelInEOL, device.ModelInLTS = &eol, &no

	state := fleetState(t, false, device)

	if state[stateKeyFirmware] != firmwareCurrent {
		t.Errorf("state[firmware] = %q, want %q: end of life is not an available update",
			state[stateKeyFirmware], firmwareCurrent)
	}
	for key := range state {
		if key == "firmware.eol" || key == "eol" {
			t.Errorf("%q is not a published key; end of life is a diagnostic", key)
		}
	}
}
