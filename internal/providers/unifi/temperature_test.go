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

// sensorCPU is one sensor name in the documented per-sensor table. The names
// are diagnostics — nothing is derived from them — so any one will do.
const sensorCPU = "CPU"

// warmDevice is an adopted device with a thermal sensor reporting one reading.
func warmDevice(name string, celsius float64) deviceRecord {
	d := adoptedDevice(name, deviceStateOnline)
	instrumented, fan := true, true
	d.temperatureFields = temperatureFields{
		HasTemperature: &instrumented,
		HasFan:         &fan,
		Overheating:    new(bool),
		Temperatures:   []deviceTemperature{{Name: sensorCPU, Type: "cpu", Value: &celsius}},
	}
	return d
}

// heatState derives the temperature key with a given threshold, so the
// bucketing and the configurability are tested through the same path.
func heatState(t *testing.T, threshold float64, devices ...deviceRecord) map[string]string {
	t.Helper()
	c := NewClient("", nil, "", false)
	c.HighTemperatureCelsius = threshold
	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: devices})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	return state
}

func TestTemperatureBuckets(t *testing.T) {
	tests := []struct {
		name    string
		devices []deviceRecord
		want    string
	}{
		{"a cool rack", []deviceRecord{
			warmDevice("Switch 48", 42),
			warmDevice("AP Kitchen", 51),
		}, temperatureNormal},
		{"one device at the threshold", []deviceRecord{
			warmDevice("Switch 48", 42),
			warmDevice("AP Attic", 75),
		}, temperatureHigh},
		{"one device over it", []deviceRecord{
			warmDevice("Switch 48", 81.5),
		}, temperatureHigh},
		{"just under it", []deviceRecord{
			warmDevice("Switch 48", 74.9),
		}, temperatureNormal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := heatState(t, DefaultHighTemperatureCelsius, tc.devices...)[stateKeyTemperature]; got != tc.want {
				t.Errorf("state[temperature] = %q, want %q", got, tc.want)
			}
		})
	}
}

// The threshold is what an operator tunes against their own rack, so the same
// reading has to be able to fall on either side of it.
func TestCustomTemperatureThreshold(t *testing.T) {
	device := warmDevice("Switch 48", 60)

	if got := heatState(t, 55, device)[stateKeyTemperature]; got != temperatureHigh {
		t.Errorf("state[temperature] = %q at 60 °C with a 55 °C threshold, want %q", got, temperatureHigh)
	}
	if got := heatState(t, 90, device)[stateKeyTemperature]; got != temperatureNormal {
		t.Errorf("state[temperature] = %q at 60 °C with a 90 °C threshold, want %q", got, temperatureNormal)
	}
}

// The hottest sensor on the hottest device decides. A board is as hot as its
// hottest part, and averaging would let one cool sensor hide a cooking one.
func TestTheHottestSensorDecides(t *testing.T) {
	cool, hot := 45.0, 88.0
	device := warmDevice("Switch 48", cool)
	device.Temperatures = []deviceTemperature{
		{Name: sensorCPU, Type: "cpu", Value: &cool},
		{Name: "PHY", Type: "phy", Value: &hot},
		{Name: "Board", Type: "board", Value: nil},
	}

	if got := heatState(t, DefaultHighTemperatureCelsius, device)[stateKeyTemperature]; got != temperatureHigh {
		t.Errorf("state[temperature] = %q, want %q: the hottest sensor decides", got, temperatureHigh)
	}
}

// The console's own verdict outranks the threshold in this repository: the
// firmware knows what this model tolerates and a default does not.
func TestOverheatingIsBelievedRegardlessOfTheReading(t *testing.T) {
	overheating := true
	device := warmDevice("AP Attic", 48)
	device.Overheating = &overheating

	if got := heatState(t, DefaultHighTemperatureCelsius, device)[stateKeyTemperature]; got != temperatureHigh {
		t.Errorf("state[temperature] = %q, want %q: the console said it is overheating", got, temperatureHigh)
	}
}

// A device that says it is overheating and reports no number at all is the case
// that must not be missed, so the flag alone is enough to publish the key.
func TestOverheatingWithoutAReadingStillPublishes(t *testing.T) {
	overheating := true
	device := adoptedDevice("AP Attic", deviceStateOnline)
	device.Overheating = &overheating

	if got := heatState(t, DefaultHighTemperatureCelsius, device)[stateKeyTemperature]; got != temperatureHigh {
		t.Errorf("state[temperature] = %q, want %q", got, temperatureHigh)
	}
}

