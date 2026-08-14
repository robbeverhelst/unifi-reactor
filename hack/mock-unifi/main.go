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

// mock-unifi serves the captured UniFi stat/device payloads from testdata so
// the operator can be developed and demoed on any machine without a UDM.
//
//	go run ./hack/mock-unifi [-addr :9443]
//
// Rehearse a WAN failover (toggles primary <-> backup):
//
//	curl -X POST http://localhost:9443/flip
//
// A real failover has never been observed (issue #34), so which fields actually
// move during one is unknown. /wan rehearses each hypothesis about that, so the
// parser can be driven against more than one of them:
//
//	curl -X POST 'http://localhost:9443/wan?link=backup&variant=clean'
//	curl -X POST 'http://localhost:9443/wan?link=backup&variant=is-uplink-pinned'
//	curl -X POST 'http://localhost:9443/wan?link=primary'   # recovery
//	curl http://localhost:9443/wan                          # state + every variant
//
// Only "clean" can be right, and possibly none of them is. The runbook for
// finding out with real hardware is in testdata/unifi/README.md.
//
// Rehearse the internet going away while the link stays up — the failure mode
// wan structurally cannot express, and the reason `internet` exists:
//
//	curl -X POST 'http://localhost:9443/internet?status=down'
//	curl -X POST 'http://localhost:9443/internet?status=degraded'
//	curl -X POST 'http://localhost:9443/internet?status=ok'
//	curl -X POST 'http://localhost:9443/internet?present=false'   # www subsystem gone
//
// Rehearse the live uplink getting bad rather than going away, which is what
// wan.quality buckets. Availability is a percentage and latency milliseconds,
// both averaged by the console over its uptime window:
//
//	curl -X POST 'http://localhost:9443/quality?availability=97'
//	curl -X POST 'http://localhost:9443/quality?latency=400'
//	curl -X POST 'http://localhost:9443/quality?present=false'    # no numbers at all
//	curl -X POST 'http://localhost:9443/quality?reset=true'       # back to the capture
//
// Rehearse a power outage (UPS on battery, draining):
//
//	curl -X POST 'http://localhost:9443/ups?mode=battery&level=80'
//	curl -X POST 'http://localhost:9443/ups?level=25'    # drains to low
//	curl -X POST 'http://localhost:9443/ups?level=5'     # drains to critical
//	curl -X POST 'http://localhost:9443/ups?mode=mains'  # power restored
//
// Runtime and load are the other two axes, and the reason charge alone is a
// poor shutdown trigger — 30% at 300W and 30% at 900W are different situations:
//
//	curl -X POST 'http://localhost:9443/ups?runtime=150'          # minutes become seconds
//	curl -X POST 'http://localhost:9443/ups?runtime=-1'           # the UPS offers no estimate
//	curl -X POST 'http://localhost:9443/ups?output=850'           # a heavy load on the same budget
//	curl -X POST 'http://localhost:9443/ups?output=310&budget=1000'  # back to the capture
//
// Rehearse the UPS dropping off the console entirely — the ups keys vanish
// from the state rather than reporting a value, which is what an Automation
// holding its last known state has to cope with:
//
//	curl -X POST 'http://localhost:9443/ups?present=false'
//	curl -X POST 'http://localhost:9443/ups?present=true'
//
// It also serves — and enforces — the write endpoints the unifi.* edge actions
// use. These are the first things Reactor changes on a console rather than
// reads from it, and no write has ever been made against real hardware, so the
// mock is where that path is exercised at all:
//
//	curl http://localhost:9443/wlan                               # what the console holds
//	curl -X POST 'http://localhost:9443/wlan?name=mock-guest&enabled=false'
//
// Enforcement is the point rather than a bonus. Like the alarm rule endpoint
// rejecting a flat triggers_data, the WLAN endpoint demands the csrf header,
// refuses a PUT that changes anything other than "enabled", and refuses one
// that arrives without the session cookie — so a Reactor that stopped checking
// before it wrote would fail here rather than on somebody's gateway.
//
// The WLAN records are NOT a capture. No wlanconf response has ever been
// recorded (testdata/unifi/README.md says which files are), so these are built
// from the field names in docs/unifi-write-api.md and the SSIDs are obviously
// fake. They prove Reactor sends what those notes describe; they do not prove a
// console answers this way.
//
// It also mocks enough of the undocumented Alarm Manager API
// (docs/unifi-alarm-manager-api.md) for Reactor to register its own webhook
// rule against, and can then fire a delivery at whatever URL that rule names:
//
//	curl -X POST http://localhost:9443/alarm-fire
//
// The mock's alarm responses are built from those reverse-engineering notes,
// not captured from a console. Registration working here means Reactor sends
// what the notes describe; it does not mean a real console accepts it.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mockCSRF is the token the mock embeds in its session JWT and demands back on
// every write, the way a real console does.
const mockCSRF = "mock-csrf-token"

