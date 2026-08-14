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
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultMaxPoEUtilizationPercent is the share of a switch's PoE budget at or
// above which poe reports insufficient.
//
// Set against the debounce this key ships with, per #7's rule. PoE draw is an
// instantaneous measurement like ups.load — a camera's heater or an AP's radios
// coming up move it by tens of watts within one poll — so it settles over 3
// samples, and 90% leaves roughly one powered device's worth of headroom on a
// typical switch for those 90 seconds. Raise it towards 100 and there is no
// headroom left to react in; lower it and a switch that simply has a full
// complement of APs reports insufficient forever.
const DefaultMaxPoEUtilizationPercent = 90.0

// jsonNull is the literal a JSON null arrives as, which flexibleNumber has to
// recognise before it can distinguish "no reading" from a number.
const jsonNull = "null"

// poeFields are the fields the poe key is derived from, embedded in
// deviceRecord so they decode from the same flat object.
//
// NONE of these has been captured. No switch record exists in testdata at all —
// the UPS 2U is reported as a switch-type device but carries no port_table —
// so both are written to the shape UniFi's API documents and to the names issue
// #14 lists, and added to hack/capture-unifi.sh. See the live-verification list
// in testdata/unifi/README.md.
type poeFields struct {
	// TotalMaxPower is the switch's whole PoE budget in watts: the denominator.
	// A pointer, because a switch reporting no budget is not a switch with no
	// budget — and dividing by an absent field read as zero is the other way
	// this key could have gone wrong.
	TotalMaxPower *float64 `json:"total_max_power"`
	// PortTable is projected down hard in the capture script: a real one carries
	// per-port names, MACs and client counts, none of which this key needs.
	PortTable []devicePort `json:"port_table"`
}

// devicePort is one switch port's PoE state.
type devicePort struct {
	PortIdx *int `json:"port_idx"`
	// PoEEnable says the port is delivering power. A pointer: absent is not
	// "disabled", and a port whose state is unknown is not a port drawing
	// nothing.
	PoEEnable *bool `json:"poe_enable"`
	// PoEPower is the watts this port is delivering, and it is a flexible
	// number because UniFi is documented to report it as a STRING ("3.90") on
	// several firmwares while other endpoints use a number. Decoding it as one
	// or the other would make an entire switch's draw unreadable on the half of
	// the world that reports it the other way.
	PoEPower flexibleNumber `json:"poe_power"`
	// PoEClass is a diagnostic: which powering class the port negotiated. Never
	// derived from — it names a port that is powering something and will not
	// say how much, which is the case that makes a switch unreadable.
	PoEClass string `json:"poe_class"`
}

// describe names one port for a diagnostic line.
func (p devicePort) describe() string {
	name := "port"
	if p.PortIdx != nil {
		name += strconv.Itoa(*p.PortIdx)
	} else {
		name += "?"
	}
	if p.PoEClass != "" {
		name += "(class " + p.PoEClass + ")"
	}
	return name
}

// flexibleNumber decodes a JSON number, a numeric string, or null into an
// optional float.
//
// It exists for one field and it is deliberately narrow: nothing here coerces
// a value it does not understand into zero. Absent, null, empty and
// unparseable all mean "no reading", which the caller treats as missing rather
// than as no power.
type flexibleNumber struct {
	Value float64
	Known bool
}

func (f *flexibleNumber) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == jsonNull {
		return nil
	}
	if data[0] == '"' {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			// A string this provider cannot read as a number is a reading it
			// does not have, not a reading of zero. Failing the decode here
			// would take the whole observation — every other key included —
			// down with one unfamiliar port.
			return nil
		}
		f.Value, f.Known = value, true
		return nil
	}
	var value float64
	if err := json.Unmarshal(data, &value); err != nil {
		return nil
	}
	f.Value, f.Known = value, true
	return nil
}

