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

	// capturedISP is the carrier in the committed capture, and its slug.
	capturedISP = "telenet"

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
)

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
				d.WAN1.IsUplink = false
				d.WAN2.IsUplink, d.WAN2.Up = true, true
				d.Uplink.Name = d.WAN2.IfName
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
		d.WAN1.IsUplink, d.WAN1.Up = false, false
		d.WAN2.IsUplink, d.WAN2.Up = true, true
		d.Uplink.Name = d.WAN2.IfName
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
		"Telenet":            capturedISP,
		"Telenet BV":         "telenet-bv",
		"  Proximus  NV  ":   "proximus-nv",
		"AT&T":               "at-t",
		"Orange (Belgium)":   "orange-belgium",
		"KPN B.V.":           "kpn-b-v",
		"telenet":            "telenet",
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
