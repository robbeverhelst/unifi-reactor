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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Default battery thresholds, as percentages of remaining charge.
const (
	DefaultLowBatteryPercent      = 30
	DefaultCriticalBatteryPercent = 10
)

// APIKey supplies the key sent with a request. It is resolved per request
// rather than held from startup so that rotating the credential does not
// require restarting the operator.
type APIKey func() (string, error)

// StaticAPIKey returns the same key for the lifetime of the process. Use it
// when the key arrives through the environment, where it cannot change.
func StaticAPIKey(key string) APIKey {
	return func() (string, error) { return key, nil }
}

// FileAPIKey reads the key from path on every use, which is what makes
// credential rotation automatic: the kubelet updates a mounted Secret in place
// (as long as it is not mounted through subPath), so the next poll after a
// rotation authenticates with the new key.
func FileAPIKey(path string) APIKey {
	return func() (string, error) {
		contents, err := os.ReadFile(path) // #nosec G304 -- operator-supplied credentials path
		if err != nil {
			return "", fmt.Errorf("reading unifi api key from %s: %w", path, err)
		}
		key := strings.TrimSpace(string(contents))
		if key == "" {
			return "", fmt.Errorf("unifi api key file %s is empty", path)
		}
		return key, nil
	}
}

// Client talks to the UniFi Network application on a UniFi OS console using
// an API key (X-API-KEY works on both the Integration API and the legacy
// /proxy/network/api endpoints as of Network 10.5).
type Client struct {
	baseURL string
	apiKey  APIKey
	site    string
	http    *http.Client

	// LowBatteryPercent and CriticalBatteryPercent bound the ups.battery
	// state key. Charge at or below the threshold reports that level.
	LowBatteryPercent      int
	CriticalBatteryPercent int
}

// NewClient creates a UniFi client. UniFi OS consoles serve a self-signed
// certificate by default, so insecureSkipVerify is commonly required for
// LAN access by IP.
func NewClient(baseURL string, apiKey APIKey, site string, insecureSkipVerify bool) *Client {
	if site == "" {
		site = "default"
	}
	if apiKey == nil {
		apiKey = StaticAPIKey("")
	}
	return &Client{
		baseURL:                baseURL,
		apiKey:                 apiKey,
		site:                   site,
		LowBatteryPercent:      DefaultLowBatteryPercent,
		CriticalBatteryPercent: DefaultCriticalBatteryPercent,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}, // #nosec G402 -- self-signed UniFi OS cert, opt-in
			},
		},
	}
}

// deviceStatResponse is the subset of /proxy/network/api/s/<site>/stat/device
// this provider reads. Field selection is based on the real captured
// responses in testdata/unifi/api/ — do not add fields that are not present
// there.
type deviceStatResponse struct {
	Data []deviceRecord `json:"data"`
}

type deviceRecord struct {
	Model string     `json:"model"`
	Type  string     `json:"type"`
	Name  string     `json:"name"`
	WAN1  *wanPort   `json:"wan1"`
	WAN2  *wanPort   `json:"wan2"`
	VBMS  *vbmsTable `json:"vbms_table"`
}

type wanPort struct {
	IsUplink bool   `json:"is_uplink"`
	Up       bool   `json:"up"`
	IfName   string `json:"ifname"`
	IP       string `json:"ip"`
}

// vbmsTable is the UniFi UPS battery-management block. Present on UniFi UPS
// devices (e.g. UPS 2U, reported as a switch-type device).
type vbmsTable struct {
	IsBatteryMode bool `json:"is_battery_mode"`
	BattPool      struct {
		BatteryLevel int  `json:"batteryLevel"`
		IsCharging   bool `json:"ischarging"`
		TimeToRemain int  `json:"timeToRemain"`
		AvailableCnt int  `json:"batt_available_cnt"`
	} `json:"battpool"`
}

// Observe returns the normalized UniFi state map. Keys are only present when
// the corresponding hardware is visible to the controller:
//
//	wan         primary | backup      (which uplink the gateway is using)
//	ups         online  | on-battery  (whether the UPS is running on mains)
//	ups.battery normal  | low | critical
//
// ups and ups.battery are deliberately independent: a `when: {ups: on-battery}`
// automation must stay matched for the whole outage, including as the battery
// drains, instead of flipping out of its matching state (which would run its
// onExit actions in the middle of a power failure).
func (c *Client) Observe(ctx context.Context) (map[string]string, error) {
	url := fmt.Sprintf("%s/proxy/network/api/s/%s/stat/device", c.baseURL, c.site)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	apiKey, err := c.apiKey()
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("polling unifi device state: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("polling unifi device state: unexpected status %d", resp.StatusCode)
	}

	var parsed deviceStatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding unifi device state: %w", err)
	}
	return c.stateFromDevices(parsed)
}

// stateFromDevices derives the state map from a device list. The first
// gateway with an active uplink and the first UPS reporting battery data win;
// multiple gateways or UPS devices per site are out of scope for v1alpha1.
func (c *Client) stateFromDevices(parsed deviceStatResponse) (map[string]string, error) {
	state := map[string]string{}

	for _, d := range parsed.Data {
		if _, seen := state[stateKeyWAN]; !seen {
			switch {
			case d.WAN2 != nil && d.WAN2.IsUplink:
				state[stateKeyWAN] = wanBackup
			case d.WAN1 != nil && d.WAN1.IsUplink:
				state[stateKeyWAN] = wanPrimary
			}
		}
		if _, seen := state[stateKeyUPS]; !seen && d.VBMS != nil {
			state[stateKeyUPS] = upsOnline
			if d.VBMS.IsBatteryMode {
				state[stateKeyUPS] = upsOnBattery
			}
			state[stateKeyUPSBattery] = c.batteryLevel(d.VBMS.BattPool.BatteryLevel)
		}
	}

	if len(state) == 0 {
		return nil, fmt.Errorf("no gateway with an active WAN uplink and no UPS found in device list")
	}
	return state, nil
}

func (c *Client) batteryLevel(percent int) string {
	low, critical := c.LowBatteryPercent, c.CriticalBatteryPercent
	if low <= 0 {
		low = DefaultLowBatteryPercent
	}
	if critical <= 0 {
		critical = DefaultCriticalBatteryPercent
	}
	switch {
	case percent <= critical:
		return batteryCritical
	case percent <= low:
		return batteryLow
	default:
		return batteryNormal
	}
}