// poeDraw is one switch's PoE picture: what it is delivering against what it
// can deliver.
type poeDraw struct {
	watts, budget float64
	// measurable is false when the switch cannot be read honestly — no budget,
	// or a port that is powering something and will not say how much.
	measurable bool
	// enabled is how many ports are powering something, and silent names the
	// ones among them that would not say how much.
	enabled int
	silent  []string
}

// poe reads one device's PoE draw.
//
// A port that is powering something and reports no wattage makes the whole
// switch unmeasurable rather than contributing zero. That is the "absent is not
// zero" rule at the only place on this key where it can hide a failure:
// under-counting the draw reports headroom that is not there, which is the
// exact situation #14 exists to catch.
func (d deviceRecord) poe() poeDraw {
	draw := poeDraw{}
	if d.TotalMaxPower == nil || *d.TotalMaxPower <= 0 || len(d.PortTable) == 0 {
		return draw
	}
	draw.budget = *d.TotalMaxPower
	for _, port := range d.PortTable {
		if port.PoEEnable == nil || !*port.PoEEnable {
			// Not powering anything. A port that is off draws nothing, and that
			// is a reading rather than an absence.
			continue
		}
		draw.enabled++
		if !port.PoEPower.Known {
			draw.silent = append(draw.silent, port.describe())
			continue
		}
		draw.watts += port.PoEPower.Value
	}
	draw.measurable = len(draw.silent) == 0
	return draw
}

// utilization is the draw as a percentage of the budget.
func (p poeDraw) utilization() float64 {
	return p.watts / p.budget * 100
}

// poeTally accumulates the fleet's PoE headroom into one bucketed key. The
// worst switch decides: one switch out of headroom drops the cameras on that
// switch, whatever the others have spare.
type poeTally struct {
	// switches is how many reported a budget and a readable port table.
	switches int
	// worst is the highest utilization anywhere, and worstSwitch the slug
	// behind it.
	worst       float64
	worstKnown  bool
	worstSwitch string
	// draws is every switch's numbers, and unreadable the ones that reported a
	// budget and would not say what they were delivering.
	draws      []string
	unreadable []string
}

// observe folds one adopted device into the tally. Devices with no PoE at all —
// a gateway, an AP, the UPS — simply contribute nothing.
func (t *poeTally) observe(d deviceRecord) {
	draw := d.poe()
	if draw.budget <= 0 {
		return
	}
	name := slugify(d.Name)
	if name == "" {
		name = strings.ToLower(d.Model)
	}
	if !draw.measurable {
		t.unreadable = append(t.unreadable, name+"="+strings.Join(draw.silent, "/")+
			" of "+strconv.Itoa(draw.enabled)+" powered ports report no wattage")
		return
	}

	t.switches++
	used := draw.utilization()
	t.draws = append(t.draws, name+"="+strconv.FormatFloat(draw.watts, 'f', 1, 64)+"/"+
		strconv.FormatFloat(draw.budget, 'f', 0, 64)+"W")
	if !t.worstKnown || used > t.worst {
		t.worst, t.worstKnown, t.worstSwitch = used, true, name
	}
}

// publishPoE buckets the worst switch's utilization against the threshold.
func (c *Client) publishPoE(ctx context.Context, state map[string]string, t poeTally) {
	log := logf.FromContext(ctx).WithName("unifi-poe")
	if t.switches == 0 {
		log.V(1).Info("No adopted switch reports a readable PoE budget; poe will not be published",
			"switchesUnreadable", strings.Join(t.unreadable, ","))
		return
	}

	maximum := c.MaxPoEUtilizationPercent
	if maximum <= 0 {
		maximum = DefaultMaxPoEUtilizationPercent
	}

	value := poeOK
	if t.worst >= maximum {
		value = poeInsufficient
	}
	state[stateKeyPoE] = value

	log.V(1).Info("poe", "poe", value, "worstUtilizationPercent", t.worst,
		"worstSwitch", t.worstSwitch, "thresholdPercent", maximum, "switchesMeasured", t.switches,
		"draws", strings.Join(t.draws, ","), "switchesUnreadable", strings.Join(t.unreadable, ","))
}
