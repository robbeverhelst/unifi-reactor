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
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultHighTemperatureCelsius is the reading at or above which the hottest
// device in the fleet reports temperature: high.
//
// It is set against the debounce this key ships with, not in isolation, and the
// #7 rule applies: move one and you have moved the other. 75 °C is well above
// where UniFi switches and APs normally sit (40–60 °C in a ventilated rack) and
// below where the console's own overheating flag is understood to trip, so 3
// samples — 90 seconds at the default poll — is a reading that has genuinely
// held rather than a fan spinning up late. Lower this towards the normal
// operating range and 90 seconds of hysteresis stops being enough to mean
// anything, because the reading will cross the line and stay there.
const DefaultHighTemperatureCelsius = 75.0

// temperatureFields are the fields the temperature key is derived from,
// embedded in deviceRecord so they decode from the same flat object.
//
// NONE of these has been captured. The UPS 2U record has no thermal fields at
// all — issue #11 notes it reports has_fan: false and no temperatures — and the
// gateway record was allowlisted before any of this was parsed. So every field
// here is written to the shape UniFi's API documents and to the names #11 lists,
// and added to hack/capture-unifi.sh so a capture from a switch or an AP settles
// them. See the live-verification list in testdata/unifi/README.md.
type temperatureFields struct {
	// HasTemperature is the console's statement that this device does thermal
	// reporting at all, which is what makes a device with no readings distinct
	// from a device that is not instrumented.
	HasTemperature *bool `json:"has_temperature"`
	// Overheating is the console's own verdict, and it is trusted directly:
	// whatever threshold the firmware applies knows more about this model's
	// tolerances than a number in this repository does.
	Overheating *bool `json:"overheating"`
	// HasFan is never derived from. It is a diagnostic: a hot fanless device and
	// a hot device with a failed fan are different problems.
	HasFan *bool `json:"has_fan"`
	// Temperatures is the per-sensor table newer devices carry. Every value is a
	// pointer for the reason this whole provider decodes numbers as pointers: a
	// missing reading is not 0 °C, and reading it as one would report a cooking
	// switch as the coldest thing in the rack — the direction that hides the
	// failure this key exists to catch.
	Temperatures []deviceTemperature `json:"temperatures"`
	// GeneralTemperature is the single-value form older devices report instead.
	GeneralTemperature *float64 `json:"general_temperature"`
}

// deviceTemperature is one sensor's reading. name and type are diagnostics —
// "CPU" and "PHY" are different places on the same board — and neither is
// derived from, because a sensor name could not be a state value.
type deviceTemperature struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Value *float64 `json:"value"`
}

// hottest is the highest reading this device reports, and whether it reported
// any. The per-sensor table wins over the single-value field where both are
// present; the hottest sensor is the one that matters, since a board is as hot
// as its hottest part.
func (d deviceRecord) hottest() (float64, bool) {
	hottest, known := 0.0, false
	for _, sensor := range d.Temperatures {
		if sensor.Value == nil {
			continue
		}
		if !known || *sensor.Value > hottest {
			hottest, known = *sensor.Value, true
		}
	}
	if known {
		return hottest, true
	}
	if d.GeneralTemperature != nil {
		return *d.GeneralTemperature, true
	}
	return 0, false
}

// instrumented reports whether this device says anything about its own
// thermals: it claims temperature reporting, it produced a reading, or it is
// telling us it is overheating. A device that says none of those things is not a
// device at 0 °C, and it contributes nothing.
func (d deviceRecord) instrumented() bool {
	if d.HasTemperature != nil && *d.HasTemperature {
		return true
	}
	if _, known := d.hottest(); known {
		return true
	}
	return d.Overheating != nil && *d.Overheating
}

// temperatureTally accumulates the fleet's thermals into one bucketed key.
//
// One key rather than one per device, for the same reason firmware is one key:
// "is anything cooking" is the question, and a per-device thermal key would
// multiply series by fleet size. The hottest device decides, and the numbers
// behind it are a V(1) log line — a temperature cannot be a state value at all,
// since spec.when compares strings and one series per reading is unbounded.
type temperatureTally struct {
	// high is the reading at or above which the fleet reports high, taken from
	// Client at construction so that publishing needs nothing but the tally.
	high float64
	// instrumented is how many adopted devices said anything about their
	// thermals. Zero publishes no key.
	instrumented int
	// hottest is the highest reading anywhere in the fleet, and hottestDevice
	// the slug behind it.
	hottest       float64
	hottestKnown  bool
	hottestDevice string
	// overheating names the devices the console itself calls overheating, which
	// is escalated regardless of what any reading says.
	overheating []string
	// readings is every device's hottest reading, for the diagnostic line.
	readings []string
	// fanless counts instrumented devices with no fan, which is why a warm one
	// may be normal.
	fanless int
}

func newTemperatureTally(high float64) temperatureTally {
	if high <= 0 {
		high = DefaultHighTemperatureCelsius
	}
	return temperatureTally{high: high}
}

// observe folds one adopted device into the tally.
func (t *temperatureTally) observe(ctx context.Context, d deviceRecord) {
	if !d.instrumented() {
		return
	}
	t.instrumented++
	name := slugify(d.Name)
	if name == "" {
		name = strings.ToLower(d.Model)
	}
	if d.HasFan != nil && !*d.HasFan {
		t.fanless++
	}
	if d.Overheating != nil && *d.Overheating {
		t.overheating = append(t.overheating, name)
	}

	reading, known := d.hottest()
	if !known {
		// Instrumented and silent: it claims thermal reporting and produced no
		// number. That is a real thing to be — and it is not 0 °C.
		logf.FromContext(ctx).WithName("unifi-temperature").V(1).Info(
			"A device claims temperature reporting but published no reading", "device", name, "model", d.Model)
		return
	}
	t.readings = append(t.readings, name+"="+strconv.FormatFloat(reading, 'f', 1, 64))
	if !t.hottestKnown || reading > t.hottest {
		t.hottest, t.hottestKnown, t.hottestDevice = reading, true, name
	}
}

// publish buckets the fleet's hottest reading against the threshold.
func (t temperatureTally) publish(ctx context.Context, state map[string]string) {
	log := logf.FromContext(ctx).WithName("unifi-temperature")
	if t.instrumented == 0 {
		log.V(1).Info("No adopted device reports its thermals; temperature will not be published")
		return
	}

	value := temperatureNormal
	switch {
	case len(t.overheating) > 0:
		// The console's own verdict wins over the threshold in this repository:
		// the firmware knows this model's tolerances and a default does not.
		value = temperatureHigh
	case t.hottestKnown && t.hottest >= t.high:
		value = temperatureHigh
	}
	state[stateKeyTemperature] = value

	// The raw numbers are diagnostics, not API. This is the line that tells an
	// operator where to put their own threshold.
	log.V(1).Info("temperature", "temperature", value, "hottestCelsius", t.hottest,
		"hottestDevice", t.hottestDevice, "readingKnown", t.hottestKnown,
		"thresholdCelsius", t.high, "devicesInstrumented", t.instrumented,
		"devicesOverheating", strings.Join(t.overheating, ","), "devicesFanless", t.fanless,
		"readings", strings.Join(t.readings, ","))
}
