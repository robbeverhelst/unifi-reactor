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
	"maps"
	"slices"
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// This file READS outlet state and nothing else. There is deliberately no way
// to change an outlet from here — not behind a flag, not through a helper, not
// reachable at all — because nobody yet knows what a UniFi UPS does with a
// per-index write.
//
// The whole reason to observe them first is in the captured table: outlets 1-4
// report relay_group 1 and outlets 5-8 report relay_group 2. If the relay GROUP
// is what the hardware switches, then asking for outlet 3 to go off takes
// outlets 1-4 with it, and one of those may be carrying the gateway, the switch
// or the storage. The documented write path — outlet_overrides via PUT
// rest/device — comes from the USP-PDU-Pro and USP-Strip, which expose
// per-outlet power, current and cycle_enabled and have no relay_group at all,
// so it is documented for a different device class and settles nothing here.
//
// Observation is the instrument that settles it safely: with these keys
// published, a human toggles ONE outlet by hand in the UniFi UI and reads off
// whether one relay_state moved or four. That is hypothesis H1 on issue #60,
// and reportOutlets below prints its answer in words rather than leaving it to
// be reconstructed from a graph. Switching itself stays deferred in #23 until
// that experiment has been run.

// outletFields is the outlet block a UniFi UPS reports, embedded in
// deviceRecord so it decodes from the same flat object.
type outletFields struct {
	// OutletTable is present on a UniFi UPS and absent on everything else in
	// the captures. A device without one contributes no outlet keys at all,
	// which is the same "omit what you cannot see" rule the rest of the state
	// model follows.
	OutletTable []outletRecord `json:"outlet_table"`
}

// outletRecord is one switchable outlet, exactly as captured. Every field this
// derives anything from is a pointer, for the reason the fleet keys use
// pointers: absent is absent. An outlet that does not report relay_state is not
// an outlet that is off, and reading it as off would be the direction that
// invents an outage.
type outletRecord struct {
	// Index is the outlet's position on the chassis, and the fallback the key
	// is addressed by while nobody has named it.
	Index *int `json:"index"`
	// Name is "Outlet 1" … "Outlet 8" out of the box, which carries no
	// information the index does not. A name a person chose is what makes the
	// key readable; see outletSuffix.
	Name string `json:"name"`
	// RelayState is the switch position: true is closed, delivering mains.
	RelayState *bool `json:"relay_state"`
	// RelayGroup is the bank the outlet belongs to. NOTHING is derived from it
	// — it is not part of any key, any value, or any decision — and it is read
	// only so that it can be reported. It is the fact somebody has to know
	// before writing an automation against an outlet, and #23 cannot be
	// designed without it.
	RelayGroup *int `json:"relay_group"`
}

// outletSuffix is what an outlet's key is addressed by: its index while it
// carries the name the console gave it, and the slug of its name once somebody
// has named it something.
//
// "Outlet 3" is not a name, it is the index spelled out, so it is treated as
// the absence of one. Any name of that shape is — a UPS whose outlet 7 is
// called "Outlet 3" is a mislabelled console, and keying it as outlet.3 would
// put one outlet's state under another's address, which on a key that will one
// day be used to cut mains power is the worst available failure.
//
// The index is the fallback rather than the plan. An automation matching
// outlet.3 means something different the moment somebody re-plugs the rack,
// which is the same argument that made portName required rather than optional
// for unifi.poe.cycle in #66: hardware should be addressed by what it is, not
// by where it happens to be plugged.
func outletSuffix(index *int, name string) (string, bool) {
	slug := slugify(name)
	if slug != "" && !isConsoleOutletName(slug) {
		return slug, true
	}
	if index == nil {
		return "", false
	}
	return strconv.Itoa(*index), true
}