const (
	linkPrimary = "primary"
	linkBackup  = "backup"

	// defaultVariant is what /flip uses: every signal moving together, which is
	// what the current wan mapping assumes a failover looks like.
	defaultVariant = "clean"

	// defaultBackupISP is deliberately not a real carrier name. The backup
	// uplink has never carried traffic, so the name the console would report
	// for it is unknown, and a plausible-looking guess in a dev tool is how
	// guesses end up being quoted as facts. Override it with ?isp=.
	defaultBackupISP = "Mock Backup Carrier"

	// statusFailed is what this mock reports for a downed uplink in
	// last_wan_status. IT IS A GUESS: the only value ever observed on real
	// hardware is "online", captured with the primary uplink live. Nothing in
	// the provider derives state from this field for exactly that reason.
	statusFailed = "failed"
	statusOnline = "online"

	// The stat/health subsystems and the uptime_stats keys this mock rewrites,
	// named exactly as the capture spells them.
	subsystemWWW     = "www"
	subsystemWAN     = "wan"
	uptimeKeyPrimary = "WAN"
	uptimeKeyBackup  = "WAN2"

	// The two uptime_stats fields wan.quality is bucketed from, and the query
	// parameters that drive them.
	fieldAvailability = "availability"
	fieldLatency      = "latency_average"
	paramAvailability = "availability"
	paramLatency      = "latency"

	// The battpool fields ups.runtime and ups.load are derived from.
	fieldTimeToRemain = "timeToRemain"
	fieldPowerOutput  = "device_total_power_output"
	fieldPowerBudget  = "device_total_power_budget"

	// fieldEnabled is the one WLAN field the write path changes, and keyNote is
	// how every dev endpoint here labels the sentence explaining what its answer
	// is and is not evidence of.
	fieldEnabled = "enabled"
	keyNote      = "note"

	// statusHealthOK is what the capture's www subsystem reports. The other
	// values this mock will happily serve — "warning", "error" — are the ones
	// the provider maps to degraded and down, and neither has ever been seen
	// on a real console's www subsystem. Serving them here rehearses what
	// Reactor does with them; it does not confirm a console ever sends them.
	statusHealthOK = "ok"

	// What a variant says wan1/wan2 is_uplink do when the backup takes over.
	uplinkMoves   = "moves"
	uplinkPinned  = "pinned"
	uplinkBoth    = "both"
	uplinkNeither = "neither"
)

// failoverVariant is one hypothesis about which fields a real failover moves.
// Together they are the reason this mock exists beyond a demo: the parser
// should be exercised against every shape a failover might have, not only the
// one the mapping already assumes.
type failoverVariant struct {
	// isUplink says what wan1/wan2 is_uplink do when the backup takes over:
	// "moves" (the assumption), "pinned" (is_uplink means "configured as
	// primary" and never moves), "both", or "neither".
	isUplink string
	// context says whether uplink.name, last_wan_status and the ISP follow the
	// backup uplink.
	context bool
	why     string
}

var failoverVariants = map[string]failoverVariant{
	defaultVariant: {
		isUplink: uplinkMoves, context: true,
		why: "every signal moves together — what the wan mapping assumes, " +
			"and the only variant it gets right for the right reason",
	},
	"is-uplink-only": {
		isUplink: uplinkMoves, context: false,
		why: "is_uplink moves and nothing else does — the mapping is right, " +
			"but its cross-checks contradict it",
	},
	"is-uplink-pinned": {
		isUplink: uplinkPinned, context: true,
		why: "is_uplink means 'the port configured as primary' and never moves — " +
			"the mapping reports primary right through a failover",
	},
	"both-uplinks": {
		isUplink: uplinkBoth, context: true,
		why: "is_uplink means 'configured as an uplink', so both ports claim it whenever both are up",
	},
	"no-uplink": {
		isUplink: uplinkNeither, context: true,
		why: "the switchover window: the old uplink has dropped and the new one is not claimed yet",
	},
}

func variantNames() []string { return slices.Sorted(maps.Keys(failoverVariants)) }

