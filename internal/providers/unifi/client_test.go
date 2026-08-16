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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captured loads a real captured stat/device response from testdata.
func captured(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "unifi", "api", name))
	if err != nil {
		t.Fatalf("reading captured payload: %v", err)
	}
	return b
}

// merged builds one device list from several captured responses, the way a
// real controller returns every device in a single call.
func merged(t *testing.T, names ...string) []byte {
	t.Helper()
	var all deviceStatResponse
	for _, name := range names {
		var parsed deviceStatResponse
		if err := json.Unmarshal(captured(t, name), &parsed); err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		all.Data = append(all.Data, parsed.Data...)
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("marshalling merged payload: %v", err)
	}
	return b
}

// serve answers both endpoints an observation reads: the given device payload,
// and the captured health response. Passing a nil health payload makes
// stat/health fail, which is the case where the console answers about its
// hardware but not about its own health.
func serve(t *testing.T, payload []byte) *Client {
	t.Helper()
	return serveBoth(t, payload, captured(t, "stat-health.json"))
}

func serveBoth(t *testing.T, devices, health []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-KEY"); got != "test-key" {
			t.Errorf("expected X-API-KEY header, got %q", got)
		}
		switch r.URL.Path {
		case "/proxy/network/api/s/default/stat/device":
			_, _ = w.Write(devices)
		case "/proxy/network/api/s/default/stat/health":
			if health == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(health)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, StaticAPIKey("test-key"), "", false)
}

func TestObserveAgainstCapturedGatewayAndUPS(t *testing.T) {
	c := serve(t, merged(t, "stat-device-gateway.json", "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// The captures were taken with WAN1 active, a placeholder carrier, the
	// UPS on mains at 97%, the www subsystem reporting ok, and WAN1 at 100%
	// availability and 16 ms average latency.
	for key, want := range map[string]string{
		stateKeyWAN:        wanPrimary,
		stateKeyWANQuality: wanQualityGood,
		stateKeyISP:        capturedISP,
		stateKeyInternet:   internetOK,
		stateKeyUPS:        upsOnline,
		stateKeyUPSBattery: batteryNormal,
		// The captured UPS reports 1043s of runtime and 310W of a 1000W
		// budget, so both derived keys read the healthy value.
		stateKeyUPSRuntime: upsRuntimeAmple,
		stateKeyUPSLoad:    upsLoadNormal,
	} {
		if state[key] != want {
			t.Errorf("state[%q] = %q, want %q", key, state[key], want)
		}
	}
}

// Partial hardware is the normal case, not an edge case: plenty of consoles
// have a gateway and no UniFi UPS, and a UPS can drop off a console that still
// reports its gateway. Neither may take the other's keys down with it.
func TestObserveWithoutUPSOmitsUPSKeys(t *testing.T) {
	c := serve(t, captured(t, "stat-device-gateway.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state[stateKeyWAN] != wanPrimary {
		t.Errorf("state[wan] = %q, want %q", state[stateKeyWAN], wanPrimary)
	}
	if state[stateKeyISP] != capturedISP {
		t.Errorf("state[isp] = %q, want %q", state[stateKeyISP], capturedISP)
	}
	for _, key := range []string{stateKeyUPS, stateKeyUPSBattery, stateKeyUPSRuntime, stateKeyUPSLoad} {
		if _, present := state[key]; present {
			t.Errorf("%s should be absent when no UPS is visible to the controller", key)
		}
	}
}

func TestObserveWithoutAGatewayStillReportsTheUPS(t *testing.T) {
	c := serve(t, captured(t, "stat-device-ups.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state[stateKeyUPS] != upsOnline {
		t.Errorf("state[ups] = %q, want %q", state[stateKeyUPS], upsOnline)
	}
	// A UPS record carries an uplink block of its own (the switch port it
	// hangs off). It is not a gateway, so it must not produce wan or isp.
	for _, key := range []string{stateKeyWAN, stateKeyISP} {
		if got, present := state[key]; present {
			t.Errorf("%s should be absent when no gateway is visible, got %q", key, got)
		}
	}
}

func TestWANBackupWhenWAN2IsUplink(t *testing.T) {
	c := NewClient("", nil, "", false)
	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{
		Model: "UDMPRO",
		WAN1:  &wanPort{IsUplink: false, Up: false},
		WAN2:  &wanPort{IsUplink: true, Up: true},
	}}})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyWAN] != wanBackup {
		t.Fatalf("state[wan] = %q, want %q", state[stateKeyWAN], wanBackup)
	}
}

