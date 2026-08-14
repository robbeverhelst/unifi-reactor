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
	"slices"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// The two values of the console's per-device state field this provider is
// willing to read. UniFi documents others — provisioning, upgrading, heartbeat
// missed, adoption pending — and none of them has been captured, so anything
// else is treated as "this device says nothing" rather than being folded into
// one of these. Reading an unfamiliar state as offline is how a firmware that
// added a value would shed a cluster's load.
const (
	deviceStateOffline = 0
	deviceStateOnline  = 1
)

// deviceHealthFields are the fields the devices and device.<name> keys are
// derived from. They are embedded in deviceRecord, so they decode from the same
// flat object; keeping them here means the fields and the parser that reads
// them are in one file.
type deviceHealthFields struct {
	// State is 1 while the console is in contact with the device and 0 when it
	// is not.
	//
	// It is a pointer, and that is the load-bearing decision in this file. An
	// absent state is not state 0: a record truncated by a firmware change, or
	// a device type that does not report the field, would otherwise be read as
	// a dead device and take the whole fleet to degraded. Absent means absent.
	State *int `json:"state"`
	// Adopted gates every fleet-wide key: an unadopted device is one the
	// console can see but does not manage, and #8 is about the fleet you own.
	// A pointer for the same reason, and nil is read as "not known to be
	// adopted" — the direction that publishes fewer keys rather than more.
	Adopted *bool `json:"adopted"`
	// DisconnectionReason is never derived from. It is the diagnostic that
	// makes an offline device actionable — a V(1) line naming which device and
	// why the console lost it — and a diagnostic is a log line rather than API.
	DisconnectionReason string `json:"disconnection_reason"`
}

// adopted reports whether this record is a device the console manages.
func (d deviceRecord) adopted() bool {
	return d.Adopted != nil && *d.Adopted
}

// deviceTally accumulates one device list into the fleet keys.
//
// It is a tally rather than a first-match scan because both keys are about the
// whole fleet: wan comes from the one gateway, but devices is only meaningful
// once every record has been seen.
type deviceTally struct {
	// perDevice is the value each device's own key would publish, keyed by the
	// full state key.
	perDevice map[string]string
	// shared holds the keys more than one device slugified onto, which is a
	// name collision rather than an observation and is published as neither
	// device's state.
	shared map[string]bool
	// counted is how many adopted devices reported a state this provider
	// recognises, and offline how many of those were offline. A fleet where
	// counted is zero publishes nothing at all: there is nothing observable,
	// which is not the same as nothing being wrong.
	counted, offline int
	// offlineNames are the slugs behind that count, for the diagnostic line.
	offlineNames []string
}

func newDeviceTally() *deviceTally {
	return &deviceTally{perDevice: map[string]string{}, shared: map[string]bool{}}
}

// observe folds one adopted device into the tally.
func (t *deviceTally) observe(ctx context.Context, d deviceRecord) {
	log := logf.FromContext(ctx).WithName("unifi-devices")

	if d.State == nil {
		log.V(1).Info("An adopted device reports no state; it counts towards neither devices nor a key of its own",
			"model", d.Model, "type", d.Type)
		return
	}

	var value string
	switch *d.State {
	case deviceStateOnline:
		value = deviceOnline
	case deviceStateOffline:
		value = deviceOffline
	default:
		// An unrecognised state is not translated into a value, for the same
		// reason an unrecognised www status is not: provisioning and upgrading
		// are states a healthy device passes through, and reading either as
		// offline would report a firmware update as a fleet outage.
		log.Info("A device reports a state this provider does not recognise; it counts towards neither "+
			"devices nor a key of its own. Please report it — the set of states is what this key is derived from",
			"state", *d.State, "model", d.Model, "type", d.Type)
		return
	}

	slug := slugify(d.Name)
	t.counted++
	if value == deviceOffline {
		t.offline++
		t.offlineNames = append(t.offlineNames, offlineDescription(slug, d))
	}

	if slug == "" {
		// Nameless devices still count towards the fleet — a dead AP is dead
		// whether or not anyone named it — but there is no key to publish it
		// under.
		log.V(1).Info("A device has no name to derive a key from; it counts towards devices only",
			"model", d.Model, "type", d.Type, "state", value)
		return
	}
	key := stateKeyDevicePrefix + slug
	if _, already := t.perDevice[key]; already {
		t.shared[key] = true
	}
	t.perDevice[key] = value
}

// publish writes the fleet keys into the state map.
//
// perDeviceKeys is the opt-in from Client. With it off — the default — one key
// is published no matter how large the fleet is, which is the whole point: the
// per-device keys are the ones whose names, and therefore whose metric series
// count, are bounded by someone's rack rather than by this repository.
func (t *deviceTally) publish(ctx context.Context, state map[string]string, perDeviceKeys bool) {
	log := logf.FromContext(ctx).WithName("unifi-devices")
	if t.counted == 0 {
		log.V(1).Info("No adopted device reported a recognisable state; devices will not be published")
		return
	}

	state[stateKeyDevices] = devicesAllOnline
	if t.offline > 0 {
		state[stateKeyDevices] = devicesDegraded
	}
	slices.Sort(t.offlineNames)
	log.V(1).Info("device fleet", "devices", state[stateKeyDevices], "adopted", t.counted,
		"offline", t.offline, "offlineDevices", strings.Join(t.offlineNames, ","),
		"perDeviceKeys", perDeviceKeys)

	if !perDeviceKeys {
		return
	}
	for key, value := range t.perDevice {
		if t.shared[key] {
			// Two devices named the same thing after slugification. Publishing
			// either one's state under this key would be arbitrary, and the
			// arbitrary choice could be the one that hides a dead device, so
			// the key is published as neither. devices still counts both.
			metrics.SignalsDisagreed(ProviderName, signalDeviceNameShared)
			log.Info("Two or more devices share one key after slugifying their names, so that key "+
				"reports neither of them; rename one on the console to tell them apart",
				"key", key)
			continue
		}
		state[key] = value
	}
}

// offlineDescription is one offline device as it appears in the diagnostic
// line: what it is called, why the console lost it, and when it was last seen.
func offlineDescription(slug string, d deviceRecord) string {
	name := slug
	if name == "" {
		name = "(unnamed " + d.Model + ")"
	}
	if d.DisconnectionReason != "" {
		name += "=" + d.DisconnectionReason
	}
	return name
}