type mock struct {
	mu sync.Mutex
	// pristine is the captured device list as JSON. Every response is built
	// from it rather than from the last response, so the mock can move fields
	// around without ever having to undo what it did.
	pristine []byte

	// pristineHealth is the captured stat/health response, kept for the same
	// reason as pristine: every response is rebuilt from the capture rather
	// than from the last one.
	pristineHealth []byte

	link           string
	variant        string
	backupISP      string
	networkVersion string
	onBatt         bool
	battLvl        int
	// runtime, output and budget override the battpool numbers ups.runtime and
	// ups.load are derived from. Nil means "serve whatever the capture has".
	runtime *int
	output  *float64
	budget  *float64

	// wwwStatus is what the www subsystem reports, and noWWW drops the
	// subsystem entirely — the case where the internet key vanishes rather
	// than reporting a value.
	wwwStatus string
	noWWW     bool
	// availability and latency override the live uplink's uptime_stats.
	// Nil means "serve whatever the capture has"; noQuality strips the
	// numbers out so the uplink reports none at all.
	availability *float64
	latency      *float64
	noQuality    bool
	// noUPS drops the UPS from the device list, as an unadopted or powered-off
	// one would be. The provider then publishes no ups keys at all rather than
	// a placeholder value, which is the case that must not be read as "the
	// outage ended".
	noUPS bool

	// wlans is the wlanconf table the write actions read and change. Synthetic
	// — see the package comment — and keyed by the id the mock made up.
	wlans map[string]map[string]any

	// delivery is the synthetic body /alarm-fire posts. It is a stand-in, not
	// a capture: no real Alarm Manager delivery has been recorded yet.
	delivery []byte
	// rules holds whatever Reactor registered, keyed by the id the mock made up.
	rules map[string]map[string]any
}

