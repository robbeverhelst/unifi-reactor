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

// The vocabulary the UniFi provider publishes into normalized state. These are
// the exact strings users write in an Automation's spec.when.state.
const (
	// ProviderName is how Automations refer to this provider.
	ProviderName = "unifi"

	stateKeyWAN        = "wan"
	stateKeyISP        = "isp"
	stateKeyUPS        = "ups"
	stateKeyUPSBattery = "ups.battery"

	wanPrimary = "primary"
	wanBackup  = "backup"

	// ispUnknown is published when a gateway is visible but names no ISP —
	// which is a real observation, not missing hardware, and is expected to
	// happen for a moment during a failover while the console re-derives the
	// geolocation of a public address it has just been handed.
	ispUnknown = "unknown"

	// wanStatusOnline is the only value ever seen in the gateway's
	// last_wan_status map, captured with the primary uplink live. What a failed
	// uplink reports there is unknown, so nothing is derived from this field:
	// it is only used to notice that the uplink believed to be live does not
	// describe itself as online. See the failover capture runbook in
	// testdata/unifi/README.md.
	wanStatusOnline = "online"

	// The keys last_wan_status and the health subsystem's uptime_stats use for
	// each uplink. Both captures agree on WAN for the first and WAN2 for the
	// second, and neither has been observed while the second was live.
	wanStatusKeyPrimary = "WAN"
	wanStatusKeyBackup  = "WAN2"

	upsOnline    = "online"
	upsOnBattery = "on-battery"

	batteryNormal   = "normal"
	batteryLow      = "low"
	batteryCritical = "critical"
)