func TestUPSStateTransitions(t *testing.T) {
	tests := []struct {
		name        string
		batteryMode bool
		level       int
		wantUPS     string
		wantBattery string
	}{
		{"on mains, full", false, 100, upsOnline, batteryNormal},
		{"on mains, recharging after outage", false, 20, upsOnline, batteryLow},
		{"outage, battery still healthy", true, 80, upsOnBattery, batteryNormal},
		{"outage, at the low threshold", true, 30, upsOnBattery, batteryLow},
		{"outage, below the low threshold", true, 25, upsOnBattery, batteryLow},
		{"outage, at the critical threshold", true, 10, upsOnBattery, batteryCritical},
		{"outage, nearly empty", true, 3, upsOnBattery, batteryCritical},
	}
	c := NewClient("", nil, "", false)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var vbms vbmsTable
			vbms.IsBatteryMode = tc.batteryMode
			vbms.BattPool.BatteryLevel = tc.level

			state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{Model: "USWDA26", VBMS: &vbms}}})
			if err != nil {
				t.Fatalf("stateFromDevices: %v", err)
			}
			if state[stateKeyUPS] != tc.wantUPS {
				t.Errorf("state[ups] = %q, want %q", state[stateKeyUPS], tc.wantUPS)
			}
			if state[stateKeyUPSBattery] != tc.wantBattery {
				t.Errorf("state[ups.battery] = %q, want %q", state[stateKeyUPSBattery], tc.wantBattery)
			}
		})
	}
}

// An `ups: on-battery` automation must stay matched as the battery drains,
// so its onExit actions never fire in the middle of a power failure.
func TestUPSStaysOnBatteryAcrossBatteryLevels(t *testing.T) {
	c := NewClient("", nil, "", false)
	for _, level := range []int{100, 50, 30, 10, 1} {
		var vbms vbmsTable
		vbms.IsBatteryMode = true
		vbms.BattPool.BatteryLevel = level

		state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
		if err != nil {
			t.Fatalf("stateFromDevices: %v", err)
		}
		if state[stateKeyUPS] != upsOnBattery {
			t.Fatalf("at %d%%: state[ups] = %q, want %q", level, state[stateKeyUPS], upsOnBattery)
		}
	}
}

// ups.runtime is why battery percentage is a poor shutdown trigger: 30% at
// 300W and 30% at 900W are very different situations, and timeToRemain already
// accounts for the difference. These cases fix the mapping and the thresholds.
func TestUPSRuntimeLevels(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    string
	}{
		{"the capture: 17 minutes at 310W", 1043, upsRuntimeAmple},
		{"just above the short threshold", 601, upsRuntimeAmple},
		{"at the short threshold", 600, upsRuntimeShort},
		{"just above critical", 181, upsRuntimeShort},
		{"at the critical threshold", 180, upsRuntimeCritical},
		{"nearly out", 20, upsRuntimeCritical},
	}
	c := NewClient("", nil, "", false)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var vbms vbmsTable
			vbms.BattPool.TimeToRemain = tc.seconds

			state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
			if err != nil {
				t.Fatalf("stateFromDevices: %v", err)
			}
			if state[stateKeyUPSRuntime] != tc.want {
				t.Errorf("state[ups.runtime] = %q, want %q", state[stateKeyUPSRuntime], tc.want)
			}
		})
	}
}

// A UPS reporting no runtime estimate publishes no runtime key. The same
// battpool block uses -1 for "unknown" on battery_avr_time, and an absent
// field decodes to 0 — neither is a runtime anything should act on, and
// inventing "critical" from one would shut a cluster down on a parse gap.
func TestUnknownRuntimeOmitsTheKey(t *testing.T) {
	c := NewClient("", nil, "", false)
	for _, seconds := range []int{0, -1} {
		var vbms vbmsTable
		vbms.BattPool.TimeToRemain = seconds

		state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
		if err != nil {
			t.Fatalf("stateFromDevices: %v", err)
		}
		if got, present := state[stateKeyUPSRuntime]; present {
			t.Errorf("timeToRemain %d should publish no ups.runtime, got %q", seconds, got)
		}
		// ...and it must not take the rest of the UPS keys with it.
		if state[stateKeyUPS] != upsOnline {
			t.Errorf("state[ups] = %q, want %q", state[stateKeyUPS], upsOnline)
		}
	}
}

func TestUPSLoadLevels(t *testing.T) {
	watts := func(v float64) *float64 { return &v }
	tests := []struct {
		name           string
		output, budget *float64
		want           string
	}{
		{"the capture: 310W of 1000W", watts(310), watts(1000), upsLoadNormal},
		{"just under the threshold", watts(799), watts(1000), upsLoadNormal},
		{"at the threshold", watts(800), watts(1000), upsLoadHigh},
		{"overloaded", watts(1100), watts(1000), upsLoadHigh},
		// Absent is not zero: a missing output would otherwise report a fully
		// loaded UPS as idle, which is the wrong direction to be wrong in.
		{"no output reported", nil, watts(1000), ""},
		{"no budget reported", watts(310), nil, ""},
		{"a budget of zero is no fraction at all", watts(310), watts(0), ""},
	}
	c := NewClient("", nil, "", false)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var vbms vbmsTable
			vbms.BattPool.TotalPowerOutput, vbms.BattPool.TotalPowerBudget = tc.output, tc.budget

			state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
			if err != nil {
				t.Fatalf("stateFromDevices: %v", err)
			}
			got, present := state[stateKeyUPSLoad]
			if tc.want == "" && present {
				t.Fatalf("ups.load should be absent, got %q", got)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("state[ups.load] = %q, want %q", got, tc.want)
			}
		})
	}
}

