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
	"slices"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// Default wan.quality thresholds. Both are starting points rather than
// findings: one link's numbers have ever been captured (100% available, 16 ms
// average), so what a genuinely bad link reads is unknown and an operator is
// expected to tune these against their own uplink.
const (
	// DefaultMinAvailabilityPercent is the availability at or above which the
	// live uplink counts as good. The console reports availability over its
	// uptime window — time_period, 86400 seconds in the capture — so 99% means
	// "no more than about a quarter of an hour of loss in the last day", and a
	// long outage keeps the key degraded for the rest of that window.
	DefaultMinAvailabilityPercent = 99.0
	// DefaultMaxLatencyMs is the average latency above which the live uplink
	// counts as degraded. It is averaged over the same window, so it describes
	// a link that is consistently slow rather than one that spiked.
	DefaultMaxLatencyMs = 150.0
)

// healthResponse is the subset of /proxy/network/api/s/<site>/stat/health this
// provider reads. Field selection is based on the real captured response in
// testdata/unifi/api/stat-health.json — do not add fields that are not present
// there.
type healthResponse struct {
	Data []healthSubsystem `json:"data"`
}

type healthSubsystem struct {
	Subsystem string `json:"subsystem"`
	Status    string `json:"status"`
	// UptimeStats appears on the wan subsystem, keyed by uplink (WAN, WAN2,
	// WAN3) exactly as last_wan_status is.
	UptimeStats map[string]uplinkHealth `json:"uptime_stats"`
}

// uplinkHealth is one uplink's entry in the wan subsystem's uptime_stats.
//
// Every number here is a pointer, and that is the load-bearing decision in
// this file. The console omits these fields rather than reporting zero: in the
// capture the live uplink carries availability, latency_average, time_period
// and uptime, while the two dead ones carry only downtime and their monitors.
// Decoding an absent field as 0 would read "this uplink reports no data" as
// "this uplink is 0% available" — the difference between holding state and
// shedding a cluster's load on a truncated response.
type uplinkHealth struct {
	Availability   *float64 `json:"availability"`
	LatencyAverage *float64 `json:"latency_average"`
	// TimePeriod is the window availability and latency are averaged over,
	// in seconds. Nothing is derived from it; it is logged, because it is what
	// tells an operator how to read the thresholds they are setting.
	TimePeriod *int `json:"time_period"`
	// Uptime and Downtime are seconds within that window. Uptime is the only
	// field the capture shows exclusively on the live uplink, which is what
	// makes it usable as a third opinion on which uplink is carrying traffic.
	Uptime   *int `json:"uptime"`
	Downtime *int `json:"downtime"`
	// Monitors are the console's uptime probes and AlertingMonitors the ones
	// its failover logic watches. Both are lists of the same shape, and both
	// carry an availability on every uplink in the capture — including the
	// dead ones, where the uplink-level fields are missing entirely.
	Monitors         []healthMonitor `json:"monitors"`
	AlertingMonitors []healthMonitor `json:"alerting_monitors"`
}

type healthMonitor struct {
	Availability   *float64 `json:"availability"`
	LatencyAverage *float64 `json:"latency_average"`
	Target         string   `json:"target"`
	Type           string   `json:"type"`
}

// mergeHealth adds the keys derived from stat/health to a state map already
// holding the ones derived from stat/device.
//
// It takes wan rather than deriving its own, because "the quality of the live
// uplink" needs to know which uplink that is, and the answer to that question
// lives in the device response. When wan says nothing, neither does
// wan.quality: guessing an uplink would publish another uplink's numbers under
// this one's name.
func (c *Client) mergeHealth(ctx context.Context, state map[string]string, health healthResponse, wan string) {
	log := logf.FromContext(ctx).WithName("unifi-health")

	for _, subsystem := range health.Data {
		switch subsystem.Subsystem {
		case healthSubsystemWWW:
			if internet := internetFrom(subsystem.Status); internet != "" {
				state[stateKeyInternet] = internet
				continue
			}
			// An unrecognised status is not translated into a value: the point
			// of this key is to be believed, and a firmware that renamed
			// "error" must not have it read as "ok". Dropping the key holds
			// whatever an Automation last matched, which is the safe side.
			log.Info("The www subsystem reports a status this provider does not recognise; "+
				"internet will not be published. Please report it — the set of statuses is "+
				"what this key is derived from",
				"status", subsystem.Status)
		case healthSubsystemWAN:
			c.crossCheckUplinkHealth(ctx, wan, subsystem.UptimeStats)
			if wan == "" {
				continue
			}
			entry, present := subsystem.UptimeStats[uptimeStatsKey(wan)]
			if !present {
				log.V(1).Info("The health response carries no uptime stats for the live uplink; "+
					"wan.quality will not be published", "wan", wan)
				continue
			}
			if quality := c.wanQualityFrom(ctx, entry); quality != "" {
				state[stateKeyWANQuality] = quality
			}
		}
	}
}

// uptimeStatsKey maps a derived uplink onto the key stat/health files it
// under. Both captured endpoints agree on WAN and WAN2.
func uptimeStatsKey(wan string) string {
	if wan == wanBackup {
		return wanStatusKeyBackup
	}
	return wanStatusKeyPrimary
}