func main() {
	addr := flag.String("addr", ":9443", "listen address")
	dir := flag.String("testdata", "testdata/unifi/api", "directory holding captured stat/device payloads")
	deliveryFile := flag.String("delivery", "hack/dev/webhook-delivery.json",
		"synthetic Alarm Manager delivery body posted by /alarm-fire")
	networkVersion := flag.String("network-version", "",
		"UniFi Network version to report; empty serves the captured one. Set 9.3.45 or 11.0.0 "+
			"to rehearse Reactor's compatibility warning.")
	flag.Parse()

	m := &mock{
		battLvl:   100,
		link:      linkPrimary,
		variant:   defaultVariant,
		backupISP: defaultBackupISP,
		wwwStatus: statusHealthOK,
		rules:     map[string]map[string]any{},
		wlans:     mockWLANs(),
	}
	if raw, err := os.ReadFile(*deliveryFile); err == nil {
		m.delivery = raw
	} else {
		log.Printf("no synthetic delivery at %s (%v); /alarm-fire will post an empty body", *deliveryFile, err)
	}
	devices := make([]any, 0, 2)
	for _, name := range []string{"stat-device-gateway.json", "stat-device-ups.json"} {
		raw, err := os.ReadFile(*dir + "/" + name)
		if err != nil {
			log.Fatalf("reading %s: %v", name, err)
		}
		var payload struct {
			Data []any `json:"data"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			log.Fatalf("parsing %s: %v", name, err)
		}
		devices = append(devices, payload.Data...)
	}
	pristine, err := json.Marshal(devices)
	if err != nil {
		log.Fatalf("re-encoding the captured devices: %v", err)
	}
	m.pristine = pristine

	health, err := os.ReadFile(*dir + "/stat-health.json")
	if err != nil {
		log.Fatalf("reading stat-health.json: %v", err)
	}
	m.pristineHealth = health

	m.networkVersion = *networkVersion
	if m.networkVersion == "" {
		var info struct {
			ApplicationVersion string `json:"applicationVersion"`
		}
		raw, err := os.ReadFile(*dir + "/integration-info.json")
		if err == nil {
			err = json.Unmarshal(raw, &info)
		}
		if err != nil {
			log.Fatalf("reading integration-info.json: %v", err)
		}
		m.networkVersion = info.ApplicationVersion
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /proxy/network/api/s/{site}/stat/device", m.serveDevices)
	mux.HandleFunc("GET /proxy/network/api/s/{site}/stat/health", m.serveHealth)
	mux.HandleFunc("GET /proxy/network/integration/v1/info", m.serveInfo)
	mux.HandleFunc("POST /flip", m.flipWAN)
	mux.HandleFunc("GET /wan", m.describeWAN)
	mux.HandleFunc("POST /wan", m.setWAN)
	mux.HandleFunc("POST /ups", m.setUPS)
	mux.HandleFunc("POST /internet", m.setInternet)
	mux.HandleFunc("POST /quality", m.setQuality)

	// The write path: the Network application under /proxy/network, but
	// authenticated the UniFi OS way — a session cookie plus the csrf header.
	mux.HandleFunc("GET /proxy/network/api/s/{site}/rest/wlanconf", m.serveWLANConf)
	mux.HandleFunc("PUT /proxy/network/api/s/{site}/rest/wlanconf/{id}", m.updateWLANConf)
	mux.HandleFunc("GET /wlan", m.describeWLANs)
	mux.HandleFunc("POST /wlan", m.setWLAN)

	// The UniFi OS layer: no /proxy/network prefix, cookie session, csrf header.
	mux.HandleFunc("POST /api/auth/login", m.login)
	mux.HandleFunc("POST /api/auth/logout", m.logout)
	mux.HandleFunc("GET /api/v2/alarms/network/manifest", m.serveManifest)
	mux.HandleFunc("GET /api/v2/alarms/network", m.serveRules)
	mux.HandleFunc("POST /api/v2/alarms/network", m.createRule)
	mux.HandleFunc("POST /alarm-fire", m.fireAlarm)

	log.Printf("mock UniFi API on %s: wan=%s, ups=online (100%%)", *addr, m.link)
	log.Printf("failover variants: %s (GET /wan explains each)", strings.Join(variantNames(), ", "))
	log.Fatal(http.ListenAndServe(*addr, mux)) // #nosec G114 -- dev tool
}

// devices rebuilds the device list from the capture and rewrites it to match
// the mock's current state. Starting from the capture every time is what lets
// a failover variant move fields around without needing to put them back.
func (m *mock) devices() []any {
	var devices []any
	if err := json.Unmarshal(m.pristine, &devices); err != nil {
		log.Printf("re-reading the captured devices: %v", err)
		return nil
	}
	visible := devices[:0]
	for _, d := range devices {
		device, ok := d.(map[string]any)
		if !ok {
			visible = append(visible, d)
			continue
		}
		if _, isUPS := device["vbms_table"]; isUPS && m.noUPS {
			continue
		}
		visible = append(visible, d)
		if _, isGateway := device["wan1"]; isGateway && m.link == linkBackup {
			m.failover(device)
		}
		if vbms, ok := device["vbms_table"].(map[string]any); ok {
			vbms["is_battery_mode"] = m.onBatt
			if pool, ok := vbms["battpool"].(map[string]any); ok {
				pool["batteryLevel"] = m.battLvl
				pool["ischarging"] = !m.onBatt
				m.rewriteBattPool(pool)
			}
		}
	}
	return visible
}

// rewriteBattPool applies the runtime and load overrides. A runtime of exactly
// zero deletes the field rather than serving 0, because "the UPS reports no
// estimate" is a distinct case from "the UPS reports none left" and the
// provider treats it as one.
func (m *mock) rewriteBattPool(pool map[string]any) {
	if m.runtime != nil {
		if *m.runtime == 0 {
			delete(pool, fieldTimeToRemain)
		} else {
			pool[fieldTimeToRemain] = *m.runtime
		}
	}
	if m.output != nil {
		pool[fieldPowerOutput] = *m.output
	}
	if m.budget != nil {
		pool[fieldPowerBudget] = *m.budget
	}
}

// failover rewrites a captured gateway record the way the current variant says
// a real failover would. The primary state needs no rewriting at all: it is
// the capture.
func (m *mock) failover(device map[string]any) {
	variant := failoverVariants[m.variant]
	wan1, _ := device["wan1"].(map[string]any)
	wan2, _ := device["wan2"].(map[string]any)
	if wan1 == nil || wan2 == nil {
		return
	}

	switch variant.isUplink {
	case uplinkMoves:
		wan1["is_uplink"], wan2["is_uplink"] = false, true
	case uplinkPinned:
		// left exactly as captured: wan1 keeps the claim
	case uplinkBoth:
		wan1["is_uplink"], wan2["is_uplink"] = true, true
	case uplinkNeither:
		wan1["is_uplink"], wan2["is_uplink"] = false, false
	}
	// The physical reality of a failover in every variant: the primary link is
	// down and the backup is carrying traffic. Only the reporting differs.
	wan1["up"], wan2["up"] = false, true

	if !variant.context {
		return
	}
	if uplink, ok := device["uplink"].(map[string]any); ok {
		uplink["name"] = wan2["ifname"]
	}
	device["last_wan_status"] = map[string]any{"WAN": statusFailed, "WAN2": statusOnline}
	device["isp"] = m.backupISP
}

// health rebuilds the captured stat/health response and rewrites it to match
// the mock's current state, the way devices() does for stat/device.
//
// The uptime_stats half follows the mock's uplink, because the provider treats
// uptime accumulating on a port other than the one wan names as evidence the
// wan mapping is wrong. A mock that left uptime on WAN while claiming to be on
// the backup would report a disagreement on every rehearsed failover.
func (m *mock) health() []any {
	var payload struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(m.pristineHealth, &payload); err != nil {
		log.Printf("re-reading the captured health: %v", err)
		return nil
	}
	subsystems := payload.Data[:0]
	for _, entry := range payload.Data {
		subsystem, ok := entry.(map[string]any)
		if !ok {
			subsystems = append(subsystems, entry)
			continue
		}
		switch subsystem["subsystem"] {
		case subsystemWWW:
			if m.noWWW {
				continue
			}
			subsystem["status"] = m.wwwStatus
		case subsystemWAN:
			m.rewriteUptimeStats(subsystem)
		}
		subsystems = append(subsystems, entry)
	}
	return subsystems
}

// rewriteUptimeStats moves the live uplink's numbers onto whichever uplink the
// mock says is carrying traffic, then applies any overrides.
func (m *mock) rewriteUptimeStats(subsystem map[string]any) {
	stats, ok := subsystem["uptime_stats"].(map[string]any)
	if !ok {
		return
	}
	live := uptimeKeyPrimary
	if m.link == linkBackup {
		live = uptimeKeyBackup
		// The capture only ever had numbers on WAN, so a backup that is live
		// has to be handed them: swap the two entries wholesale.
		stats[uptimeKeyPrimary], stats[uptimeKeyBackup] = stats[uptimeKeyBackup], stats[uptimeKeyPrimary]
	}
	entry, ok := stats[live].(map[string]any)
	if !ok {
		return
	}
	if m.noQuality {
		for _, field := range []string{fieldAvailability, fieldLatency, "monitors", "alerting_monitors"} {
			delete(entry, field)
		}
		return
	}
	if m.availability != nil {
		entry[fieldAvailability] = *m.availability
	}
	if m.latency != nil {
		entry[fieldLatency] = *m.latency
	}
}

func (m *mock) serveHealth(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	subsystems := m.health()
	m.mu.Unlock()
	writeResponse(w, subsystems)
}

// writeResponse wraps a payload in the meta/data envelope every stat endpoint
// answers with.
func writeResponse(w http.ResponseWriter, data []any) {
	writeJSON(w, map[string]any{
		"meta": map[string]string{"rc": "ok"},
		"data": data,
	})
}

// setInternet drives the www subsystem, which is what the internet key reads.
// present=false removes the subsystem entirely, so the key vanishes rather
// than reporting a value — the case an Automation has to hold its claim
// through rather than treat as recovery.
func (m *mock) setInternet(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	m.mu.Lock()
	defer m.mu.Unlock()

	if status := query.Get("status"); status != "" {
		m.wwwStatus = status
	}
	if raw := query.Get("present"); raw != "" {
		present, err := strconv.ParseBool(raw)
		if err != nil {
			http.Error(w, "present must be a boolean", http.StatusBadRequest)
			return
		}
		m.noWWW = !present
	}
	log.Printf("www subsystem is now %s (present=%v)", m.wwwStatus, !m.noWWW)
	writeJSON(w, map[string]any{"status": m.wwwStatus, "present": !m.noWWW})
}

// setQuality drives the live uplink's uptime_stats, which is what wan.quality
// buckets. Both numbers are averages the console keeps over its uptime window,
// so they move slowly on real hardware and instantly here.
func (m *mock) setQuality(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	m.mu.Lock()
	defer m.mu.Unlock()

	if raw := query.Get("reset"); raw != "" {
		m.availability, m.latency, m.noQuality = nil, nil, false
	}
	for _, field := range []struct {
		name   string
		target **float64
	}{
		{paramAvailability, &m.availability},
		{paramLatency, &m.latency},
	} {
		raw := query.Get(field.name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			http.Error(w, field.name+" must be a number", http.StatusBadRequest)
			return
		}
		*field.target = &value
	}
	if raw := query.Get("present"); raw != "" {
		present, err := strconv.ParseBool(raw)
		if err != nil {
			http.Error(w, "present must be a boolean", http.StatusBadRequest)
			return
		}
		m.noQuality = !present
	}
	log.Printf("uplink quality overrides: availability=%s latency=%s present=%v",
		describeFloat(m.availability), describeFloat(m.latency), !m.noQuality)
	writeJSON(w, map[string]any{
		paramAvailability: m.availability,
		paramLatency:      m.latency,
		"present":         !m.noQuality,
		keyNote:           "both are averages over the console's uptime window (time_period, 86400s in the capture)",
	})
}

func describeFloat(value *float64) string {
	if value == nil {
		return "captured"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func (m *mock) serveDevices(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	devices := m.devices()
	m.mu.Unlock()
	writeResponse(w, devices)
}

// serveInfo answers the Integration API endpoint Reactor's compatibility guard
// reads at startup, so the guard can be rehearsed — including its warnings, by
// passing -network-version 9.3.45 or 11.0.0.
func (m *mock) serveInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"applicationVersion": m.networkVersion})
}

// flipWAN toggles between the captured primary state and a failover in
// whichever variant is current, defaulting to the one the mapping assumes.
func (m *mock) flipWAN(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if name := r.URL.Query().Get("variant"); name != "" {
		if _, known := failoverVariants[name]; !known {
			m.mu.Unlock()
			unknownVariant(w, name)
			return
		}
		m.variant = name
	}
	if m.link == linkBackup {
		m.link = linkPrimary
	} else {
		m.link = linkBackup
	}
	link, variant := m.link, m.variant
	m.mu.Unlock()

	log.Printf("flipped: wan is now %s (variant %s)", link, variant)
	_, _ = fmt.Fprintf(w, `{"wan":%q,"variant":%q}`+"\n", link, variant)
}

// setWAN is the explicit form: which uplink is live, and which hypothesis
// about a failover to render it under.
func (m *mock) setWAN(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	m.mu.Lock()
	defer m.mu.Unlock()

	if name := query.Get("variant"); name != "" {
		if _, known := failoverVariants[name]; !known {
			unknownVariant(w, name)
			return
		}
		m.variant = name
	}
	switch link := query.Get("link"); link {
	case linkPrimary, linkBackup:
		m.link = link
	case "":
	default:
		http.Error(w, `link must be "primary" or "backup"`, http.StatusBadRequest)
		return
	}
	if isp := query.Get("isp"); isp != "" {
		m.backupISP = isp
	}

	log.Printf("wan is now %s (variant %s: %s)", m.link, m.variant, failoverVariants[m.variant].why)
	writeJSON(w, map[string]any{"wan": m.link, "variant": m.variant, "backupISP": m.backupISP})
}

// describeWAN reports the current state and what every variant means, so the
// list does not have to be remembered or looked up in this file.
func (m *mock) describeWAN(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	variants := make(map[string]any, len(failoverVariants))
	for name, variant := range failoverVariants {
		variants[name] = map[string]any{
			"is_uplink":    variant.isUplink,
			"contextMoves": variant.context,
			"hypothesis":   variant.why,
		}
	}
	writeJSON(w, map[string]any{
		"wan":       m.link,
		"variant":   m.variant,
		"backupISP": m.backupISP,
		"variants":  variants,
		keyNote: "no real failover has ever been observed (issue #34); " +
			"every variant here is a hypothesis, and the capture runbook in testdata/unifi/README.md settles it",
	})
}

func unknownVariant(w http.ResponseWriter, name string) {
	http.Error(w, fmt.Sprintf("unknown variant %q; try one of: %s\n", name, strings.Join(variantNames(), ", ")),
		http.StatusBadRequest)
}

func (m *mock) setUPS(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.URL.Query().Get("mode") {
	case "battery":
		m.onBatt = true
	case "mains":
		m.onBatt = false
	case "":
	default:
		http.Error(w, `mode must be "battery" or "mains"`, http.StatusBadRequest)
		return
	}
	if raw := r.URL.Query().Get("level"); raw != "" {
		level, err := strconv.Atoi(raw)
		if err != nil || level < 0 || level > 100 {
			http.Error(w, "level must be an integer between 0 and 100", http.StatusBadRequest)
			return
		}
		m.battLvl = level
	}
	if raw := r.URL.Query().Get("present"); raw != "" {
		present, err := strconv.ParseBool(raw)
		if err != nil {
			http.Error(w, "present must be a boolean", http.StatusBadRequest)
			return
		}
		m.noUPS = !present
	}
	// runtime is seconds, and 0 means "the UPS reports no estimate at all"
	// rather than "no time left" — the case the provider omits the key for.
	if raw := r.URL.Query().Get("runtime"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "runtime must be an integer number of seconds", http.StatusBadRequest)
			return
		}
		m.runtime = &seconds
	}
	for _, field := range []struct {
		name   string
		target **float64
	}{
		{"output", &m.output},
		{"budget", &m.budget},
	} {
		raw := r.URL.Query().Get(field.name)
		if raw == "" {
			continue
		}
		watts, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			http.Error(w, field.name+" must be a number of watts", http.StatusBadRequest)
			return
		}
		*field.target = &watts
	}

	state := map[bool]string{false: "online", true: "on-battery"}[m.onBatt]
	if m.noUPS {
		state = "absent"
	}
	log.Printf("ups is now %s at %d%% (runtime=%s output=%s budget=%s)",
		state, m.battLvl, describeInt(m.runtime), describeFloat(m.output), describeFloat(m.budget))
	writeJSON(w, map[string]any{
		"ups": state, "battery": m.battLvl,
		"runtime": m.runtime, "output": m.output, "budget": m.budget,
	})
}

func describeInt(value *int) string {
	if value == nil {
		return "captured"
	}
	return strconv.Itoa(*value)
}

// --- WLAN configuration (the write path) ------------------------------------

// mockWLANs builds the wlanconf table. NOT A CAPTURE: no wlanconf response has
// ever been recorded from a console, so these carry only the three fields the
// write path reads plus a couple of neighbours, and the SSIDs are deliberately
// not anybody's. The extra fields are here to prove one thing — that Reactor
// hands the record back unchanged apart from "enabled".
func mockWLANs() map[string]map[string]any {
	table := map[string]map[string]any{}
	for i, wlan := range []struct {
		name    string
		enabled bool
	}{
		{"mock-main", true},
		{"mock-guest", true},
		{"mock-iot", false},
	} {
		id := fmt.Sprintf("019ff10d-1111-0000-0000-%012d", i+1)
		table[id] = map[string]any{
			"_id":          id,
			"name":         wlan.name,
			fieldEnabled:   wlan.enabled,
			"security":     "wpapsk",
			"wpa_mode":     "wpa2",
			"usergroup_id": "019ff10d-2222-0000-0000-000000000001",
		}
	}
	return table
}

// sessionCookiePresent reports whether a request carries the session the login
// handed out. The real console authenticates reads as well as writes, and a
// Reactor that read wlanconf without logging in first would work against a mock
// that did not check — right up until it did not work against hardware.
func (m *mock) sessionCookiePresent(r *http.Request) bool {
	cookie, err := r.Cookie("TOKEN")
	return err == nil && cookie.Value != ""
}

func (m *mock) serveWLANConf(w http.ResponseWriter, r *http.Request) {
	if !m.sessionCookiePresent(r) {
		http.Error(w, `{"message":"api.err.LoginRequired"}`, http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	records := make([]any, 0, len(m.wlans))
	for _, wlan := range m.wlans {
		records = append(records, maps.Clone(wlan))
	}
	m.mu.Unlock()
	slices.SortFunc(records, func(a, b any) int {
		return strings.Compare(wlanName(a), wlanName(b))
	})
	writeResponse(w, records)
}

func wlanName(record any) string {
	wlan, _ := record.(map[string]any)
	name, _ := wlan["name"].(string)
	return name
}

// updateWLANConf is the enforcing half. It accepts the read-modify-write the
// endpoint only offers in that shape, and refuses anything that would be a
// wider change than the action claims to make: a missing csrf header, a missing
// session, an unknown id, or a body that differs from the stored record in more
// than "enabled".
//
// That last check is the one worth having. It is what would catch a future
// change to the WLAN action that started sending a record it had built rather
// than one it had read — which on real hardware would silently rewrite somebody
// else's wireless configuration.
func (m *mock) updateWLANConf(w http.ResponseWriter, r *http.Request) {
	if !m.sessionCookiePresent(r) {
		http.Error(w, `{"message":"api.err.LoginRequired"}`, http.StatusUnauthorized)
		return
	}
	if r.Header.Get("x-csrf-token") != mockCSRF {
		http.Error(w, `{"message":"csrf token mismatch"}`, http.StatusForbidden)
		return
	}
	var submitted map[string]any
	if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
		http.Error(w, `{"message":"invalid json"}`, http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	stored, known := m.wlans[r.PathValue("id")]
	if !known {
		http.Error(w, `{"message":"api.err.ObjectNotFound"}`, http.StatusNotFound)
		return
	}
	if changed := changedKeys(stored, submitted); !slices.Equal(changed, []string{fieldEnabled}) {
		http.Error(w, fmt.Sprintf(
			`{"message":"this mock only accepts a record read back with enabled changed; changed: %v"}`, changed),
			http.StatusBadRequest)
		return
	}

	enabled, _ := submitted[fieldEnabled].(bool)
	stored[fieldEnabled] = enabled
	log.Printf("wlan %q is now enabled=%v", stored["name"], enabled)
	writeResponse(w, []any{maps.Clone(stored)})
}

// changedKeys names every key that differs between two records, in either
// direction, so an added or removed field counts as a change too.
func changedKeys(stored, submitted map[string]any) []string {
	var changed []string
	for key := range maps.Keys(stored) {
		if !equalJSON(stored[key], submitted[key]) {
			changed = append(changed, key)
		}
	}
	for key := range maps.Keys(submitted) {
		if _, present := stored[key]; !present {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return slices.Compact(changed)
}

// equalJSON compares two decoded values by their encoding, which is enough for
// the flat records here and does not need a reflect-based deep equal.
func equalJSON(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && bytes.Equal(left, right)
}

// describeWLANs and setWLAN are the dev endpoints: what the console holds, and
// a way to put a WLAN back without going through Reactor.
func (m *mock) describeWLANs(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := map[string]any{}
	for _, wlan := range m.wlans {
		name, _ := wlan["name"].(string)
		state[name] = wlan[fieldEnabled]
	}
	writeJSON(w, map[string]any{
		"wlans": state,
		keyNote: "synthetic, not a capture: no wlanconf response has ever been recorded from a console. " +
			"See docs/unifi-write-api.md for what is known and what is assumed.",
	})
}

func (m *mock) setWLAN(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	enabled, err := strconv.ParseBool(r.URL.Query().Get(fieldEnabled))
	if name == "" || err != nil {
		http.Error(w, "name and enabled are both required, e.g. ?name=mock-guest&enabled=false",
			http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, wlan := range m.wlans {
		if wlan["name"] != name {
			continue
		}
		wlan[fieldEnabled] = enabled
		log.Printf("wlan %q set to enabled=%v by hand", name, enabled)
		writeJSON(w, map[string]any{"wlan": name, fieldEnabled: enabled})
		return
	}
	http.Error(w, fmt.Sprintf("no wlan named %q", name), http.StatusNotFound)
}

// --- Alarm Manager (UniFi OS layer) -----------------------------------------

// login issues a session cookie shaped like the console's: a JWT whose payload
// carries the csrfToken claim that every write must echo. Any credentials are
// accepted; this mock is about shapes, not authentication.
func (m *mock) login(w http.ResponseWriter, _ *http.Request) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"csrfToken":"` + mockCSRF + `"}`))
	http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "header." + payload + ".signature", Path: "/"})
	w.Header().Set("x-csrf-token", mockCSRF)
	writeJSON(w, map[string]any{"unique_id": "00000000-0000-0000-0000-0000000000ff"})
}