// Absent is not zero, in the direction that matters most for this key: a device
// reporting no temperature is not a device at 0 °C, and reading it as one would
// make the fleet look coldest exactly when a sensor stops answering. The UPS
// capture is a real example — issue #11 notes it has no thermal fields at all.
func TestMissingTemperatureIsNotZeroDegrees(t *testing.T) {
	c := serve(t, merged(t, "stat-device-gateway.json", "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got, present := state[stateKeyTemperature]; present {
		t.Errorf("neither capture reports thermals, so temperature must be absent, got %q", got)
	}

	// And the same at the level below: a sensor with a null value contributes
	// nothing rather than dragging the maximum down to zero.
	hot := 82.0
	device := warmDevice("Switch 48", hot)
	device.Temperatures = []deviceTemperature{{Name: sensorCPU, Value: nil}, {Name: "PHY", Value: &hot}}
	if got := heatState(t, DefaultHighTemperatureCelsius, device)[stateKeyTemperature]; got != temperatureHigh {
		t.Errorf("state[temperature] = %q, want %q: a null sensor is not 0 °C", got, temperatureHigh)
	}
}

// A device that claims thermal reporting and then publishes nothing is
// instrumented and silent. It keeps the key alive — something is reporting — and
// contributes no reading, rather than being read as cold or dropping the key.
func TestAnInstrumentedDeviceWithNoReadingKeepsTheKey(t *testing.T) {
	instrumented := true
	silent := adoptedDevice("Switch 48", deviceStateOnline)
	silent.HasTemperature = &instrumented

	state := heatState(t, DefaultHighTemperatureCelsius, silent)
	if got := state[stateKeyTemperature]; got != temperatureNormal {
		t.Errorf("state[temperature] = %q, want %q", got, temperatureNormal)
	}
}

// The single-value form older devices report instead of a sensor table.
func TestGeneralTemperatureIsReadWhenThereIsNoTable(t *testing.T) {
	celsius := 79.0
	device := adoptedDevice("Old Switch", deviceStateOnline)
	instrumented := true
	device.HasTemperature, device.GeneralTemperature = &instrumented, &celsius

	if got := heatState(t, DefaultHighTemperatureCelsius, device)[stateKeyTemperature]; got != temperatureHigh {
		t.Errorf("state[temperature] = %q, want %q", got, temperatureHigh)
	}
}

// Unadopted devices are outside every fleet key, temperature included.
func TestTemperatureIgnoresUnadoptedDevices(t *testing.T) {
	neighbour := warmDevice("Neighbour AP", 95)
	neighbour.Adopted = nil

	state := heatState(t, DefaultHighTemperatureCelsius, warmDevice("Switch 48", 40), neighbour)
	if got := state[stateKeyTemperature]; got != temperatureNormal {
		t.Errorf("state[temperature] = %q, want %q", got, temperatureNormal)
	}
}

// The thermal fields have never been captured, so this asserts the decode of
// the shape UniFi documents. It is a HYPOTHESIS and it lives in code for the
// same reason the failover variants do: a file under testdata/ claims to have
// come off a console, and this has not. Both documented forms are covered —
// the per-sensor table and the single general_temperature field.
func TestTemperatureDecodesTheDocumentedShape(t *testing.T) {
	const documented = `{"data":[
		{
			"model": "USW48", "type": "usw", "name": "Switch 48", "state": 1, "adopted": true,
			"has_temperature": true, "has_fan": true, "overheating": false,
			"temperatures": [
				{"name": "CPU", "type": "cpu", "value": 62.5},
				{"name": "PHY", "type": "phy", "value": 78.25},
				{"name": "System", "type": "board", "value": null}
			]
		},
		{
			"model": "UAPFLEXHD", "type": "uap", "name": "AP Kitchen", "state": 1, "adopted": true,
			"has_temperature": true, "has_fan": false, "overheating": false,
			"general_temperature": 55
		}
	]}`

	var parsed deviceStatResponse
	if err := json.Unmarshal([]byte(documented), &parsed); err != nil {
		t.Fatalf("parsing the documented shape: %v", err)
	}
	if got, known := parsed.Data[0].hottest(); !known || got != 78.25 {
		t.Errorf("the sensor table decoded to (%v, %v), want (78.25, true)", got, known)
	}
	if got, known := parsed.Data[1].hottest(); !known || got != 55 {
		t.Errorf("general_temperature decoded to (%v, %v), want (55, true)", got, known)
	}

	c := NewClient("", nil, "", false)
	state, err := c.stateFromDevices(context.Background(), parsed)
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyTemperature] != temperatureHigh {
		t.Errorf("state[temperature] = %q, want %q at 78.25 °C against a 75 °C default",
			state[stateKeyTemperature], temperatureHigh)
	}
}
