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

	stateKeyWAN         = "wan"
	stateKeyWANQuality  = "wan.quality"
	stateKeyISP         = "isp"
	stateKeyInternet    = "internet"
	stateKeyUPS         = "ups"
	stateKeyUPSBattery  = "ups.battery"
	stateKeyUPSRuntime  = "ups.runtime"
	stateKeyUPSLoad     = "ups.load"
	stateKeyDevices     = "devices"
	stateKeyFirmware    = "firmware"
	stateKeyTemperature = "temperature"
	stateKeyWiFi        = "wifi"
	stateKeyPoE         = "poe"

	// stateKeyDevicePrefix is what a per-device key is published under:
	// device.<slugified name>. It is the first key on this list whose NAME is
	// open rather than whose values are, and that is why publishing them is
	// opt-in — see the note on StateVocabulary below.
	stateKeyDevicePrefix = "device."

	wanPrimary = "primary"
	wanBackup  = "backup"

	// internet answers a question wan structurally cannot: wan says which
	// uplink is selected, and stays primary while the link is up, the uplink is
	// unchanged, and there is no internet. These values come from the console's
	// own www subsystem, which is its judgement about reachability rather than
	// about link state.
	internetOK       = "ok"
	internetDegraded = "degraded"
	internetDown     = "down"

	// wan.quality buckets a measurement rather than reporting a switch
	// position. The numbers behind it — availability and average latency over
	// the console's uptime window — are continuous, and a continuous value can
	// be neither matched by spec.when (which compares strings) nor exported as
	// a metric label (which needs a closed set). Two named levels are what
	// survives both constraints.
	wanQualityGood     = "good"
	wanQualityDegraded = "degraded"

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

	// The stat/health subsystems this provider reads. www is the console's own
	// internet-reachability subsystem; wan carries the per-uplink uptime_stats.
	healthSubsystemWWW  = "www"
	healthSubsystemWAN  = "wan"
	healthSubsystemWLAN = "wlan"

	// The per-subsystem status values. ok, warning and unknown are all present
	// in the committed capture — on wan, wlan and vpn respectively — so the
	// vocabulary itself is observed. error is documented by UniFi and has never
	// been captured, and no capture has ever caught the www subsystem saying
	// anything but ok. Which value www takes when the internet is actually
	// unreachable is therefore inferred; see testdata/unifi/README.md.
	healthStatusOK      = "ok"
	healthStatusWarning = "warning"
	healthStatusError   = "error"

	upsOnline    = "online"
	upsOnBattery = "on-battery"

	batteryNormal   = "normal"
	batteryLow      = "low"
	batteryCritical = "critical"

	// ups.runtime is how long the UPS says it can carry its current load, and
	// it is a better shutdown trigger than charge alone: 30% at 300W and 30% at
	// 900W are very different situations, and timeToRemain already accounts for
	// the difference. It is a separate key from ups.battery for the same reason
	// ups.battery is separate from ups — an Automation matching one must not
	// stop matching because the other moved.
	upsRuntimeAmple    = "ample"
	upsRuntimeShort    = "short"
	upsRuntimeCritical = "critical"

	// ups.load is the output drawn as a fraction of the budget. Like
	// wan.quality it is a bucketed measurement rather than a switch position,
	// and for the same reasons: a fraction cannot be matched by spec.when and
	// could never be a metric label.
	upsLoadNormal = "normal"
	upsLoadHigh   = "high"

	// device.<name> is whether the console is in contact with one adopted
	// device. It is a switch position, not a measurement — the console either
	// has a heartbeat from a device or it does not.
	deviceOnline  = "online"
	deviceOffline = "offline"

	// devices is the fleet in one value, and it is the key most installs should
	// match on: it says nothing about which device is missing, and it costs one
	// series regardless of how many devices are adopted.
	devicesAllOnline = "all-online"
	devicesDegraded  = "degraded"

	// firmware is whether the console has an update waiting for anything in the
	// fleet. Observing is in scope and applying is not: Reactor never triggers
	// an upgrade, so this key turns "I should check for UniFi updates sometime"
	// into something that can page, notify or open a ticket, and stops there.
	firmwareCurrent          = "current"
	firmwareUpdatesAvailable = "updates-available"

	// temperature buckets a measurement, exactly as wan.quality and ups.load
	// do, and for the same two reasons: a number cannot be matched by spec.when,
	// and it could never be a metric label. The hottest device in the fleet
	// decides, against a configurable threshold; the readings behind it are a
	// V(1) log line.
	temperatureNormal = "normal"
	temperatureHigh   = "high"

	// wifi is the WiFi subsystem as a whole, which is a different question from
	// any single AP being down: error is every adopted AP gone, warning is some
	// of them. It is derived from the console's AP counts rather than from its
	// own status wording — see wifi.go for why, which #9 asks to be documented.
	wifiOK      = "ok"
	wifiWarning = "warning"
	wifiError   = "error"

	// poe buckets a budget, which is a measurement like temperature and
	// ups.load. insufficient means the headroom is gone — the worst switch is
	// delivering at or above the configured share of its budget — rather than
	// that a port has already been denied power. By the time the console
	// refuses a port, the camera is already off.
	poeOK           = "ok"
	poeInsufficient = "insufficient"
)

