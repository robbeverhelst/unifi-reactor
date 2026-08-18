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

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// dataUsageFields are the fields the data.usage key is derived from, embedded
// in deviceRecord so they decode from the same flat object.
//
// The console does the accounting: data_warning and data_limited are its own
// judgement of the SIM's traffic against whatever the plan is, so this key
// needs no byte counting, no persistence across restarts, and no threshold of
// its own — the one state key here whose threshold lives on somebody else's
// hardware.
type dataUsageFields struct {
	MBB *mbbBlock `json:"mbb"`
}

// mbbBlock is the gateway's mobile-broadband record. Only the SIM list is
// read; the rest of a real block is identifiers (a cell_id, a modem MAC) that
// nothing here needs and no capture may carry.
type mbbBlock struct {
	SIM []simSlot `json:"sim"`
}

// simSlot is one SIM slot, projected down to the six fields the data.usage
// key reads. A real entry also carries the SIM's iccid, the modem's imei, the
// PIN/PUK retry counters, the carrier's name in spn, and mcc/mnc/asn which
// together identify the country and network operator — the most sensitive
// block this API has shown so far. None of that may be decoded here: being
// read is what earns a field a place in the capture allowlist (#94), so a
// field added to this struct is a field the capture script starts keeping.
//
// The booleans are plain rather than pointers, and that is safe by
// construction rather than by accident: every one of them gates towards
// absence. An absent active, card_present or has_data_plan decodes to false
// and drops the key — the direction that publishes fewer keys — and the two
// flags the value comes from are only read once those gates have passed, so a
// truncated record cannot be read as headroom.
type simSlot struct {
	Active      bool `json:"active"`
	Slot        int  `json:"slot"`
	CardPresent bool `json:"card_present"`
	HasDataPlan bool `json:"has_data_plan"`
	DataWarning bool `json:"data_warning"`
	DataLimited bool `json:"data_limited"`
}

// dataUsageFrom derives data.usage from the active SIM in one modem record.
// An empty result means the key is not published, and that is the deliberate
// answer for most of this function: under means "there is an allowance and it
// is not close", so a slot with no card, a SIM with no plan, or a record with
// no active SIM has nothing to be under — publishing under there would report
// plenty of headroom for a site that has none.
func dataUsageFrom(ctx context.Context, d deviceRecord) string {
	log := logf.FromContext(ctx).WithName("unifi-data-usage")
	if d.MBB == nil {
		return ""
	}

	var active []simSlot
	for _, s := range d.MBB.SIM {
		if s.Active {
			active = append(active, s)
		}
	}
	switch len(active) {
	case 0:
		log.V(1).Info("No SIM reports itself active; data.usage will not be published")
		return ""
	case 1:
	default:
		// The console is contradicting itself, and which slot carries the
		// traffic is exactly what it has failed to say. Guessing could pick
		// the slot whose allowance is fine while the live one is over its cap,
		// so nothing is published and the contradiction is counted.
		slots := make([]string, len(active))
		for i, s := range active {
			slots[i] = strconv.Itoa(s.Slot)
		}
		metrics.SignalsDisagreed(ProviderName, signalSIMMultipleActive)
		log.Info("More than one SIM reports itself active, so which slot is live is unknown; "+
			"data.usage will not be published",
			"slots", strings.Join(slots, ","))
		return ""
	}

	sim := active[0]
	if !sim.CardPresent {
		log.V(1).Info("The active SIM slot has no card in it; data.usage will not be published",
			"slot", sim.Slot)
		return ""
	}
	if !sim.HasDataPlan {
		log.V(1).Info("The active SIM has no data plan; data.usage will not be published",
			"slot", sim.Slot)
		return ""
	}

	// A reached limit wins over a warning about approaching one: when both
	// flags are set, over is the one that is still true.
	value := dataUsageUnder
	switch {
	case sim.DataLimited:
		value = dataUsageOver
	case sim.DataWarning:
		value = dataUsageWarning
	}
	log.V(1).Info("data.usage", "data.usage", value, "slot", sim.Slot,
		"dataWarning", sim.DataWarning, "dataLimited", sim.DataLimited)
	return value
}