// isConsoleOutletName reports whether a slug is the console's own "Outlet N"
// placeholder rather than a name anyone chose.
func isConsoleOutletName(slug string) bool {
	digits, isOutlet := strings.CutPrefix(slug, "outlet-")
	if !isOutlet || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// outletTally accumulates one device's outlet table into per-outlet keys.
//
// One device's, not the fleet's: outlet indexes restart at 1 on every chassis,
// so two devices reporting outlet tables would both claim outlet.1. The first
// device reporting one wins, exactly as the first gateway and the first UPS do,
// and a second is named in a log line rather than silently merged.
type outletTally struct {
	// device is the slug of the device the outlets belong to, for the
	// diagnostic line.
	device string
	// claimed records that a device with an outlet table has been taken.
	claimed bool
	// perOutlet is each outlet's value, keyed by the full state key, and shared
	// holds the keys more than one outlet landed on — a mislabelled console
	// rather than an observation, published as neither outlet's state.
	perOutlet map[string]string
	shared    map[string]bool
	// group is each key's relay group, and groups the reverse: which keys share
	// a bank. Both are reported, never derived from.
	group  map[string]int
	groups map[int][]string
	// ungrouped are outlets that report no relay group at all, which is a
	// readable outlet whose blast radius is unknown.
	ungrouped []string
	// unreadable names the outlets that could not be read, so an outlet missing
	// from the keys is explained rather than merely absent.
	unreadable []string
	// ignored names devices whose outlet tables were skipped because another
	// device's table was already taken.
	ignored []string
}

func newOutletTally() *outletTally {
	return &outletTally{
		perOutlet: map[string]string{},
		shared:    map[string]bool{},
		group:     map[string]int{},
		groups:    map[int][]string{},
	}
}

// observe folds one adopted device's outlet table into the tally.
func (t *outletTally) observe(d deviceRecord) {
	if len(d.OutletTable) == 0 {
		return
	}
	name := slugify(d.Name)
	if name == "" {
		name = strings.ToLower(d.Model)
	}
	if t.claimed {
		t.ignored = append(t.ignored, name)
		return
	}
	t.claimed, t.device = true, name

	for _, outlet := range d.OutletTable {
		suffix, addressable := outletSuffix(outlet.Index, outlet.Name)
		if !addressable {
			// No index and no name is an outlet with no address. It exists, and
			// there is nothing to publish it under.
			t.unreadable = append(t.unreadable, "(unaddressable outlet)")
			continue
		}
		key := stateKeyOutletPrefix + suffix
		if outlet.RelayState == nil {
			// Absent is not off. An outlet whose switch position is unknown
			// publishes nothing, and is named here instead.
			t.unreadable = append(t.unreadable, key+"=no relay_state")
			continue
		}

		value := outletOff
		if *outlet.RelayState {
			value = outletOn
		}
		if _, already := t.perOutlet[key]; already {
			t.shared[key] = true
		}
		t.perOutlet[key] = value

		if outlet.RelayGroup == nil {
			t.ungrouped = append(t.ungrouped, key)
			continue
		}
		t.group[key] = *outlet.RelayGroup
		t.groups[*outlet.RelayGroup] = append(t.groups[*outlet.RelayGroup], key)
	}
}

// publish writes the outlet keys into the state map and returns what was
// observed, so the next poll can say which outlets moved together.
func (t *outletTally) publish(ctx context.Context, state map[string]string) outletSnapshot {
	log := logf.FromContext(ctx).WithName("unifi-outlets")
	if !t.claimed {
		return outletSnapshot{}
	}
	if len(t.ignored) > 0 {
		// Merging two chassis' outlet tables would put one device's outlet 1
		// under the other's key. Naming the ignored device is the honest
		// version of a limitation; #23 has to decide how a second one is
		// addressed before either can be switched.
		log.Info("More than one adopted device reports an outlet table; only the first is published, "+
			"because outlet indexes restart on every chassis and merging them would put one device's "+
			"outlet under another's key. Please report it on issue #23",
			"published", t.device, "ignored", strings.Join(t.ignored, ","))
	}

	snapshot := outletSnapshot{
		device: t.device,
		state:  map[string]string{},
		group:  map[string]int{},
		groups: map[int][]string{},
	}
	for key, value := range t.perOutlet {
		if t.shared[key] {
			// Two outlets addressed the same way, which happens when a console
			// has two outlets named the same thing, or one named after another
			// one's index. Publishing either would be arbitrary, and on a key
			// that names something carrying mains power the arbitrary choice is
			// not one to make.
			metrics.SignalsDisagreed(ProviderName, signalOutletNameShared)
			log.Info("Two or more outlets are addressed by the same key, so that key reports neither of "+
				"them; rename one on the console to tell them apart", "key", key)
			continue
		}
		state[key] = value
		snapshot.state[key] = value
		if group, grouped := t.group[key]; grouped {
			snapshot.group[key] = group
			snapshot.groups[group] = append(snapshot.groups[group], key)
		}
	}
	for _, members := range snapshot.groups {
		slices.Sort(members)
	}
	snapshot.ungrouped = t.ungroupedPublished(snapshot.state)
	snapshot.grouping = describeGrouping(snapshot.groups, snapshot.ungrouped)

	log.V(1).Info("outlets", "device", t.device, "outlets", describeOutlets(snapshot.state),
		"relayGroups", snapshot.grouping, "outletsUnreadable", strings.Join(t.unreadable, ","))
	return snapshot
}

// ungroupedPublished is the outlets that reported no relay group and survived
// the shared-key check.
func (t *outletTally) ungroupedPublished(published map[string]string) []string {
	var keys []string
	for _, key := range t.ungrouped {
		if _, ok := published[key]; ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// outletSnapshot is one poll's outlet picture, kept so the next poll can report
// not just that an outlet moved but whether its whole relay group moved with
// it. That comparison is the entire readout of hypothesis H1 on #60.
type outletSnapshot struct {
	device string
	// state is each published key's value.
	state map[string]string
	// group is each key's relay group; groups is the reverse, sorted.
	group  map[string]int
	groups map[int][]string
	// grouping is the rendered form, so "has the grouping changed" is a string
	// comparison and the answer can be logged verbatim.
	grouping string
	// ungrouped are the published keys reporting no relay group.
	ungrouped []string
}

// observed reports whether any outlet was seen at all.
func (s outletSnapshot) observed() bool { return len(s.state) > 0 }

// reportOutlets says out loud what an outlet change means, and it is the reason
// this key ships before any way to write one.
//
// Two lines, both at INFO because the person running the experiment on #60 is
// reading the default log stream rather than a debug one:
//
//   - the grouping itself, whenever it is first seen or changes. This is what
//     has to be understood before an automation is written against an outlet.
//   - what moved, whenever a relay_state changes, together with how much of its
//     relay group moved with it. One outlet of four is the individually
//     switchable answer; four of four is the relay-group answer, and #23's API
//     has to be per-group and say so.
//
// The reading is taken from the raw observation rather than from reported
// state, which is the same thing here: outlet keys are debounced at 1 sample,
// because a relay is a switch position and not a measurement.
func (c *Client) reportOutlets(ctx context.Context, current outletSnapshot) {
	log := logf.FromContext(ctx).WithName("unifi-outlets")

	c.mu.Lock()
	previous := c.previous.outlets
	c.previous.outlets = current
	c.mu.Unlock()

	if !current.observed() {
		return
	}
	if !previous.observed() || previous.grouping != current.grouping {
		log.Info("A UPS is reporting switchable outlets. Reactor only READS them — switching is deferred "+
			"in issue #23 until the relay grouping below is understood: if the relay group is what the "+
			"hardware switches, changing one outlet changes every outlet in its group. To settle it, "+
			"toggle ONE outlet by hand in the UniFi UI and read the next line",
			"device", current.device, "relayGroups", current.grouping,
			"outlets", describeOutlets(current.state))
		return
	}

	moved := map[int][]string{}
	var movedUngrouped []string
	for key, value := range current.state {
		was, known := previous.state[key]
		if !known || was == value {
			continue
		}
		change := key + "=" + was + "->" + value
		if group, grouped := current.group[key]; grouped {
			moved[group] = append(moved[group], change)
			continue
		}
		movedUngrouped = append(movedUngrouped, change)
	}

	for _, group := range slices.Sorted(maps.Keys(moved)) {
		changes := moved[group]
		slices.Sort(changes)
		size := len(current.groups[group])
		log.Info("Outlet state changed. If you are running the relay-group experiment on issue #60, "+
			"this line is its readout", "moved", strings.Join(changes, ","),
			"relayGroup", group, "movedInGroup", len(changes), "outletsInGroup", size,
			"verdict", groupVerdict(len(changes), size))
	}
	if len(movedUngrouped) > 0 {
		slices.Sort(movedUngrouped)
		log.Info("Outlet state changed on outlets reporting no relay group, so nothing can be said "+
			"about what else would move with them", "moved", strings.Join(movedUngrouped, ","))
	}
}

// groupVerdict is what one poll's movement says about the question #23 is
// blocked on. It is stated as a reading rather than a conclusion: one poll is
// evidence, and the operator toggling the outlet is the one who knows whether
// they touched one outlet or a bank.
func groupVerdict(moved, size int) string {
	switch {
	case size <= 1:
		return "inconclusive: a relay group of one cannot tell the two apart"
	case moved < size:
		return "outlets in this group moved independently of each other"
	default:
		return "every outlet in this group moved at once — if you toggled only one, " +
			"the relay group is the switching unit and #23 must be per-group"
	}
}

// describeGrouping renders which outlets share a bank: "1=[outlet.1 outlet.2]".
func describeGrouping(groups map[int][]string, ungrouped []string) string {
	parts := make([]string, 0, len(groups)+1)
	for _, group := range slices.Sorted(maps.Keys(groups)) {
		parts = append(parts, strconv.Itoa(group)+"=["+strings.Join(groups[group], " ")+"]")
	}
	if len(ungrouped) > 0 {
		parts = append(parts, "none=["+strings.Join(ungrouped, " ")+"]")
	}
	return strings.Join(parts, " ")
}

// describeOutlets renders each outlet's current position, in key order.
func describeOutlets(state map[string]string) string {
	parts := make([]string, 0, len(state))
	for _, key := range slices.Sorted(maps.Keys(state)) {
		parts = append(parts, key+"="+state[key])
	}
	return strings.Join(parts, ",")
}