// logout ends a session. The write path calls it after every action so a
// console session is never held longer than the action that needed it; the
// verb is INFERRED like the rest of this API, and answering it here is what
// lets that call be exercised at all.
func (m *mock) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{})
}

// serveManifest offers the trigger and action IDs the notes record for Network
// 10.5.67. Reactor checks its IDs against this before writing anything.
func (m *mock) serveManifest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"triggers": []any{map[string]any{
			"id": "network:category_internet",
			"items": []any{
				map[string]any{"id": "network:internet_disconnected"},
				map[string]any{"id": "network:high_latency_detected"},
				map[string]any{"id": "network:packet_loss_detected"},
				map[string]any{"id": "network:data_limit"},
			},
		}},
		"actions": []any{
			map[string]any{"id": "network:webhook"},
			map[string]any{"id": "network:slack"},
		},
	})
}

func (m *mock) serveRules(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rules := make([]any, 0, len(m.rules))
	for id, rule := range m.rules {
		listed := map[string]any{"id": id}
		maps.Copy(listed, rule)
		rules = append(rules, listed)
	}
	writeJSON(w, map[string]any{"data": rules})
}

// createRule enforces the two things the real API is known to be strict about:
// the csrf header, and triggers_data / actions_data being sequences of
// sequences rather than flat arrays.
func (m *mock) createRule(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-csrf-token") != mockCSRF {
		http.Error(w, `{"message":"csrf token mismatch"}`, http.StatusForbidden)
		return
	}
	var rule map[string]any
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, `{"message":"invalid json"}`, http.StatusBadRequest)
		return
	}
	for _, field := range []string{"triggers_data", "actions_data"} {
		outer, ok := rule[field].([]any)
		if !ok || len(outer) == 0 {
			http.Error(w, `{"message":"`+field+`: expected a sequence of sequences"}`, http.StatusBadRequest)
			return
		}
		if _, ok := outer[0].([]any); !ok {
			http.Error(w, `{"message":"`+field+`: invalid type: sequence, expected a sequence of sequences"}`,
				http.StatusBadRequest)
			return
		}
	}

	id := fmt.Sprintf("019ff10d-0000-0000-0000-%012d", len(m.rules)+1)
	m.mu.Lock()
	m.rules[id] = rule
	m.mu.Unlock()

	log.Printf("registered alarm rule %q as %s -> %s", rule["title"], id, ruleURL(rule))
	rule["id"] = id
	writeJSON(w, rule)
}