// internetFrom translates the www subsystem's status. An empty result means
// the status said nothing this provider is willing to act on — including
// "unknown", which the capture shows the vpn subsystem using for "nothing
// configured here".
func internetFrom(status string) string {
	switch status {
	case healthStatusOK:
		return internetOK
	case healthStatusWarning:
		return internetDegraded
	case healthStatusError:
		return internetDown
	default:
		return ""
	}
}

// wanQualityFrom buckets the live uplink's measured availability and latency
// into the two levels the key publishes. An empty result means the entry
// carried no availability anywhere, which is a truncated or unfamiliar
// response rather than a bad link, and is reported by omitting the key.
func (c *Client) wanQualityFrom(ctx context.Context, entry uplinkHealth) string {
	availability, known := entry.availability()
	if !known {
		logf.FromContext(ctx).WithName("unifi-health").V(1).Info(
			"The live uplink's health entry reports no availability; wan.quality will not be published")
		return ""
	}
	latency, latencyKnown := entry.latency()

	minAvailability, maxLatency := c.MinAvailabilityPercent, c.MaxLatencyMs
	if minAvailability <= 0 {
		minAvailability = DefaultMinAvailabilityPercent
	}
	if maxLatency <= 0 {
		maxLatency = DefaultMaxLatencyMs
	}

	quality := wanQualityGood
	switch {
	case availability < minAvailability:
		quality = wanQualityDegraded
	case latencyKnown && latency > maxLatency:
		quality = wanQualityDegraded
	}
	logf.FromContext(ctx).WithName("unifi-health").V(1).Info("wan quality",
		"quality", quality, "availability", availability, "latencyMs", latency,
		"latencyKnown", latencyKnown, "windowSeconds", derefInt(entry.TimePeriod))
	return quality
}

// availability is the uplink's availability as a percentage.
//
// The uplink-level field is preferred and is the only one the live uplink
// carries in the capture. Where it is absent the per-monitor availabilities
// are averaged instead: those are present on every uplink in the capture,
// including the dead ones, so a link bad enough to lose its uplink-level
// summary still produces a number rather than disappearing.
func (u uplinkHealth) availability() (float64, bool) {
	if u.Availability != nil {
		return *u.Availability, true
	}
	return meanOf(u.Monitors, u.AlertingMonitors, func(m healthMonitor) *float64 { return m.Availability })
}

// latency is the uplink's average round-trip time in milliseconds, by the same
// rule. It is genuinely optional: the capture omits it on every uplink that is
// not carrying traffic, and a link with no latency reading is judged on
// availability alone rather than being called degraded for the omission.
func (u uplinkHealth) latency() (float64, bool) {
	if u.LatencyAverage != nil {
		return *u.LatencyAverage, true
	}
	return meanOf(u.Monitors, u.AlertingMonitors, func(m healthMonitor) *float64 { return m.LatencyAverage })
}

// meanOf averages one field across every monitor that reports it, over both
// monitor lists. The two lists probe different targets for different reasons —
// uptime graphs and failover decisions — but both answer "could this uplink
// reach the outside world", which is the only question this key asks.
func meanOf(monitors, alerting []healthMonitor, field func(healthMonitor) *float64) (float64, bool) {
	var sum float64
	var count int
	for _, list := range [][]healthMonitor{monitors, alerting} {
		for _, monitor := range list {
			if value := field(monitor); value != nil {
				sum += *value
				count++
			}
		}
	}
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

// crossCheckUplinkHealth reports when stat/health thinks a different uplink has
// been carrying traffic than the one stat/device's is_uplink named.
//
// This is a genuinely independent third opinion on the question issue #34 is
// open about, and the first one that comes from a different endpoint: uptime is
// accumulated by the console over hours, so an uplink with uptime has demonstrably
// passed traffic, where is_uplink and uplink.name are both statements about
// configuration that a failover may or may not move.
//
// It is deliberately narrow. Nothing fires unless some uplink reports uptime
// and the one believed live is not among them — the shape of "the mapping is
// pointing at the wrong port", not the shape of "this link is having a bad
// day", which wan.quality is the key for.
func (c *Client) crossCheckUplinkHealth(ctx context.Context, wan string, stats map[string]uplinkHealth) {
	if wan == "" || len(stats) == 0 {
		return
	}
	believed := uptimeStatsKey(wan)
	var withUptime []string
	for key, entry := range stats {
		if entry.Uptime != nil && *entry.Uptime > 0 {
			withUptime = append(withUptime, key)
		}
	}
	if len(withUptime) == 0 || slices.Contains(withUptime, believed) {
		return
	}
	slices.Sort(withUptime)
	metrics.SignalsDisagreed(ProviderName, signalWANHealthDisagrees)
	logf.FromContext(ctx).WithName("unifi-wan").Info(
		"The health endpoint accumulated uptime on an uplink other than the one wan names; "+
			"uptime is passed traffic rather than configuration, so this is the strongest "+
			"evidence available that the wan mapping is pointing at the wrong port",
		"wan", wan, "uptimeStatsKey", believed, "uplinksWithUptime", strings.Join(withUptime, ","))
}

func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
