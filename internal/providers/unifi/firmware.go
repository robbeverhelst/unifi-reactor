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
)

// firmwareFields are the fields the firmware key is derived from, embedded in
// deviceRecord so they decode from the same flat object.
//
// NONE of these has been captured. The committed records carry version and
// displayable_version and nothing else about upgrades, so every field here is
// written to the shape UniFi's own API documents and named in issue #12, and
// added to the allowlist in hack/capture-unifi.sh so the next real capture
// settles them. See the live-verification list in testdata/unifi/README.md.
type firmwareFields struct {
	// Version is the firmware the device is running now, and the one field here
	// that IS in both committed captures. Diagnostics only: it gives the log
	// line a "from" to go with the "to" below.
	Version string `json:"version"`
	// Upgradable is the console's own answer to "is there a newer firmware for
	// this device", and the only field this key is derived from.
	//
	// A pointer, because absent is not false. A firmware that renamed the field,
	// or a device type that does not report it, would otherwise be read as "up
	// to date" — which is the wrong direction to be wrong in for a key whose
	// whole job is to say that an update exists.
	Upgradable *bool `json:"upgradable"`
	// UpgradeToFirmware and RequiredVersion are diagnostics: which version the
	// console would move a device to, and the minimum the controller wants.
	// Never derived from — a version string is not something spec.when could
	// match, and one series per version is not something Prometheus should keep.
	UpgradeToFirmware string `json:"upgrade_to_firmware"`
	RequiredVersion   string `json:"required_version"`
	// SafeForAutoupgrade, ModelInEOL and ModelInLTS are diagnostics too, and
	// the reason they are read at all is that they are what makes an upgrade
	// decision: a model past end of life is not going to get the fix you are
	// waiting for. See the note on why none of them is a key of its own.
	SafeForAutoupgrade *bool `json:"safe_for_autoupgrade"`
	ModelInEOL         *bool `json:"model_in_eol"`
	ModelInLTS         *bool `json:"model_in_lts"`
}

// firmwareTally accumulates the fleet's upgrade state.
//
// One key for the whole fleet rather than one per device, for the reason
// device.<name> is opt-in: a per-device firmware key would multiply series by
// fleet size, and "is there anything to upgrade" is the question. Which devices
// answers it — issue #12's acceptance criterion — is a log line.
type firmwareTally struct {
	// reporting is how many adopted devices answered the question at all. Zero
	// publishes no key: a console that says nothing about upgrades is not a
	// console saying everything is current.
	reporting int
	// upgradable names the devices with an update waiting, and silent the ones
	// that did not report the field, both for the diagnostic line.
	upgradable []string
	silent     []string
	// eol and lts are counted rather than named: they are properties of a model
	// rather than of an install, and they do not change between polls.
	eol, lts int
}

// observe folds one adopted device into the tally.
func (t *firmwareTally) observe(d deviceRecord) {
	name := slugify(d.Name)
	if name == "" {
		name = strings.ToLower(d.Model)
	}
	if d.ModelInEOL != nil && *d.ModelInEOL {
		t.eol++
	}
	if d.ModelInLTS != nil && *d.ModelInLTS {
		t.lts++
	}

	if d.Upgradable == nil {
		t.silent = append(t.silent, name)
		return
	}
	t.reporting++
	if !*d.Upgradable {
		return
	}
	if d.UpgradeToFirmware != "" {
		name += "=" + d.Version + "->" + d.UpgradeToFirmware
	}
	t.upgradable = append(t.upgradable, name)
}

// publish writes the firmware key, or withholds it when nothing answered.
func (t *firmwareTally) publish(ctx context.Context, state map[string]string) {
	log := logf.FromContext(ctx).WithName("unifi-firmware")
	slices.Sort(t.silent)
	if t.reporting == 0 {
		log.V(1).Info("No adopted device reports whether it is upgradable; firmware will not be published",
			"devicesSilent", strings.Join(t.silent, ","))
		return
	}

	state[stateKeyFirmware] = firmwareCurrent
	if len(t.upgradable) > 0 {
		state[stateKeyFirmware] = firmwareUpdatesAvailable
	}
	// Which devices, and what would move where: #12 asks for that to be visible
	// somewhere, and a log line is where a version string belongs. It is not a
	// state value (spec.when compares strings, and one series per version is
	// the cardinality failure isp would have been) and it is not one per key.
	slices.Sort(t.upgradable)
	log.V(1).Info("firmware", "firmware", state[stateKeyFirmware],
		"devicesReporting", t.reporting, "devicesUpgradable", strings.Join(t.upgradable, ","),
		"devicesSilent", strings.Join(t.silent, ","), "modelsPastEOL", t.eol, "modelsOnLTS", t.lts)
}
