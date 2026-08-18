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
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// These tests are about issue #34: the wan mapping has never seen a real
// failover, so every payload below is a *hypothesis* about what one looks
// like, derived here in code from the one capture that is ground truth
// (testdata/unifi/api/stat-device-gateway.json, taken with the primary uplink
// live and the backup down).
//
// None of them is committed to testdata/. A file in testdata/ claims to have
// come off a console, and none of these did. The transformation each one
// applies is spelled out in this file precisely so that a reader can tell what
// was observed from what was assumed — and so that when a real failover is
// finally captured, the difference between it and these guesses is visible.
//
// hack/mock-unifi serves the same hypotheses over HTTP for hand-testing; the
// capture runbook that settles which of them is real is in
// testdata/unifi/README.md.

const (
	gatewayCapture = "stat-device-gateway.json"

	// gatewayModel is the console model every capture and hand-built gateway
	// record in these tests reports.
	gatewayModel = "UDMPRO"

	// capturedISP is the slug of the carrier in the committed capture. The
	// capture carries a placeholder name rather than the console's real one —
	// the isp key is a real feature, so a fixture has to carry *a* carrier, and
	// never had to carry the true one. See hack/capture-unifi.sh.
	capturedISP = "example-telecom"

	// backupCarrier is an obviously synthetic ISP name. The real backup carrier
	// is unknown — the 5G uplink has never carried traffic — so inventing a
	// plausible one would be inventing ground truth.
	backupCarrier     = "Mock Backup Carrier"
	backupCarrierSlug = "mock-backup-carrier"

	// statusFailed is a guess at what a downed uplink reports in
	// last_wan_status; only "online" has ever been observed. Nothing in the
	// provider derives state from that field, which is why a guess is safe to
	// use in a test that only exercises the mismatch reporting.
	statusFailed = "failed"

	// loggedDisagreement is the substring every cross-check warning shares.
	loggedDisagreement = "disagree"

	// loggedUnclaimed is the line the provider logs when is_uplink resolves
	// nothing and the gateway's uplink interface decides instead — the path a
	// cellular failover takes, since the cellular entry has no is_uplink key.
	loggedUnclaimed = "is_uplink does not name a single live WAN port"

	// cellularIfName is the interface the live hardware behind #104 reports
	// for its cellular uplink: a GRE tunnel, not a physical WAN port.
	cellularIfName = "gre1"
)

// wan returns the collected entry for wanN, so a hypothesis edits "wan2"
// rather than a slice position. Asking for an index the record does not carry
// is a bug in the test.
func wan(d *deviceRecord, index int) *wanEntry {
	for i := range d.WANs {
		if d.WANs[i].Index == index {
			return &d.WANs[i]
		}
	}
	panic(fmt.Sprintf("no wan%d in this record", index))
}

// withCellularBackup appends a third uplink shaped exactly as the gateway
// behind #104 reports its cellular backup: a GRE tunnel, up and holding an
// address — and with no is_uplink key at all, which the plain-bool field
// decodes as false. It goes in as wan3 so the fixture's wired pair stays the
// observed ground truth.
func withCellularBackup(d *deviceRecord) {
	d.WANs = append(d.WANs, wanEntry{Index: 3, wanPort: wanPort{
		Up:     true,
		IfName: cellularIfName,
		Name:   cellularIfName,
		IP:     "203.0.113.11",
	}})
}

// gatewayFromCapture returns the committed gateway record with one
// transformation applied, so the starting point of every hypothesis is real.
func gatewayFromCapture(t *testing.T, apply func(*deviceRecord)) deviceStatResponse {
	t.Helper()
	var parsed deviceStatResponse
	if err := json.Unmarshal(captured(t, gatewayCapture), &parsed); err != nil {
		t.Fatalf("parsing %s: %v", gatewayCapture, err)
	}
	if len(parsed.Data) != 1 {
		t.Fatalf("%s should hold exactly one gateway record, got %d", gatewayCapture, len(parsed.Data))
	}
	if apply != nil {
		apply(&parsed.Data[0])
	}
	return parsed
}

