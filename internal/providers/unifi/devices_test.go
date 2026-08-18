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
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// adoptedDevice is a device record in the shape the captures have: adopted,
// named, and reporting a state. Everything below starts from this so that what
// a test changes is the one field it is about.
func adoptedDevice(name string, state int) deviceRecord {
	adopted := true
	return deviceRecord{
		Model: "USW48",
		Type:  "usw",
		Name:  name,
		deviceHealthFields: deviceHealthFields{
			State:   &state,
			Adopted: &adopted,
		},
	}
}

// fleetState derives the fleet keys from a device list, with the per-device
// keys opted into or not.
func fleetState(t *testing.T, perDeviceKeys bool, devices ...deviceRecord) map[string]string {
	t.Helper()
	c := NewClient("", nil, "", false)
	c.PerDeviceKeys = perDeviceKeys
	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: devices})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	return state
}

// The captured gateway and UPS are both adopted and both report state 1, so the
// committed captures alone say the fleet is healthy.
func TestFleetIsAllOnlineInTheCapture(t *testing.T) {
	c := serve(t, merged(t, "stat-device-gateway.json", "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state[stateKeyDevices] != devicesAllOnline {
		t.Errorf("state[devices] = %q, want %q", state[stateKeyDevices], devicesAllOnline)
	}
}

// The aggregate is the safe default and the one that ships on, so it is
// published with no per-device key beside it however large the fleet is.
func TestPerDeviceKeysAreOptIn(t *testing.T) {
	devices := []deviceRecord{
		adoptedDevice("Switch 48", deviceStateOnline),
		adoptedDevice("AP Kitchen", deviceStateOffline),
	}

	off := fleetState(t, false, devices...)
	if off[stateKeyDevices] != devicesDegraded {
		t.Errorf("state[devices] = %q, want %q", off[stateKeyDevices], devicesDegraded)
	}
	for key := range off {
		if strings.HasPrefix(key, stateKeyDevicePrefix) {
			t.Errorf("per-device key %q must not be published unless it is opted into", key)
		}
	}

	on := fleetState(t, true, devices...)
	for key, want := range map[string]string{
		stateKeyDevices:                     devicesDegraded,
		stateKeyDevicePrefix + "switch-48":  deviceOnline,
		stateKeyDevicePrefix + "ap-kitchen": deviceOffline,
	} {
		if on[key] != want {
			t.Errorf("state[%q] = %q, want %q", key, on[key], want)
		}
	}
}

func TestFleetAggregate(t *testing.T) {
	tests := []struct {
		name    string
		devices []deviceRecord
		want    string
	}{
		{"every device online", []deviceRecord{
			adoptedDevice("A", deviceStateOnline),
			adoptedDevice("B", deviceStateOnline),
		}, devicesAllOnline},
		{"one device offline", []deviceRecord{
			adoptedDevice("A", deviceStateOnline),
			adoptedDevice("B", deviceStateOffline),
		}, devicesDegraded},
		{"every device offline", []deviceRecord{
			adoptedDevice("A", deviceStateOffline),
		}, devicesDegraded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fleetState(t, false, tc.devices...)[stateKeyDevices]; got != tc.want {
				t.Errorf("state[devices] = %q, want %q", got, tc.want)
			}
		})
	}
}

// The bug #5 nearly shipped, in this file's terms. The console omits fields
// rather than zeroing them, and state 0 is offline: a record that carries no
// state at all must not be read as a dead device, because one truncated record
// would take the whole fleet to degraded and shed a cluster's load.
func TestMissingStateIsNotAnOfflineDevice(t *testing.T) {
	silent := adoptedDevice("Mystery", deviceStateOnline)
	silent.State = nil

	state := fleetState(t, true, adoptedDevice("Switch 48", deviceStateOnline), silent)

	if got := state[stateKeyDevices]; got != devicesAllOnline {
		t.Errorf("state[devices] = %q, want %q: a device reporting no state is not a device at zero", got, devicesAllOnline)
	}
	if got, present := state[stateKeyDevicePrefix+"mystery"]; present {
		t.Errorf("a device reporting no state should publish no key of its own, got %q", got)
	}
}

