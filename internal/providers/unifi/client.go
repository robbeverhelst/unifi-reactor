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
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// Default battery thresholds, as percentages of remaining charge.
const (
	DefaultLowBatteryPercent      = 30
	DefaultCriticalBatteryPercent = 10
)

// Default UPS runtime and load thresholds.
//
// The runtime pair is chosen against the debounce it ships with rather than in
// isolation. ups.runtime settles over 2 samples — 60 seconds at the default
// poll — so a critical threshold of 180 seconds leaves two minutes of headroom
// between Reactor believing the reading and the UPS running out. Moving one of
// these without the other is how that headroom disappears.
const (
	// DefaultShortRuntimeSeconds is remaining runtime at or below which
	// ups.runtime reports "short": start winding down.
	DefaultShortRuntimeSeconds = 600
	// DefaultCriticalRuntimeSeconds is where it reports "critical": stop.
	DefaultCriticalRuntimeSeconds = 180
	// DefaultHighLoadPercent is the draw, as a percentage of the UPS's power
	// budget, at or above which ups.load reports "high". The capture was taken
	// at 310W of a 1000W budget.
	DefaultHighLoadPercent = 80.0
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

	// ShortRuntimeSeconds and CriticalRuntimeSeconds bound the ups.runtime
	// state key, and HighLoadPercent bounds ups.load.
	ShortRuntimeSeconds    int
	CriticalRuntimeSeconds int
	HighLoadPercent        float64

	// MinAvailabilityPercent and MaxLatencyMs bound the wan.quality state key.
	// The live uplink is good while it is at least this available and no
	// slower than this on average, over the console's own uptime window.
	MinAvailabilityPercent float64
	MaxLatencyMs           float64

	// PerDeviceKeys publishes a device.<name> key per adopted device alongside
	// the aggregate devices key. It defaults off because it is the one setting
	// here that changes how many things Reactor publishes rather than what they
	// mean: a fleet of forty devices is forty more state keys, forty more
	// transition series, and forty more keys an Automation could hold state for.
	// The aggregate answers "is anything down" on one series.
	PerDeviceKeys bool

	// mu guards previous only.
	mu sync.Mutex
	// previous remembers the last WAN signals so that a change in one can be
	// checked against a change in the other. Nothing is ever derived from it:
	// it exists so a disagreement between two independent signals is reported
	// instead of one of them being silently trusted. See crossCheckOverTime.
	previous struct{ wan, isp string }
}