// logged captures what the provider says while deriving state, so the tests
// can assert on the disagreements it is supposed to report rather than only on
// the value it returns.
func logged(t *testing.T) (context.Context, func() string) {
	t.Helper()
	var mu sync.Mutex
	var sink strings.Builder
	logger := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		sink.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 1})
	return logf.IntoContext(context.Background(), logger), func() string {
		mu.Lock()
		defer mu.Unlock()
		return sink.String()
	}
}

// The capture itself: WAN1 live, and every signal agreeing about it. This is
// the only row in this file that is an observation rather than a hypothesis.
func TestCapturedGatewayHasEverySignalAgreeing(t *testing.T) {
	ctx, logs := logged(t)
	c := NewClient("", nil, "", false)

	state, err := c.stateFromDevices(ctx, gatewayFromCapture(t, nil))
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyWAN] != wanPrimary {
		t.Errorf("state[wan] = %q, want %q", state[stateKeyWAN], wanPrimary)
	}
	if state[stateKeyISP] != capturedISP {
		t.Errorf("state[isp] = %q, want %q", state[stateKeyISP], capturedISP)
	}
	if strings.Contains(logs(), loggedDisagreement) {
		t.Errorf("the capture's signals all agree, so nothing should be reported as a disagreement:\n%s", logs())
	}
}

