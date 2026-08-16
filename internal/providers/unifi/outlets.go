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
	"regexp"
	"slices"
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// This file READS outlet state. Switching one is write.go's, and the split is
// worth keeping: everything here runs on every poll for every install, and
// nothing in it can move a relay.
//
// It shipped first, and deliberately, because of what the captured table shows:
// outlets 1-4 report relay_group 1 and outlets 5-8 report relay_group 2. If the
// relay GROUP had been what the hardware switches, then asking for outlet 3 to
// go off would have taken outlets 1-4 with it, and one of those may be carrying
// the gateway, the switch or the storage. Observing was the instrument that
// settled it safely — publish the keys, toggle ONE outlet by hand, and read off
// whether one relay_state moved or four.
//
// It was settled on 2026-08-15: one outlet moved, its three group siblings did
// not. relay_group turned out to partition outlets by capability rather than by
// what switches together, which outlet_caps says in its own right. reportOutlets
// below still prints the reading, because a second UPS model is not obliged to
// behave like the first one and the line costs nothing on a device that does.

// outletFields is the outlet block a UniFi UPS reports, embedded in
// deviceRecord so it decodes from the same flat object.
type outletFields struct {
	// OutletTable carries the outlets. A device without one contributes no
	// outlet keys at all, which is the same "omit what you cannot see" rule the
	// rest of the state model follows.
	//
	// An EMPTY one contributes nothing either, and that is not a hypothetical:
	// the captured gateway reports "outlet_table": [], so having the field is
	// not the same as having outlets. A device is the outlet-bearing one when
	// it lists outlets, never when it merely mentions the field.
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
	// only so that it can be reported.
	//
	// That stayed true after the experiment settled what it means. It groups
	// outlets by capability, and the capability itself is in outlet_caps, which
	// is what the write path reads to tell a battery-backed outlet from a
	// surge-only one. Deriving the bank from a group NUMBER would be assuming
	// that group 1 is the battery-backed one on every model there will ever be.
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
	if slug != "" && !isPlaceholderOutletName(name) {
		return slug, true
	}
	if index == nil {
		return "", false
	}
	return strconv.Itoa(*index), true
}

// consoleOutletName matches the placeholder every outlet on this hardware ships
// with: "Outlet 1" … "Outlet 8", which is the index spelled out rather than a
// name.
//
// It is applied to the raw name rather than to the slug, and it is the ONLY
// copy of this rule. Two things consult it and they must never disagree: the
// state key, where a placeholder means the key falls back to the index, and the
// outlet write, where it means refusal — because an outlet nobody has named is
// an outlet nobody has said what is plugged into, and this action cuts mains.
// The CRD's admission rule on Outlet.name is written to the same pattern.
//
// A separator is required, so "Outlet3" reads as a placeholder and "Outlet
// cupboard" does not. If some firmware ships a default without one, the guard
// will not fire on it — the allowlist and the name check still stand, and the
// right fix would be to widen this pattern rather than to lean on them.
var consoleOutletName = regexp.MustCompile(`^ *[Oo]utlet[ _-]+[0-9]+ *$`)

// isPlaceholderOutletName reports whether a name is the console's own rather
// than one a person chose.
func isPlaceholderOutletName(name string) bool { return consoleOutletName.MatchString(name) }

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
		// version of a limitation. The write path does not have it — an outlet
		// action names its UPS by MAC — so a second UPS can be switched even
		// while only the first one's outlets are observable.
		log.Info("More than one adopted device reports an outlet table; only the first is published, "+
			"because outlet indexes restart on every chassis and merging them would put one device's "+
			"outlet under another's key. Switching an outlet on the ignored device still works, since "+
			"an outlet action names its ups by MAC. Please report it on issue #23",
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
// it. That comparison was the readout of hypothesis H1 on #60, and it stays
// because a second UPS model is not obliged to answer it the same way.
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
// this key shipped before any way to write one.
//
// Two lines, both at INFO because somebody deciding whether to allowlist an
// outlet is reading the default log stream rather than a debug one:
//
//   - the grouping itself, whenever it is first seen or changes. This is what
//     has to be understood before an automation is written against an outlet.
//   - what moved, whenever a relay_state changes, together with how much of its
//     relay group moved with it. One outlet of four is the individually
//     switchable answer, and it is what this hardware gave on 2026-08-15; four
//     of four would mean this model switches banks, and that the outlet actions
//     must not be pointed at it.
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
		log.Info("A UPS is reporting switchable outlets. On the hardware this was tested against they "+
			"switch INDIVIDUALLY and the relay group below is a capability split, not a switching "+
			"bank — but confirm it on yours before allowlisting anything for unifi.outlet.cut: toggle "+
			"ONE outlet by hand in the UniFi UI and read the next line",
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
		log.Info("Outlet state changed. If you are checking whether this ups switches an outlet or a "+
			"whole bank, this line is the readout", "moved", strings.Join(changes, ","),
			"relayGroup", group, "movedInGroup", len(changes), "outletsInGroup", size,
			"verdict", groupVerdict(len(changes), size))
	}
	if len(movedUngrouped) > 0 {
		slices.Sort(movedUngrouped)
		log.Info("Outlet state changed on outlets reporting no relay group, so nothing can be said "+
			"about what else would move with them", "moved", strings.Join(movedUngrouped, ","))
	}
}

// groupVerdict is what one poll's movement says about whether this ups switches
// an outlet or a bank. It is stated as a reading rather than a conclusion: one
// poll is evidence, and the operator toggling the outlet is the one who knows
// whether they touched one outlet or a bank.
func groupVerdict(moved, size int) string {
	switch {
	case size <= 1:
		return "inconclusive: a relay group of one cannot tell the two apart"
	case moved < size:
		return "outlets in this group moved independently of each other"
	default:
		return "every outlet in this group moved at once — if you toggled only one, this ups " +
			"switches a whole bank, and unifi.outlet.cut must not be pointed at it. Please report it"
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
