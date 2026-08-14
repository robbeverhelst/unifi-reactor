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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// outlet builds one outlet_table entry. A nil relayState is an outlet that does
// not report its switch position, which is not an outlet that is off.
func outlet(index int, name string, relayState *bool, group *int) outletRecord {
	return outletRecord{Index: &index, Name: name, RelayState: relayState, RelayGroup: group}
}

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

// upsWithOutlets is an adopted UPS carrying an outlet table.
func upsWithOutlets(name string, outlets ...outletRecord) deviceRecord {
	d := adoptedDevice(name, deviceStateOnline)
	d.OutletTable = outlets
	return d
}

// capturedGrouping is the UPS 2U's own layout: outlets 1-4 on relay group 1,
// outlets 5-8 on relay group 2, all closed and unnamed.
func capturedGrouping() []outletRecord {
	var outlets []outletRecord
	for i := 1; i <= 8; i++ {
		group := 1
		if i > 4 {
			group = 2
		}
		outlets = append(outlets, outlet(i, "Outlet "+strconv.Itoa(i), boolPtr(true), &group))
	}
	return outlets
}

// outletState derives the outlet keys from a device list.
func outletState(t *testing.T, devices ...deviceRecord) map[string]string {
	t.Helper()
	c := NewClient("", nil, "", false)
	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: devices})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	return state
}