// The hypotheses. Each says which fields a real failover moves; the assertions
// say what the provider does about it.
func TestFailoverHypotheses(t *testing.T) {
	tests := []struct {
		name string
		// what this hypothesis says a failover looks like
		apply func(*deviceRecord)
		// what the provider should publish under it
		wantWAN string
		wantISP string
		// substrings the provider must have logged, so that a hypothesis it
		// cannot resolve is never resolved silently
		wantLogged []string
	}{
		{
			// Every signal moves together. If this is what a failover looks
			// like, the current mapping is simply right.
			name: "every signal moves",
			apply: func(d *deviceRecord) {
				wan(d, 1).IsUplink, wan(d, 1).Up = false, false
				wan(d, 2).IsUplink, wan(d, 2).Up = true, true
				d.Uplink.Name = wan(d, 2).IfName
				d.LastWANStatus = map[string]any{wanStatusKeyPrimary: statusFailed, wanStatusKeyBackup: wanStatusOnline}
				d.ISP = backupCarrier
			},
			wantWAN: wanBackup,
			wantISP: backupCarrierSlug,
		},
		{
			// is_uplink moves and nothing else does. The mapping still gets
			// the right answer, but two signals now contradict it, which is
			// worth knowing about before trusting either.
			name: "only is_uplink moves",
			apply: func(d *deviceRecord) {
				wan(d, 1).IsUplink = false
				wan(d, 2).IsUplink, wan(d, 2).Up = true, true
			},
			wantWAN:    wanBackup,
			wantISP:    capturedISP,
			wantLogged: []string{loggedDisagreement, `"byIsUplink"="backup"`, `"byUplinkName"="primary"`},
		},
		{
			// The dangerous one: is_uplink turns out to mean "the port
			// configured as the uplink" and stays pinned to WAN1, while
			// everything else moves. The mapping reports primary right through
			// a failover — so the only defence is that it says so loudly.
			name: "is_uplink stays pinned while everything else moves",
			apply: func(d *deviceRecord) {
				wan(d, 2).Up = true
				d.Uplink.Name = wan(d, 2).IfName
				d.LastWANStatus = map[string]any{wanStatusKeyPrimary: statusFailed, wanStatusKeyBackup: wanStatusOnline}
				d.ISP = backupCarrier
			},
			wantWAN: wanPrimary,
			wantISP: backupCarrierSlug,
			wantLogged: []string{
				loggedDisagreement,
				"does not report itself as online",
			},
		},
		{
			// is_uplink means "configured", so both ports claim it. Before
			// uplink.name was consulted this reported backup unconditionally,
			// for no reason beyond the order of a switch statement.
			name: "both ports claim the uplink",
			apply: func(d *deviceRecord) {
				wan(d, 2).IsUplink, wan(d, 2).Up = true, true
			},
			wantWAN:    wanPrimary,
			wantISP:    capturedISP,
			wantLogged: []string{loggedUnclaimed, `"bothPortsClaimedUplink"=true`},
		},
		{
			// The switchover window: the old uplink has dropped and the new
			// one has not been claimed yet. Without a second signal the key
			// vanishes here, which reads to every Automation as "the gateway
			// disappeared".
			name: "neither port claims the uplink mid-switchover",
			apply: func(d *deviceRecord) {
				wan(d, 1).IsUplink, wan(d, 1).Up = false, false
				wan(d, 2).Up = true
				d.Uplink.Name = wan(d, 2).IfName
			},
			wantWAN:    wanBackup,
			wantISP:    capturedISP,
			wantLogged: []string{loggedUnclaimed},
		},
		{
			// A gateway reporting wan1 and wan3 with no wan2 — nothing promises
			// the numbering is dense, so a gap must resolve exactly as the
			// contiguous record does.
			name: "a numbering gap still resolves",
			apply: func(d *deviceRecord) {
				withCellularBackup(d)
				d.WANs = slices.DeleteFunc(d.WANs, func(w wanEntry) bool { return w.Index == 2 })
				wan(d, 1).IsUplink, wan(d, 1).Up = false, false
				d.Uplink.Name = cellularIfName
			},
			wantWAN:    wanBackup,
			wantISP:    capturedISP,
			wantLogged: []string{loggedUnclaimed},
		},
		{
			// is_uplink contradicting itself across two backups. Both indices
			// mean backup, but which one carries the traffic is exactly what
			// the field failed to say, so it stays ambiguous — and with no
			// uplink block to break the tie, the provider reports backup and
			// says it may be wrong, as it always has on an ambiguous claim.
			name: "two backups both claim the uplink and nothing breaks the tie",
			apply: func(d *deviceRecord) {
				withCellularBackup(d)
				wan(d, 1).IsUplink, wan(d, 1).Up = false, false
				wan(d, 2).IsUplink, wan(d, 2).Up = true, true
				wan(d, 3).IsUplink = true
				d.Uplink = nil
			},
			wantWAN:    wanBackup,
			wantISP:    capturedISP,
			wantLogged: []string{"Both WAN ports report is_uplink"},
		},
		{
			// Nothing left to go on. The key is omitted rather than guessed,
			// which is what makes the engine hold the last known state instead
			// of running onExit actions mid-failover.
			name: "no signal at all",
			apply: func(d *deviceRecord) {
				wan(d, 1).IsUplink = false
				d.Uplink = nil
			},
			wantWAN: "",
			wantISP: capturedISP,
		},
		{
			// A failover hands the gateway a public address the console has
			// not geolocated yet.
			name: "the carrier is not known yet",
			apply: func(d *deviceRecord) {
				d.ISP = ""
			},
			wantWAN: wanPrimary,
			wantISP: ispUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, logs := logged(t)
			c := NewClient("", nil, "", false)

			state, err := c.stateFromDevices(ctx, gatewayFromCapture(t, tc.apply))
			if err != nil {
				t.Fatalf("stateFromDevices: %v", err)
			}
			if got := state[stateKeyWAN]; got != tc.wantWAN {
				t.Errorf("state[wan] = %q, want %q", got, tc.wantWAN)
			}
			if got := state[stateKeyISP]; got != tc.wantISP {
				t.Errorf("state[isp] = %q, want %q", got, tc.wantISP)
			}
			for _, want := range tc.wantLogged {
				if !strings.Contains(logs(), want) {
					t.Errorf("expected the provider to report %q, logged:\n%s", want, logs())
				}
			}
		})
	}
}

