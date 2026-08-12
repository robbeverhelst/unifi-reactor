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
	"time"
)

// Client talks to the UniFi Network application on a UniFi OS console using
// an API key (X-API-KEY works on both the Integration API and the legacy
// /proxy/network/api endpoints as of Network 10.5).
type Client struct {
	baseURL string
	apiKey  string
	site    string
	http    *http.Client
}

// NewClient creates a UniFi client. UniFi OS consoles serve a self-signed
// certificate by default, so insecureSkipVerify is commonly required for
// LAN access by IP.
func NewClient(baseURL, apiKey, site string, insecureSkipVerify bool) *Client {
	if site == "" {
		site = "default"
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		site:    site,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}, // #nosec G402 -- self-signed UniFi OS cert, opt-in
			},
		},
	}
}

// deviceStatResponse is the subset of /proxy/network/api/s/<site>/stat/device
// this provider reads. Field selection is based on the real captured response
// in testdata/unifi/api/stat-device-gateway.json — do not add fields that are
// not present there.
type deviceStatResponse struct {
	Data []struct {
		Model string   `json:"model"`
		Type  string   `json:"type"`
		WAN1  *wanPort `json:"wan1"`
		WAN2  *wanPort `json:"wan2"`
	} `json:"data"`
}

type wanPort struct {
	IsUplink bool   `json:"is_uplink"`
	Up       bool   `json:"up"`
	IfName   string `json:"ifname"`
	IP       string `json:"ip"`
}

// ObserveWANState returns the provider state map, currently just
// {"wan": "primary" | "backup"}, derived from which WAN port is the active
// uplink on the gateway device. WAN1 is primary, WAN2 is backup — matching
// the UniFi UI's labeling. Verified against the captured gateway record;
// the failover direction must be re-verified with a real failover capture.
func (c *Client) ObserveWANState(ctx context.Context) (map[string]string, error) {
	url := fmt.Sprintf("%s/proxy/network/api/s/%s/stat/device", c.baseURL, c.site)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", c.apiKey)
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
	return wanStateFromDevices(parsed)
}

func wanStateFromDevices(parsed deviceStatResponse) (map[string]string, error) {
	for _, d := range parsed.Data {
		if d.WAN1 == nil && d.WAN2 == nil {
			continue
		}
		switch {
		case d.WAN2 != nil && d.WAN2.IsUplink:
			return map[string]string{"wan": "backup"}, nil
		case d.WAN1 != nil && d.WAN1.IsUplink:
			return map[string]string{"wan": "primary"}, nil
		}
	}
	return nil, fmt.Errorf("no gateway with an active WAN uplink found in device list")
}
