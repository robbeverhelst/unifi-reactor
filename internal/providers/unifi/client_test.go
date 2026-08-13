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

func serve(t *testing.T, payload []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-KEY"); got != "test-key" {
			t.Errorf("expected X-API-KEY header, got %q", got)
		}
		if got := r.URL.Path; got != "/proxy/network/api/s/default/stat/device" {
			t.Errorf("unexpected path %q", got)
		}
		_, _ = w.Write(payload)
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
	// The captures were taken with WAN1 active and the UPS on mains at 100%.
	for key, want := range map[string]string{
		stateKeyWAN:        wanPrimary,
		stateKeyUPS:        upsOnline,
		stateKeyUPSBattery: batteryNormal,
	} {
		if state[key] != want {
			t.Errorf("state[%q] = %q, want %q", key, state[key], want)
		}
	}
}

func TestObserveWithoutUPSOmitsUPSKeys(t *testing.T) {
	c := serve(t, captured(t, "stat-device-gateway.json"))

	state, err := c.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state[stateKeyWAN] != wanPrimary {
		t.Errorf("state[wan] = %q, want %q", state[stateKeyWAN], wanPrimary)
	}
	if _, present := state[stateKeyUPS]; present {
		t.Error("ups key should be absent when no UPS is visible to the controller")
	}
}

func TestWANBackupWhenWAN2IsUplink(t *testing.T) {
	c := NewClient("", nil, "", false)
	state, err := c.stateFromDevices(deviceStatResponse{Data: []deviceRecord{{
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

			state, err := c.stateFromDevices(deviceStatResponse{Data: []deviceRecord{{Model: "USWDA26", VBMS: &vbms}}})
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

		state, err := c.stateFromDevices(deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
		if err != nil {
			t.Fatalf("stateFromDevices: %v", err)
		}
		if state[stateKeyUPS] != upsOnBattery {
			t.Fatalf("at %d%%: state[ups] = %q, want %q", level, state[stateKeyUPS], upsOnBattery)
		}
	}
}

func TestCustomBatteryThresholds(t *testing.T) {
	c := NewClient("", nil, "", false)
	c.LowBatteryPercent = 60
	c.CriticalBatteryPercent = 40

	var vbms vbmsTable
	vbms.IsBatteryMode = true
	vbms.BattPool.BatteryLevel = 50

	state, err := c.stateFromDevices(deviceStatResponse{Data: []deviceRecord{{VBMS: &vbms}}})
	if err != nil {
		t.Fatalf("stateFromDevices: %v", err)
	}
	if state[stateKeyUPSBattery] != batteryLow {
		t.Fatalf("state[ups.battery] = %q, want %q at 50%% with low=60", state[stateKeyUPSBattery], batteryLow)
	}
}

func TestErrorsWhenNothingObservable(t *testing.T) {
	c := NewClient("", nil, "", false)
	if _, err := c.stateFromDevices(deviceStatResponse{}); err == nil {
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
		seen = append(seen, r.Header.Get("X-API-KEY"))
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