// The committed capture is the whole reason this key exists, so it is asserted
// against directly rather than against a hand-built record: eight outlets, all
// closed, addressed by index because none of them is named.
func TestOutletsFromTheCapture(t *testing.T) {
	c := serve(t, merged(t, "stat-device-gateway.json", "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for i := 1; i <= 8; i++ {
		key := stateKeyOutletPrefix + strconv.Itoa(i)
		if state[key] != outletOn {
			t.Errorf("state[%q] = %q, want %q", key, state[key], outletOn)
		}
	}
	count := 0
	for key := range state {
		if strings.HasPrefix(key, stateKeyOutletPrefix) {
			count++
		}
	}
	if count != 8 {
		t.Errorf("published %d outlet keys, want 8", count)
	}
}

// The relay grouping is the fact #23 is blocked on, so the parser has to read
// it off the capture rather than take anyone's word for it: 1-4 in one bank,
// 5-8 in the other.
func TestCapturedRelayGroupingIsReported(t *testing.T) {
	c := serve(t, captured(t, "stat-device-ups.json"))

	var parsed deviceStatResponse
	if err := decodeDevices(t, c, &parsed); err != nil {
		t.Fatalf("reading devices: %v", err)
	}
	tally := newOutletTally()
	for _, d := range parsed.Data {
		tally.observe(d)
	}
	snapshot := tally.publish(context.Background(), map[string]string{})

	want := "1=[outlet.1 outlet.2 outlet.3 outlet.4] 2=[outlet.5 outlet.6 outlet.7 outlet.8]"
	if snapshot.grouping != want {
		t.Errorf("grouping = %q, want %q", snapshot.grouping, want)
	}
}

// decodeDevices fetches and decodes the device endpoint the way Observe does,
// so a test can work with the parsed records.
func decodeDevices(t *testing.T, c *Client, out *deviceStatResponse) error {
	t.Helper()
	return c.get(context.Background(), "stat/device", out)
}

// "Outlet 3" is the console's placeholder rather than a name, so it addresses
// nothing the index does not already. A name somebody chose replaces the index,
// which is the entire argument for naming them.
func TestOutletKeyPrefersANameOverAnIndex(t *testing.T) {
	tests := []struct {
		name  string
		index *int
		given string
		want  string
		ok    bool
	}{
		{name: "console placeholder falls back to the index", index: intPtr(3), given: "Outlet 3", want: "3", ok: true},
		{name: "a placeholder naming another index is still a placeholder",
			index: intPtr(7), given: "Outlet 3", want: "7", ok: true},
		{name: "no name at all falls back to the index", index: intPtr(5), given: "", want: "5", ok: true},
		{name: "a chosen name wins", index: intPtr(3), given: "NAS", want: "nas", ok: true},
		{name: "a chosen name is slugified", index: intPtr(3), given: "Rack Switch 1", want: "rack-switch-1", ok: true},
		{name: "a name survives a missing index", index: nil, given: "NAS", want: "nas", ok: true},
		{name: "no index and no name is not addressable", index: nil, given: "Outlet 4", want: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := outletSuffix(tt.index, tt.given)
			if got != tt.want || ok != tt.ok {
				t.Errorf("outletSuffix(%v, %q) = %q, %v; want %q, %v", tt.index, tt.given, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestOutletValues(t *testing.T) {
	group := 1
	state := outletState(t, upsWithOutlets("UPS 2U",
		outlet(1, "Outlet 1", boolPtr(true), &group),
		outlet(2, "NAS", boolPtr(false), &group),
	))

	for key, want := range map[string]string{
		stateKeyOutletPrefix + "1":   outletOn,
		stateKeyOutletPrefix + "nas": outletOff,
	} {
		if state[key] != want {
			t.Errorf("state[%q] = %q, want %q", key, state[key], want)
		}
	}
}

// Absent is not off. An outlet that will not say what it is doing publishes
// nothing, because reading a missing relay_state as an open relay would invent
// an outage on whatever is plugged into it.
func TestOutletWithNoRelayStateIsOmitted(t *testing.T) {
	group := 1
	state := outletState(t, upsWithOutlets("UPS 2U",
		outlet(1, "Outlet 1", nil, &group),
		outlet(2, "Outlet 2", boolPtr(true), &group),
	))

	if _, published := state[stateKeyOutletPrefix+"1"]; published {
		t.Error("an outlet reporting no relay_state must publish no key at all")
	}
	if state[stateKeyOutletPrefix+"2"] != outletOn {
		t.Errorf("state[outlet.2] = %q, want %q", state[stateKeyOutletPrefix+"2"], outletOn)
	}
}

// A device with no outlet table contributes nothing, so an install with no UPS
// publishes no outlet key rather than a placeholder.
func TestNoOutletTableMeansNoKeys(t *testing.T) {
	state := outletState(t, adoptedDevice("Switch 48", deviceStateOnline))

	for key := range state {
		if strings.HasPrefix(key, stateKeyOutletPrefix) {
			t.Errorf("published %q with no outlet table anywhere", key)
		}
	}
}

// The captured gateway reports "outlet_table": [], so a device that merely
// mentions the field is not the outlet-bearing one. Taking the first device
// with the field rather than the first with outlets would claim the gateway's
// empty table and hide the UPS behind it.
func TestAnEmptyOutletTableIsNotAnOutletBearingDevice(t *testing.T) {
	gateway := adoptedDevice("Dream Machine Pro", deviceStateOnline)
	gateway.OutletTable = []outletRecord{}
	group := 1
	state := outletState(t, gateway, upsWithOutlets("UPS 2U", outlet(1, "Outlet 1", boolPtr(true), &group)))

	if state[stateKeyOutletPrefix+"1"] != outletOn {
		t.Errorf("state[outlet.1] = %q, want the UPS behind the gateway's empty table to be read",
			state[stateKeyOutletPrefix+"1"])
	}
}

// Two outlets addressed the same way publish neither. Picking one would be
// arbitrary, and this key names something carrying mains power.
func TestOutletsSharingAKeyPublishNeither(t *testing.T) {
	group := 1
	state := outletState(t, upsWithOutlets("UPS 2U",
		outlet(1, "NAS", boolPtr(true), &group),
		outlet(2, "nas", boolPtr(false), &group),
		outlet(3, "Outlet 3", boolPtr(true), &group),
	))

	if _, published := state[stateKeyOutletPrefix+"nas"]; published {
		t.Error("a key two outlets share must report neither of them")
	}
	if state[stateKeyOutletPrefix+"3"] != outletOn {
		t.Error("a collision must not take the outlets that did not collide with it")
	}
}

// Outlet indexes restart on every chassis, so a second device's table is
// ignored rather than merged into the first's keys.
func TestOnlyTheFirstOutletTableIsPublished(t *testing.T) {
	group := 1
	state := outletState(t,
		upsWithOutlets("UPS 2U", outlet(1, "Outlet 1", boolPtr(true), &group)),
		upsWithOutlets("PDU", outlet(1, "Outlet 1", boolPtr(false), &group)),
	)

	if state[stateKeyOutletPrefix+"1"] != outletOn {
		t.Errorf("state[outlet.1] = %q, want the first device's %q",
			state[stateKeyOutletPrefix+"1"], outletOn)
	}
}

// An outlet with no relay group is still readable — its position is a fact —
// but nothing can be said about what else would move with it.
func TestOutletWithNoRelayGroupIsStillPublished(t *testing.T) {
	tally := newOutletTally()
	tally.observe(upsWithOutlets("UPS 2U", outlet(1, "Outlet 1", boolPtr(true), nil)))
	state := map[string]string{}
	snapshot := tally.publish(context.Background(), state)

	if state[stateKeyOutletPrefix+"1"] != outletOn {
		t.Errorf("state[outlet.1] = %q, want %q", state[stateKeyOutletPrefix+"1"], outletOn)
	}
	if snapshot.grouping != "none=[outlet.1]" {
		t.Errorf("grouping = %q, want the outlet reported as ungrouped", snapshot.grouping)
	}
}

// The verdict is the sentence the H1 experiment on #60 is read off, so both
// hypotheses have to come out of it in words.
func TestGroupVerdictSaysWhichHypothesisHeld(t *testing.T) {
	tests := []struct {
		name        string
		moved, size int
		want        string
	}{
		{name: "one of four is the individually switchable answer", moved: 1, size: 4, want: "independently"},
		{name: "four of four is the relay-group answer", moved: 4, size: 4, want: "switching unit"},
		{name: "a group of one settles nothing", moved: 1, size: 1, want: "inconclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := groupVerdict(tt.moved, tt.size); !strings.Contains(got, tt.want) {
				t.Errorf("groupVerdict(%d, %d) = %q, want it to mention %q", tt.moved, tt.size, got, tt.want)
			}
		})
	}
}

// Both hypotheses are rehearsed end to end, through two consecutive
// observations, because a single poll cannot express either of them. This is
// the parser side of what hack/mock-unifi's /outlets endpoint drives by hand.
func TestBothRelayHypothesesAreObservable(t *testing.T) {
	tests := []struct {
		name string
		// off is the outlet indexes that open on the second observation.
		off  []int
		want map[string]string
	}{
		{
			name: "individual switching moves one outlet in the bank",
			off:  []int{5},
			want: map[string]string{
				stateKeyOutletPrefix + "5": outletOff,
				stateKeyOutletPrefix + "6": outletOn,
				stateKeyOutletPrefix + "7": outletOn,
				stateKeyOutletPrefix + "8": outletOn,
			},
		},
		{
			name: "group switching takes the whole bank",
			off:  []int{5, 6, 7, 8},
			want: map[string]string{
				stateKeyOutletPrefix + "5": outletOff,
				stateKeyOutletPrefix + "6": outletOff,
				stateKeyOutletPrefix + "7": outletOff,
				stateKeyOutletPrefix + "8": outletOff,
				stateKeyOutletPrefix + "1": outletOn,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient("", nil, "", false)
			ctx := context.Background()

			if _, err := c.stateFromDevices(ctx, deviceStatResponse{
				Data: []deviceRecord{upsWithOutlets("UPS 2U", capturedGrouping()...)},
			}); err != nil {
				t.Fatalf("first observation: %v", err)
			}

			opened := map[int]bool{}
			for _, index := range tt.off {
				opened[index] = true
			}
			second := make([]outletRecord, 0, len(capturedGrouping()))
			for _, o := range capturedGrouping() {
				if opened[*o.Index] {
					o.RelayState = boolPtr(false)
				}
				second = append(second, o)
			}
			state, err := c.stateFromDevices(ctx, deviceStatResponse{
				Data: []deviceRecord{upsWithOutlets("UPS 2U", second...)},
			})
			if err != nil {
				t.Fatalf("second observation: %v", err)
			}

			for key, want := range tt.want {
				if state[key] != want {
					t.Errorf("state[%q] = %q, want %q", key, state[key], want)
				}
			}
		})
	}
}

// The log line is the deliverable, not a side effect: #61 exists so that a
// human toggling one outlet by hand can read the answer to H1, and an answer
// nobody can see is not one. Both readings are asserted through two
// consecutive observations, at INFO, naming the relay group and how much of it
// moved.
func TestOutletChangeIsReportedWithItsRelayGroup(t *testing.T) {
	tests := []struct {
		name string
		off  []int
		want []string
	}{
		{
			name: "one outlet of four",
			off:  []int{5},
			want: []string{"outlet.5=on->off", "movedInGroup\"=1", "outletsInGroup\"=4", "independently"},
		},
		{
			name: "the whole bank",
			off:  []int{5, 6, 7, 8},
			want: []string{"outlet.8=on->off", "movedInGroup\"=4", "outletsInGroup\"=4", "switching unit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged strings.Builder
			ctx := logf.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
				logged.WriteString(prefix + " " + args + "\n")
			}, funcr.Options{}))
			c := NewClient("", nil, "", false)

			if _, err := c.stateFromDevices(ctx, deviceStatResponse{
				Data: []deviceRecord{upsWithOutlets("UPS 2U", capturedGrouping()...)},
			}); err != nil {
				t.Fatalf("first observation: %v", err)
			}
			// The first observation reports the grouping rather than a change,
			// because there is nothing yet to have changed from.
			if !strings.Contains(logged.String(), "1=[outlet.1 outlet.2 outlet.3 outlet.4]") {
				t.Errorf("the first observation must report the relay grouping; got:\n%s", logged.String())
			}
			logged.Reset()

			opened := map[int]bool{}
			for _, index := range tt.off {
				opened[index] = true
			}
			second := make([]outletRecord, 0, len(capturedGrouping()))
			for _, o := range capturedGrouping() {
				if opened[*o.Index] {
					o.RelayState = boolPtr(false)
				}
				second = append(second, o)
			}
			if _, err := c.stateFromDevices(ctx, deviceStatResponse{
				Data: []deviceRecord{upsWithOutlets("UPS 2U", second...)},
			}); err != nil {
				t.Fatalf("second observation: %v", err)
			}

			for _, want := range tt.want {
				if !strings.Contains(logged.String(), want) {
					t.Errorf("the readout must mention %q; got:\n%s", want, logged.String())
				}
			}
		})
	}
}

// Nothing here may change an outlet: not behind a flag, not through a helper,
// not reachable. #61's acceptance criteria say so in as many words, and this is
// the test that keeps it true after the issue is closed.
//
// Two guards, because "no write path" can be broken two ways. Writer is the
// only thing in this package that sends anything to a console, so no method on
// it may mention an outlet or a relay; and outlet_overrides is the field the
// documented PDU write path uses, so it may not appear in this package's source
// at all. Both must be deleted deliberately to implement #23, which is the
// point.
func TestNoOutletWritePathExists(t *testing.T) {
	for method := range reflect.TypeFor[*Writer]().Methods() {
		name := strings.ToLower(method.Name)
		if strings.Contains(name, "outlet") || strings.Contains(name, "relay") {
			t.Errorf("Writer.%s can change an outlet; #23 is deferred and outlet state is read-only",
				method.Name)
		}
	}

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing package sources: %v", err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		// String literals only, never comments: outlet_overrides is discussed
		// at length in this package and implemented nowhere, and a guard that
		// could not tell those apart would only teach people to stop explaining
		// themselves.
		parsed, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", source, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && literal.Kind == token.STRING && strings.Contains(literal.Value, "outlet_overrides") {
				t.Errorf("%s builds outlet_overrides, the field the documented write path uses; "+
					"outlet state is observed and never written until #23 is decided", source)
			}
			return true
		})
	}
}