// The same rule that keeps ups and ups.battery apart. An Automation matching
// ups.runtime: critical must not stop matching because the load moved, and one
// matching ups.load: high must not stop matching because the battery drained —
// each of those would fire onExit and scale workloads back up mid-outage.
func TestUPSKeysAreIndependentAxes(t *testing.T) {
	c := NewClient("", nil, "", false)
	watts := func(v float64) *float64 { return &v }

	for _, level := range []int{100, 40, 5} {
		for _, output := range []float64{100, 850} {
			var vbms vbmsTable
			vbms.IsBatteryMode = true
			vbms.BattPool.BatteryLevel = level
			vbms.BattPool.TimeToRemain = 120
			vbms.BattPool.TotalPowerOutput, vbms.BattPool.TotalPowerBudget = watts(output), watts(1000)

			state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
			if err != nil {
				t.Fatalf("stateFromDevices: %v", err)
			}
			if state[stateKeyUPSRuntime] != upsRuntimeCritical {
				t.Errorf("at %d%% and %.0fW: state[ups.runtime] = %q, want %q",
					level, output, state[stateKeyUPSRuntime], upsRuntimeCritical)
			}
			if state[stateKeyUPS] != upsOnBattery {
				t.Errorf("at %d%% and %.0fW: state[ups] = %q, want %q",
					level, output, state[stateKeyUPS], upsOnBattery)
			}
		}
	}
}

func TestCustomUPSRuntimeAndLoadThresholds(t *testing.T) {
	c := NewClient("", nil, "", false)
	c.ShortRuntimeSeconds, c.CriticalRuntimeSeconds = 3600, 1800
	c.HighLoadPercent = 25

	var vbms vbmsTable
	vbms.BattPool.TimeToRemain = 1043
	output, budget := 310.0, 1000.0
	vbms.BattPool.TotalPowerOutput, vbms.BattPool.TotalPowerBudget = &output, &budget

	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	// The same capture that reads ample/normal by default reads critical/high
	// against a site that needs an hour of runtime and runs a tight budget.
	if state[stateKeyUPSRuntime] != upsRuntimeCritical {
		t.Errorf("state[ups.runtime] = %q, want %q", state[stateKeyUPSRuntime], upsRuntimeCritical)
	}
	if state[stateKeyUPSLoad] != upsLoadHigh {
		t.Errorf("state[ups.load] = %q, want %q", state[stateKeyUPSLoad], upsLoadHigh)
	}
}

func TestCustomBatteryThresholds(t *testing.T) {
	c := NewClient("", nil, "", false)
	c.LowBatteryPercent = 60
	c.CriticalBatteryPercent = 40

	var vbms vbmsTable
	vbms.IsBatteryMode = true
	vbms.BattPool.BatteryLevel = 50

	state, err := c.stateFromDevices(context.Background(), deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyUPSBattery] != batteryLow {
		t.Fatalf("state[ups.battery] = %q, want %q at 50%% with low=60", state[stateKeyUPSBattery], batteryLow)
	}
}

func TestErrorsWhenNothingObservable(t *testing.T) {
	c := NewClient("", nil, "", false)
	if _, err := c.stateFromDevices(context.Background(), deviceStatResponse{}); err == nil {
		t.Fatal("expected error for empty device list")
	}
}

// TestFileAPIKeyPicksUpRotation is the credential-rotation contract: the key
// is read per request, so replacing the mounted Secret's contents changes what
// the next poll authenticates with — no restart, no stale credential quietly
// failing every poll.
func TestFileAPIKeyPicksUpRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "UNIFI_API_KEY")
	if err := os.WriteFile(path, []byte("first-key\n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One observation reads two endpoints; the key is resolved per request,
		// so record it once per observation to keep the assertion about
		// rotation rather than about how many calls a poll makes.
		if strings.HasSuffix(r.URL.Path, "stat/device") {
			seen = append(seen, r.Header.Get("X-API-KEY"))
		}
		_, _ = w.Write(captured(t, "stat-device-gateway.json"))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, FileAPIKey(path), "", false)

	if _, err := c.Observe(context.Background()); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// A rotated Secret reaches the container as new file contents.
	if err := os.WriteFile(path, []byte("second-key\n"), 0o600); err != nil {
		t.Fatalf("rotating key file: %v", err)
	}
	if _, err := c.Observe(context.Background()); err != nil {
		t.Fatalf("Observe after rotation: %v", err)
	}

	want := []string{"first-key", "second-key"}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("keys sent = %q, want %q", seen, want)
	}
}

func TestFileAPIKeyFailsLoudlyWhenUnreadable(t *testing.T) {
	if _, err := FileAPIKey(filepath.Join(t.TempDir(), "absent"))(); err == nil {
		t.Fatal("expected an error for a missing key file")
	}
	empty := filepath.Join(t.TempDir(), "UNIFI_API_KEY")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	if _, err := FileAPIKey(empty)(); err == nil {
		t.Fatal("expected an error for an empty key file")
	}
}