// The comparisons this provider makes between two independent signals for the
// same fact, named so a disagreement can be counted without the values that
// disagreed — which come from the outside world — becoming a metric label.
const (
	signalWANUplinkDisagrees  = "wan-uplink-disagrees"
	signalWANUplinkUnclaimed  = "wan-uplink-unclaimed"
	signalWANUplinkAmbiguous  = "wan-uplink-ambiguous"
	signalWANNotOnline        = "wan-not-online"
	signalWANMovedWithoutISP  = "wan-moved-without-isp"
	signalISPMovedWithoutWAN  = "isp-moved-without-wan"
	signalWANHealthDisagrees  = "wan-health-disagrees"
	signalDeviceNameShared    = "device-name-shared"
	signalWiFiStatusDisagrees = "wifi-status-disagrees"
)

// StateVocabulary is the closed value set of every key this provider publishes
// one for.
//
// It exists so that an exported gauge can report 0 for the values a key does
// not currently hold rather than leaving a stale series sitting at 1 — which
// needs the full list of values, and this file is already exactly that list.
// Nothing outside this package may spell a key or a value, so the list is
// handed out as opaque data.
//
// isp is deliberately absent. Its values are carrier names derived from
// whatever public address the gateway currently holds, so the set is open by
// construction — the one exception to the closed-vocabulary rule argued in
// docs/adding-a-provider.md — and an open set is the one thing that must never
// become a metric label. Its transitions are still counted, and its current
// value is still in every referencing Automation's status.
//
// wan.quality is here, and it is here because it was bucketed. The console
// reports the availability and average latency behind it as continuous
// numbers, and neither could have appeared in this map: one series per
// distinct latency reading is the same cardinality failure isp would have
// been. Bucketing into two named levels is what makes it a state key at all —
// a number is not something spec.when can match either.
//
// device.<name> is absent for a third reason, and it is the reason this map is
// keyed at all rather than being a list of values. Its VALUES are closed —
// online and offline, two of them — but its key NAME is derived from a device
// name, so the set of keys is open and only known at runtime. This map is
// returned once at startup and cannot enumerate them, and enumerating them
// would be the wrong thing to want: an install with forty adopted devices would
// silently multiply its series count. So the per-device keys are opt-in
// (Client.PerDeviceKeys), they are never labelled by value, and the aggregate
// devices key — one series, whatever the fleet size — is what ships on by
// default. What any single device currently is stays in the Automation's status
// and in a Kubernetes Event, exactly as isp's value does.
func StateVocabulary() map[string][]string {
	return map[string][]string{
		stateKeyWAN:         {wanPrimary, wanBackup},
		stateKeyWANQuality:  {wanQualityGood, wanQualityDegraded},
		stateKeyInternet:    {internetOK, internetDegraded, internetDown},
		stateKeyUPS:         {upsOnline, upsOnBattery},
		stateKeyUPSBattery:  {batteryNormal, batteryLow, batteryCritical},
		stateKeyUPSRuntime:  {upsRuntimeAmple, upsRuntimeShort, upsRuntimeCritical},
		stateKeyUPSLoad:     {upsLoadNormal, upsLoadHigh},
		stateKeyDevices:     {devicesAllOnline, devicesDegraded},
		stateKeyFirmware:    {firmwareCurrent, firmwareUpdatesAvailable},
		stateKeyTemperature: {temperatureNormal, temperatureHigh},
		stateKeyWiFi:        {wifiOK, wifiWarning, wifiError},
		stateKeyPoE:         {poeOK, poeInsufficient},
	}
}