// fireAlarm posts the synthetic delivery to whatever URL a registered rule
// names, presenting the token that rule carries. This is the mock standing in
// for a console that noticed something.
func (m *mock) fireAlarm(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	rules := make([]map[string]any, 0, len(m.rules))
	for _, rule := range m.rules {
		rules = append(rules, rule)
	}
	m.mu.Unlock()

	if len(rules) == 0 {
		http.Error(w, "no alarm rule registered; start Reactor with self-registration enabled\n",
			http.StatusPreconditionFailed)
		return
	}
	for _, rule := range rules {
		url, token := ruleURL(rule), ruleToken(rule)
		if url == "" {
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(m.delivery))
		if err != nil {
			log.Printf("delivery to %s could not be built: %v", url, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			log.Printf("delivery to %s failed: %v", url, err)
			_, _ = fmt.Fprintf(w, "delivery to %s failed: %v\n", url, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		log.Printf("delivered to %s -> %s", url, resp.Status)
		_, _ = fmt.Fprintf(w, "delivered to %s -> %s\n", url, resp.Status)
	}
}

// ruleURL and ruleToken dig the webhook action out of the arrays-of-arrays
// body, tolerating anything that does not look the way it should.
func ruleURL(rule map[string]any) string {
	data, ok := webhookActionData(rule)
	if !ok {
		return ""
	}
	url, _ := data["url"].(string)
	return url
}

func ruleToken(rule map[string]any) string {
	data, ok := webhookActionData(rule)
	if !ok {
		return ""
	}
	auth, ok := data["auth"].(map[string]any)
	if !ok {
		return ""
	}
	token, _ := auth["token"].(string)
	return token
}

func webhookActionData(rule map[string]any) (map[string]any, bool) {
	outer, ok := rule["actions_data"].([]any)
	if !ok {
		return nil, false
	}
	for _, group := range outer {
		members, ok := group.([]any)
		if !ok {
			continue
		}
		for _, member := range members {
			action, ok := member.(map[string]any)
			if !ok || action["id"] != "network:webhook" {
				continue
			}
			data, ok := action["data"].(map[string]any)
			return data, ok
		}
	}
	return nil, false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