// The regression #104 is about, asserted the way the bug presented on real
// hardware: primary down, is_uplink claimed by nobody — the cellular entry
// carries no is_uplink key at all — and the gateway's uplink interface naming
// the third WAN's tunnel. The key must be PRESENT and read backup. Before
// every wanN was collected, this exact record produced no wan key, so a
// `when: {wan: backup}` Automation held "everything is fine" straight through
// the failover it was written for.
func TestCellularFailoverReportsBackup(t *testing.T) {
	ctx, logs := logged(t)
	c := NewClient("", nil, "", false)

	state, err := c.stateFromDevices(ctx, gatewayFromCapture(t, func(d *deviceRecord) {
		withCellularBackup(d)
		wan(d, 1).IsUplink, wan(d, 1).Up = false, false
		d.Uplink.Name = cellularIfName
	}))
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	value, present := state[stateKeyWAN]
	if !present {
		t.Fatalf("state carries no wan key at all — the failover to cellular is invisible, which is the bug #104 reported; state = %v", state)
	}
	if value != wanBackup {
		t.Fatalf("state[wan] = %q, want %q", value, wanBackup)
	}
	// The path taken must be the documented one: is_uplink resolves nothing,
	// the uplink interface decides.
	if !strings.Contains(logs(), loggedUnclaimed) {
		t.Errorf("expected the provider to report %q, logged:\n%s", loggedUnclaimed, logs())
	}
}

// The common case on the same hardware: three WANs visible, wired primary
// live. Nothing may change, and nothing may be reported as a disagreement —
// a third uplink at rest is not noise.
func TestACellularBackupAtRestChangesNothing(t *testing.T) {
	ctx, logs := logged(t)
	c := NewClient("", nil, "", false)

	state, err := c.stateFromDevices(ctx, gatewayFromCapture(t, withCellularBackup))
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyWAN] != wanPrimary {
		t.Errorf("state[wan] = %q, want %q", state[stateKeyWAN], wanPrimary)
	}
	if strings.Contains(logs(), loggedDisagreement) {
		t.Errorf("a cellular backup sitting idle is not a disagreement:\n%s", logs())
	}
}

// The decode itself: every wanN key is collected, in index order, with the
// named fields still decoding alongside them. This is JSON-level on purpose —
// absent-versus-false on is_uplink only exists at the decode boundary.
func TestEveryWANIsCollected(t *testing.T) {
	blob := `{
		"model": "UDMPRO",
		"wan10": {"up": false, "ifname": "eth10"},
		"wan1": {"is_uplink": true, "up": true, "ifname": "eth8"},
		"wan3": {"up": true, "ifname": "gre1"},
		"wan2": null,
		"wan_ip": "203.0.113.10"
	}`
	var d deviceRecord
	if err := json.Unmarshal([]byte(blob), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Model != gatewayModel {
		t.Errorf("Model = %q; the named fields must keep decoding next to the wanN collection", d.Model)
	}
	indices := make([]int, 0, len(d.WANs))
	for _, w := range d.WANs {
		indices = append(indices, w.Index)
	}
	// wan2 is null (a WAN the console did not report), wan_ip is not a WAN,
	// and wan10 must sort after wan3 numerically rather than lexically.
	if want := []int{1, 3, 10}; !slices.Equal(indices, want) {
		t.Fatalf("collected indices %v, want %v", indices, want)
	}
	if wan(&d, 3).IsUplink {
		t.Errorf("wan3 carries no is_uplink key, which must decode as false — absent claims nothing")
	}
	if !wan(&d, 1).IsUplink || wan(&d, 1).IfName != "eth8" {
		t.Errorf("wan1 = %+v, want the values it was given", *wan(&d, 1))
	}
}

// The cross-check issue #6 exists for: wan and isp are independent answers to
// "did the uplink change", and they are only meaningful together across two
// observations.
func TestISPAndWANDisagreementAcrossObservations(t *testing.T) {
	tests := []struct {
		name       string
		then       func(*deviceRecord)
		wantLogged string
	}{
		{
			name: "the uplink changed but the carrier did not",
			then: func(d *deviceRecord) {
				wan(d, 1).IsUplink = false
				wan(d, 2).IsUplink, wan(d, 2).Up = true, true
				d.Uplink.Name = wan(d, 2).IfName
			},
			wantLogged: "changed uplink but the ISP behind it did not change",
		},
		{
			name: "the carrier changed but the uplink did not",
			then: func(d *deviceRecord) {
				d.ISP = backupCarrier
			},
			wantLogged: "still reports the same uplink",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, logs := logged(t)
			c := NewClient("", nil, "", false)

			// First observation: the capture, unmodified. Nothing to compare
			// against yet, so nothing may be reported.
			if _, err := c.stateFromDevices(ctx, gatewayFromCapture(t, nil)); err != nil {
				t.Fatalf("first observation: %v", err)
			}
			if strings.Contains(logs(), "ISP") {
				t.Errorf("the first observation has nothing to disagree with:\n%s", logs())
			}
			if _, err := c.stateFromDevices(ctx, gatewayFromCapture(t, tc.then)); err != nil {
				t.Fatalf("second observation: %v", err)
			}
			if !strings.Contains(logs(), tc.wantLogged) {
				t.Errorf("expected the provider to report %q, logged:\n%s", tc.wantLogged, logs())
			}
		})
	}
}