// Neither the known transient states — upgrading, provisioning, heartbeat
// missed — nor a value this provider has never heard of may be folded into
// offline: a fleet mid-firmware-upgrade is not a fleet outage. State 5 is the
// one real hardware has shown (#97), so it is pinned here by constructing the
// record directly; no capture carries it.
func TestUnrecognisedDeviceStateIsNeitherOnlineNorOffline(t *testing.T) {
	for _, state := range []int{2, deviceStateUpgrading, deviceStateProvisioning, deviceStateHeartbeatMissing, 11, -1} {
		odd := adoptedDevice("Provisioning", state)
		got := fleetState(t, true, adoptedDevice("Switch 48", deviceStateOnline), odd)

		if got[stateKeyDevices] != devicesAllOnline {
			t.Errorf("state %d: state[devices] = %q, want %q", state, got[stateKeyDevices], devicesAllOnline)
		}
		if value, present := got[stateKeyDevicePrefix+"provisioning"]; present {
			t.Errorf("state %d should publish no key of its own, got %q", state, value)
		}
	}
}

// The INFO level is reserved for states this provider has genuinely never
// heard of, because those are worth an operator's attention. The known
// transient states are something a healthy site produces every config push,
// so they must not add an INFO line per device per poll (#97) — they speak at
// V(1) only, which the verbosity-0 logger here drops.
func TestKnownTransientStatesDoNotLogAtInfo(t *testing.T) {
	var sink strings.Builder
	ctx := logf.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		sink.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{}))

	tally := newDeviceTally(false)
	for _, state := range []int{deviceStateUpgrading, deviceStateProvisioning, deviceStateHeartbeatMissing} {
		tally.observe(ctx, adoptedDevice("Core Switch", state))
	}
	if got := sink.String(); got != "" {
		t.Errorf("a known transient state should say nothing at INFO, got %q", got)
	}

	tally.observe(ctx, adoptedDevice("Core Switch", 11))
	if got := sink.String(); !strings.Contains(got, "does not recognise") {
		t.Errorf("a genuinely unknown state should keep the please-report INFO line, got %q", got)
	}
}

// #8 asks for unadopted and pending devices to be excluded: they are devices
// the console can see and does not manage, so an offline one is not an outage
// of anything anyone owns.
func TestUnadoptedDevicesAreNotPartOfTheFleet(t *testing.T) {
	pending := adoptedDevice("Neighbour AP", deviceStateOffline)
	pending.Adopted = nil
	notAdopted := adoptedDevice("Other AP", deviceStateOffline)
	notAdopted.Adopted = new(bool)

	state := fleetState(t, true, adoptedDevice("Switch 48", deviceStateOnline), pending, notAdopted)

	if got := state[stateKeyDevices]; got != devicesAllOnline {
		t.Errorf("state[devices] = %q, want %q: unadopted devices are not part of the fleet", got, devicesAllOnline)
	}
	for _, slug := range []string{"neighbour-ap", "other-ap"} {
		if value, present := state[stateKeyDevicePrefix+slug]; present {
			t.Errorf("unadopted device %q should publish no key, got %q", slug, value)
		}
	}
}

// Omit what you cannot see, at the fleet level: a device list where nothing is
// adopted publishes no devices key rather than all-online. "Nothing to report"
// and "nothing is wrong" are different claims, and only one of them is true.
func TestAListWithNothingAdoptedObservesNothing(t *testing.T) {
	pending := adoptedDevice("Neighbour AP", deviceStateOnline)
	pending.Adopted = nil

	c := NewClient("", nil, "", false)
	if _, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{pending}}); err == nil {
		t.Fatal("a list holding nothing observable should be an error, not an empty observation")
	}
}