// NewClient creates a UniFi client. UniFi OS consoles serve a self-signed
// certificate by default, so insecureSkipVerify is commonly required for
// LAN access by IP.
func NewClient(baseURL string, apiKey APIKey, site string, insecureSkipVerify bool) *Client {
	if site == "" {
		site = defaultSite
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
		ShortRuntimeSeconds:    DefaultShortRuntimeSeconds,
		CriticalRuntimeSeconds: DefaultCriticalRuntimeSeconds,
		HighLoadPercent:        DefaultHighLoadPercent,
		MinAvailabilityPercent: DefaultMinAvailabilityPercent,
		MaxLatencyMs:           DefaultMaxLatencyMs,
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
	Model  string     `json:"model"`
	Type   string     `json:"type"`
	Name   string     `json:"name"`
	WAN1   *wanPort   `json:"wan1"`
	WAN2   *wanPort   `json:"wan2"`
	Uplink *uplinkRef `json:"uplink"`
	// ISP is the capture's name for active_geo_info.WAN.isp_name — the carrier
	// the console geolocated the current public address to.
	ISP string `json:"isp"`
	// LastWANStatus is keyed by uplink (WAN, WAN2). It is deliberately not
	// map[string]string: a value that turned out not to be a string on some
	// other firmware would fail the decode and take the whole observation —
	// including the UPS keys — down with it.
	LastWANStatus map[string]any `json:"last_wan_status"`
	VBMS          *vbmsTable     `json:"vbms_table"`

	// The fleet-wide fields, embedded so each group lives in the file holding
	// the parser that reads it. Embedded struct fields decode from the same
	// flat object, so this is a grouping for readers and nothing more.
	deviceHealthFields
}

type wanPort struct {
	IsUplink bool   `json:"is_uplink"`
	Up       bool   `json:"up"`
	IfName   string `json:"ifname"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
}

// uplinkRef is the gateway's own statement of which interface it is uplinked
// through. It is the second, independent answer to the question wan1/wan2
// is_uplink answers.
type uplinkRef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// named reports whether this port is the interface the given name refers to.
// Captures carry the same value in ifname and name; matching either costs
// nothing and does not assume which one a future firmware keeps.
func (p *wanPort) named(name string) bool {
	if p == nil || name == "" {
		return false
	}
	return p.IfName == name || p.Name == name
}

// vbmsTable is the UniFi UPS battery-management block. Present on UniFi UPS
// devices (e.g. UPS 2U, reported as a switch-type device).
type vbmsTable struct {
	IsBatteryMode bool `json:"is_battery_mode"`
	BattPool      struct {
		BatteryLevel int  `json:"batteryLevel"`
		IsCharging   bool `json:"ischarging"`
		AvailableCnt int  `json:"batt_available_cnt"`
		// TimeToRemain is seconds of runtime at the current load — inferred
		// from one observation (1043 on a UPS 2U drawing 310W), never yet
		// checked against a real outage. Zero and negative both mean "no
		// estimate": the same block uses -1 that way for battery_avr_time, and
		// an absent field decodes to 0, which is not a runtime anyone should
		// act on either.
		TimeToRemain int `json:"timeToRemain"`
		// TotalPowerOutput and TotalPowerBudget are watts drawn and watts
		// available. Pointers for the same reason the health numbers are: an
		// absent output would decode as 0W and report a fully loaded UPS as
		// idle, which is the wrong direction to be wrong in.
		TotalPowerOutput *float64 `json:"device_total_power_output"`
		TotalPowerBudget *float64 `json:"device_total_power_budget"`
	} `json:"battpool"`
}

// Observe returns the normalized UniFi state map. Keys are only present when
// the corresponding hardware is visible to the controller:
//
//	wan          primary | backup      (which uplink the gateway is using)
//	wan.quality  good    | degraded    (how well that uplink is performing)
//	isp          a slug, or unknown    (the carrier behind the live uplink)
//	internet     ok | degraded | down  (whether the outside world is reachable)
//	ups          online  | on-battery  (whether the UPS is running on mains)
//	ups.battery  normal  | low | critical
//	ups.runtime  ample   | short | critical
//	ups.load     normal  | high
//	devices      all-online | degraded   (the adopted fleet in one value)
//	device.<name>  online | offline      (opt-in; see Client.PerDeviceKeys)
//
// ups and ups.battery are deliberately independent: a `when: {ups: on-battery}`
// automation must stay matched for the whole outage, including as the battery
// drains, instead of flipping out of its matching state (which would run its
// onExit actions in the middle of a power failure). wan and internet are
// independent for the same kind of reason and a different one: they answer
// different questions, and the case where the link is up, the uplink is
// unchanged and there is no internet is precisely the one wan cannot express.
//
// Two endpoints are read, and either may fail without taking the other's keys
// with it. Only observing nothing at all is an error.
func (c *Client) Observe(ctx context.Context) (map[string]string, error) {
	log := logf.FromContext(ctx).WithName("unifi-observe")
	state := map[string]string{}

	var devices deviceStatResponse
	deviceErr := c.get(ctx, "stat/device", &devices)
	if deviceErr == nil {
		var fromDevices map[string]string
		fromDevices, deviceErr = c.stateFromDevices(ctx, devices)
		maps.Copy(state, fromDevices)
	}

	// The health endpoint is a second call rather than a second parse of the
	// first, so it fails separately. Losing it costs internet and wan.quality,
	// which then vanish from the observation and are held as last known state
	// by anything matching them — the same degradation a UPS dropping off the
	// console produces, and the same reason it must not be an error here.
	var health healthResponse
	healthErr := c.get(ctx, "stat/health", &health)
	if healthErr == nil {
		c.mergeHealth(ctx, state, health, state[stateKeyWAN])
	}

	if len(state) == 0 {
		return nil, errors.Join(deviceErr, healthErr)
	}
	if deviceErr != nil {
		log.Error(deviceErr, "The device endpoint failed; the keys derived from it are unavailable this poll")
	}
	if healthErr != nil {
		log.Error(healthErr, "The health endpoint failed; internet and wan.quality are unavailable this poll")
	}
	return state, nil
}

// get fetches one site-scoped endpoint and decodes it. The API key is resolved
// per request, so a rotated credential takes effect on the next call.
func (c *Client) get(ctx context.Context, endpoint string, out any) error {
	url := fmt.Sprintf("%s/proxy/network/api/s/%s/%s", c.baseURL, c.site, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	apiKey, err := c.apiKey()
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("polling unifi %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("polling unifi %s: unexpected status %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding unifi %s: %w", endpoint, err)
	}
	return nil
}

// stateFromDevices derives the state map from a device list. The first record
// carrying WAN ports and the first UPS reporting battery data win; multiple
// gateways or UPS devices per site are out of scope for v1alpha1.
//
// The two halves are independent on purpose: a site with no UPS still reports
// wan, and a site whose gateway reports nothing recognisable still reports the
// UPS keys. Only observing nothing at all is an error.
func (c *Client) stateFromDevices(ctx context.Context, parsed deviceStatResponse) (map[string]string, error) {
	state := map[string]string{}
	gatewaySeen := false
	fleet := newDeviceTally()

	for _, d := range parsed.Data {
		// The fleet keys are about devices the console manages, so an unadopted
		// or pending one is skipped here while the gateway and UPS halves below
		// are unchanged: they are about specific hardware being present, which
		// is a different question from whether it has been adopted.
		if d.adopted() {
			fleet.observe(ctx, d)
		} else {
			logf.FromContext(ctx).WithName("unifi-devices").V(1).Info(
				"Skipping a device that is not adopted", "model", d.Model, "type", d.Type)
		}
		if !gatewaySeen && (d.WAN1 != nil || d.WAN2 != nil) {
			gatewaySeen = true
			if wan := c.wanFrom(ctx, d); wan != "" {
				state[stateKeyWAN] = wan
			}
			state[stateKeyISP] = ispFrom(d)
			c.crossCheckOverTime(ctx, state[stateKeyWAN], state[stateKeyISP])
		}
		if _, seen := state[stateKeyUPS]; !seen && d.VBMS != nil {
			state[stateKeyUPS] = upsOnline
			if d.VBMS.IsBatteryMode {
				state[stateKeyUPS] = upsOnBattery
			}
			state[stateKeyUPSBattery] = c.batteryLevel(d.VBMS.BattPool.BatteryLevel)
			// Runtime and load are each published only when the UPS reports
			// the numbers behind them. A UPS that reports charge but no
			// runtime estimate is a real thing to be, and inventing one is
			// worse than omitting the key.
			if runtime := c.runtimeLevel(d.VBMS.BattPool.TimeToRemain); runtime != "" {
				state[stateKeyUPSRuntime] = runtime
			}
			if load := c.loadLevel(d.VBMS.BattPool.TotalPowerOutput, d.VBMS.BattPool.TotalPowerBudget); load != "" {
				state[stateKeyUPSLoad] = load
			}
		}
	}
	fleet.publish(ctx, state, c.PerDeviceKeys)

	if len(state) == 0 {
		return nil, fmt.Errorf(
			"no gateway reporting WAN ports, no UPS, and no adopted device reporting a state "+
				"were found in the device list; "+
				"the fields this provider reads were verified on UniFi Network %s (%s), "+
				"and another version or console model may report them differently "+
				"— see the compatibility matrix in the README",
			VerifiedNetworkVersion, VerifiedConsoleModel)
	}
	return state, nil
}

// wanSignal is one field's answer to which uplink is live.
type wanSignal struct {
	// value is wanPrimary, wanBackup, or empty when the field says nothing.
	value string
	// ambiguous records that the field claimed both uplinks at once. That is
	// itself information — it means the field does not mean what this provider
	// takes it to mean — so it is kept apart from "said nothing".
	ambiguous bool
}

// byIsUplink is the signal the wan mapping has always been derived from, and
// the one issue #34 exists to verify: it has only ever been observed on a
// gateway with a single live uplink, so "the port with is_uplink set is the
// live one" is inference, not observation.
func (d deviceRecord) byIsUplink() wanSignal {
	primary := d.WAN1 != nil && d.WAN1.IsUplink
	backup := d.WAN2 != nil && d.WAN2.IsUplink
	return resolveSignal(primary, backup)
}

// byUplinkName is the independent second opinion: the gateway names the
// interface it is uplinked through in its own uplink block, and each WAN port
// names its interface. Matching one against the other answers the same
// question through entirely different fields.
func (d deviceRecord) byUplinkName() wanSignal {
	if d.Uplink == nil {
		return wanSignal{}
	}
	return resolveSignal(d.WAN1.named(d.Uplink.Name), d.WAN2.named(d.Uplink.Name))
}

func resolveSignal(primary, backup bool) wanSignal {
	switch {
	case primary && backup:
		return wanSignal{ambiguous: true}
	case backup:
		return wanSignal{value: wanBackup}
	case primary:
		return wanSignal{value: wanPrimary}
	}
	return wanSignal{}
}

// wanFrom decides which uplink is live, and reports rather than resolves any
// disagreement between the signals that say so.
//
// The rule is deliberately conservative: is_uplink keeps the cases it already
// answers, so no behaviour a real deployment depends on changes on the
// strength of a hypothesis. uplink.name only fills in the cases is_uplink
// leaves blank — no uplink claimed, or both claimed — which today produce a
// missing key and a coin flip respectively, so it can only be an improvement.
// Everything else is a log line, because deciding which signal wins needs a
// real failover to have been observed, and one has not been (issue #34).
func (c *Client) wanFrom(ctx context.Context, d deviceRecord) string {
	log := logf.FromContext(ctx).WithName("unifi-wan")
	claimed, named := d.byIsUplink(), d.byUplinkName()

	var wan string
	switch {
	case claimed.value != "" && named.value != "" && claimed.value != named.value:
		metrics.SignalsDisagreed(ProviderName, signalWANUplinkDisagrees)
		log.Info("The gateway's WAN signals disagree about which uplink is live; "+
			"trusting is_uplink, which is the signal this mapping has always used",
			"byIsUplink", claimed.value, "byUplinkName", named.value, "uplink", d.Uplink.Name)
		wan = claimed.value
	case claimed.value != "":
		wan = claimed.value
	case named.value != "":
		metrics.SignalsDisagreed(ProviderName, signalWANUplinkUnclaimed)
		log.Info("is_uplink does not name a single live WAN port; "+
			"deriving the live uplink from the gateway's uplink interface instead",
			"byUplinkName", named.value, "uplink", d.Uplink.Name, "bothPortsClaimedUplink", claimed.ambiguous)
		wan = named.value
	case claimed.ambiguous:
		// Both ports claim the uplink and nothing resolves it. Reporting the
		// backup is what this provider has always done here; it is a guess,
		// and saying so is the only honest thing available.
		metrics.SignalsDisagreed(ProviderName, signalWANUplinkAmbiguous)
		log.Info("Both WAN ports report is_uplink and nothing resolves which is live; "+
			"reporting the backup uplink, which may be wrong",
			"wan", wanBackup)
		wan = wanBackup
	default:
		log.V(1).Info("No WAN port reports is_uplink and the gateway names no uplink interface; wan will not be published")
		return ""
	}

	c.checkLastWANStatus(ctx, d, wan)
	return wan
}

// checkLastWANStatus compares the derived uplink against the gateway's own
// per-uplink status. Nothing is derived from that field because only one of
// its values has ever been observed ("online", on the primary, with the
// primary live) — so this reports the mismatch and leaves the interpretation
// to whoever reads the log with the hardware in front of them.
func (c *Client) checkLastWANStatus(ctx context.Context, d deviceRecord, wan string) {
	if len(d.LastWANStatus) == 0 {
		return
	}
	key := wanStatusKeyPrimary
	if wan == wanBackup {
		key = wanStatusKeyBackup
	}
	status, ok := d.LastWANStatus[key].(string)
	if !ok || status == wanStatusOnline {
		return
	}
	metrics.SignalsDisagreed(ProviderName, signalWANNotOnline)
	logf.FromContext(ctx).WithName("unifi-wan").Info(
		"The uplink believed to be live does not report itself as online",
		"wan", wan, "statusKey", key, "status", status, "lastWANStatus", fmt.Sprint(d.LastWANStatus))
}

// crossCheckOverTime reports when the uplink and the ISP behind it fail to
// move together. Neither can confirm the other on its own — nothing says which
// carrier belongs to which port — but across two observations they should
// change at the same time, and an ISP that changes while wan does not is
// exactly the shape a wrong wan mapping would have.
//
// Only real ISP names are remembered, so a momentary unknown during a failover
// does not count as a change and does not erase what was known before it.
func (c *Client) crossCheckOverTime(ctx context.Context, wan, isp string) {
	c.mu.Lock()
	was := c.previous
	if wan != "" {
		c.previous.wan = wan
	}
	if isp != "" && isp != ispUnknown {
		c.previous.isp = isp
	}
	c.mu.Unlock()

	if was.wan == "" || was.isp == "" || wan == "" || isp == "" || isp == ispUnknown {
		return
	}
	wanMoved, ispMoved := wan != was.wan, isp != was.isp
	if wanMoved == ispMoved {
		return
	}
	log := logf.FromContext(ctx).WithName("unifi-wan")
	if wanMoved {
		metrics.SignalsDisagreed(ProviderName, signalWANMovedWithoutISP)
		log.Info("The gateway changed uplink but the ISP behind it did not change; "+
			"one of the two signals is wrong and the wan mapping is the unverified one",
			"wanFrom", was.wan, "wanTo", wan, "isp", isp)
		return
	}
	metrics.SignalsDisagreed(ProviderName, signalISPMovedWithoutWAN)
	log.Info("The ISP behind the uplink changed but the gateway still reports the same uplink; "+
		"if this was a failover, the wan mapping missed it",
		"wan", wan, "ispFrom", was.isp, "ispTo", isp)
}

// ispFrom normalizes the carrier name into a value that can be written in an
// Automation. A gateway that names no carrier reports unknown rather than
// dropping the key: the gateway is visible, so this is an observation about
// it, not a loss of sight of it.
func ispFrom(d deviceRecord) string {
	if slug := slugify(d.ISP); slug != "" {
		return slug
	}
	return ispUnknown
}

// slugify lowercases and collapses every run of non-alphanumeric characters
// into a single hyphen, so "Telenet BV" becomes "telenet-bv".
func slugify(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	pendingHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingHyphen = false
			b.WriteRune(r)
		default:
			pendingHyphen = true
		}
	}
	return b.String()
}

// runtimeLevel buckets the UPS's own estimate of how long it can carry the
// current load. An empty result means the UPS offered no estimate.
//
// This is a better shutdown trigger than charge, and that is the whole point of
// the key: charge ignores load, and timeToRemain does not. It is published on
// mains as well as on battery, because "could we even survive an outage right
// now" is worth being able to ask before one starts.
func (c *Client) runtimeLevel(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	short, critical := c.ShortRuntimeSeconds, c.CriticalRuntimeSeconds
	if short <= 0 {
		short = DefaultShortRuntimeSeconds
	}
	if critical <= 0 {
		critical = DefaultCriticalRuntimeSeconds
	}
	switch {
	case seconds <= critical:
		return upsRuntimeCritical
	case seconds <= short:
		return upsRuntimeShort
	default:
		return upsRuntimeAmple
	}
}

// loadLevel buckets the draw as a fraction of the UPS's budget. An empty result
// means the UPS reported no usable pair of numbers — a missing output, or a
// budget of zero, which no fraction can be taken against.
func (c *Client) loadLevel(output, budget *float64) string {
	if output == nil || budget == nil || *budget <= 0 {
		return ""
	}
	high := c.HighLoadPercent
	if high <= 0 {
		high = DefaultHighLoadPercent
	}
	if (*output / *budget * 100) >= high {
		return upsLoadHigh
	}
	return upsLoadNormal
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