// A failover where both signals move together is the case that must stay
// quiet: a warning that fires on a correct failover is a warning nobody reads.
func TestNoDisagreementWhenBothSignalsMoveTogether(t *testing.T) {
	ctx, logs := logged(t)
	c := NewClient("", nil, "", false)

	if _, err := c.stateFromDevices(ctx, gatewayFromCapture(t, nil)); err != nil {
		t.Fatalf("first observation: %v", err)
	}
	state, err := c.stateFromDevices(ctx, gatewayFromCapture(t, func(d *deviceRecord) {
		wan(d, 1).IsUplink, wan(d, 1).Up = false, false
		wan(d, 2).IsUplink, wan(d, 2).Up = true, true
		d.Uplink.Name = wan(d, 2).IfName
		d.LastWANStatus = map[string]any{wanStatusKeyPrimary: statusFailed, wanStatusKeyBackup: wanStatusOnline}
		d.ISP = backupCarrier
	}))
	if err != nil {
		t.Fatalf("second observation: %v", err)
	}
	if state[stateKeyWAN] != wanBackup || state[stateKeyISP] != backupCarrierSlug {
		t.Fatalf("state = %v, want wan=%q isp=%q", state, wanBackup, backupCarrierSlug)
	}
	for _, unwanted := range []string{loggedDisagreement, "did not change", "same uplink", "does not report itself"} {
		if strings.Contains(logs(), unwanted) {
			t.Errorf("a clean failover should report nothing, but logged %q:\n%s", unwanted, logs())
		}
	}
}

// A carrier that momentarily has no name must not read as a carrier change,
// and must not erase what was known before it.
func TestAnUnknownCarrierIsNotACarrierChange(t *testing.T) {
	ctx, logs := logged(t)
	c := NewClient("", nil, "", false)

	for _, apply := range []func(*deviceRecord){
		nil,
		func(d *deviceRecord) { d.ISP = "" },
		nil,
	} {
		if _, err := c.stateFromDevices(ctx, gatewayFromCapture(t, apply)); err != nil {
			t.Fatalf("stateFromDevices: %v", err)
		}
	}
	if strings.Contains(logs(), "ISP") {
		t.Errorf("a blank carrier name is not a carrier change:\n%s", logs())
	}
}

func TestISPNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"Example Telecom":    capturedISP,
		"Example Telecom NV": "example-telecom-nv",
		"  Proximus  NV  ":   "proximus-nv",
		"AT&T":               "at-t",
		"Orange (Belgium)":   "orange-belgium",
		"KPN B.V.":           "kpn-b-v",
		"example telecom":    capturedISP,
		"3 Ireland":          "3-ireland",
		"":                   "",
		"!!!":                "",
		"Comcast Cable Comm": "comcast-cable-comm",
	} {
		if got := slugify(input); got != want {
			t.Errorf("slugify(%q) = %q, want %q", input, got, want)
		}
	}
}