// Renaming a device on the console makes its old key vanish and a new one
// appear. The vanishing half is what must not look like a transition to
// "recovered": the engine reports a disappeared key as unavailable and the
// reconciler holds the last known state, which is only correct if the provider
// genuinely stops publishing the old name rather than publishing it as offline.
func TestRenamingADeviceMovesItsKeyRatherThanFailingIt(t *testing.T) {
	before := fleetState(t, true, adoptedDevice("Switch 48", deviceStateOnline))
	after := fleetState(t, true, adoptedDevice("Core Switch", deviceStateOnline))

	if before[stateKeyDevicePrefix+"switch-48"] != deviceOnline {
		t.Fatalf("state[device.switch-48] = %q, want %q", before[stateKeyDevicePrefix+"switch-48"], deviceOnline)
	}
	if value, present := after[stateKeyDevicePrefix+"switch-48"]; present {
		t.Errorf("the old key should vanish on a rename, not report %q", value)
	}
	if after[stateKeyDevicePrefix+"core-switch"] != deviceOnline {
		t.Errorf("state[device.core-switch] = %q, want %q", after[stateKeyDevicePrefix+"core-switch"], deviceOnline)
	}
	// Removing a device is the same shape as renaming one, and the aggregate
	// must not move for either: what is left is still all online.
	if after[stateKeyDevices] != devicesAllOnline {
		t.Errorf("state[devices] = %q, want %q", after[stateKeyDevices], devicesAllOnline)
	}
}

// Two devices whose names slugify to the same key. Publishing either one's
// state would be arbitrary, and the arbitrary choice could be the one that
// hides the dead device, so the key reports neither — while the aggregate,
// which counts both, still goes degraded.
func TestTwoDevicesSharingAKeyPublishNeither(t *testing.T) {
	before := disagreements(t, signalDeviceNameShared)

	state := fleetState(t, true,
		adoptedDevice("AP 1", deviceStateOnline),
		adoptedDevice("ap-1", deviceStateOffline))

	if value, present := state[stateKeyDevicePrefix+"ap-1"]; present {
		t.Errorf("a shared key should publish nothing, got %q", value)
	}
	if state[stateKeyDevices] != devicesDegraded {
		t.Errorf("state[devices] = %q, want %q: both devices still count", state[stateKeyDevices], devicesDegraded)
	}
	if after := disagreements(t, signalDeviceNameShared); after <= before {
		t.Errorf("a shared key should be counted as a disagreement, %v -> %v", before, after)
	}
}

// A device with no name at all counts towards the fleet — a dead AP is dead
// whether or not anyone named it — but there is no key to publish it under.
func TestANamelessDeviceStillCountsTowardsTheFleet(t *testing.T) {
	nameless := adoptedDevice("", deviceStateOffline)

	state := fleetState(t, true, adoptedDevice("Switch 48", deviceStateOnline), nameless)

	if state[stateKeyDevices] != devicesDegraded {
		t.Errorf("state[devices] = %q, want %q", state[stateKeyDevices], devicesDegraded)
	}
	if len(state) != 2 { // devices plus the one named device
		t.Errorf("expected only the aggregate and the named device's key, got %v", state)
	}
}

// Per-key degradation: the fleet keys and the gateway keys come from the same
// response and are still independent observations. A device list with no
// gateway in it reports the fleet, and a gateway with nothing adopted reports
// wan.
func TestFleetAndGatewayKeysDegradeIndependently(t *testing.T) {
	withoutGateway := fleetState(t, false, adoptedDevice("Switch 48", deviceStateOffline))
	if withoutGateway[stateKeyDevices] != devicesDegraded {
		t.Errorf("state[devices] = %q, want %q", withoutGateway[stateKeyDevices], devicesDegraded)
	}
	if _, present := withoutGateway[stateKeyWAN]; present {
		t.Error("wan should be absent when no gateway is in the list")
	}

	gateway := deviceRecord{Model: "UDMPRO", WAN1: &wanPort{IsUplink: true, Up: true}}
	withoutFleet := fleetState(t, false, gateway)
	if withoutFleet[stateKeyWAN] != wanPrimary {
		t.Errorf("state[wan] = %q, want %q", withoutFleet[stateKeyWAN], wanPrimary)
	}
	if value, present := withoutFleet[stateKeyDevices]; present {
		t.Errorf("devices should be absent when the gateway does not report adoption, got %q", value)
	}
}
