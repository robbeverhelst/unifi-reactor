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
// Rehearse a power outage (UPS on battery, draining):
//
//	curl -X POST 'http://localhost:9443/ups?mode=battery&level=80'
//	curl -X POST 'http://localhost:9443/ups?level=25'    # drains to low
//	curl -X POST 'http://localhost:9443/ups?level=5'     # drains to critical
//	curl -X POST 'http://localhost:9443/ups?mode=mains'  # power restored
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
	"strconv"
	"sync"
	"time"
)

// mockCSRF is the token the mock embeds in its session JWT and demands back on
// every write, the way a real console does.
const mockCSRF = "mock-csrf-token"

type mock struct {
	mu       sync.Mutex
	devices  []any
	onBackup bool
	onBatt   bool
	battLvl  int

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
	flag.Parse()

	m := &mock{battLvl: 100, rules: map[string]map[string]any{}}
	if raw, err := os.ReadFile(*deliveryFile); err == nil {
		m.delivery = raw
	} else {
		log.Printf("no synthetic delivery at %s (%v); /alarm-fire will post an empty body", *deliveryFile, err)
	}
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
		m.devices = append(m.devices, payload.Data...)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /proxy/network/api/s/{site}/stat/device", m.serveDevices)
	mux.HandleFunc("POST /flip", m.flipWAN)
	mux.HandleFunc("POST /ups", m.setUPS)

	// The UniFi OS layer: no /proxy/network prefix, cookie session, csrf header.
	mux.HandleFunc("POST /api/auth/login", m.login)
	mux.HandleFunc("GET /api/v2/alarms/network/manifest", m.serveManifest)
	mux.HandleFunc("GET /api/v2/alarms/network", m.serveRules)
	mux.HandleFunc("POST /api/v2/alarms/network", m.createRule)
	mux.HandleFunc("POST /alarm-fire", m.fireAlarm)

	log.Printf("mock UniFi API on %s: %d devices, wan=primary, ups=online (100%%)", *addr, len(m.devices))
	log.Fatal(http.ListenAndServe(*addr, mux)) // #nosec G114 -- dev tool
}

// apply rewrites the captured devices to match the mock's current state.
func (m *mock) apply() {
	for _, d := range m.devices {
		device, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if wan1, ok := device["wan1"].(map[string]any); ok {
			wan1["is_uplink"] = !m.onBackup
		}
		if wan2, ok := device["wan2"].(map[string]any); ok {
			wan2["is_uplink"] = m.onBackup
			wan2["up"] = m.onBackup
		}
		if vbms, ok := device["vbms_table"].(map[string]any); ok {
			vbms["is_battery_mode"] = m.onBatt
			if pool, ok := vbms["battpool"].(map[string]any); ok {
				pool["batteryLevel"] = m.battLvl
				pool["ischarging"] = !m.onBatt
			}
		}
	}
}

func (m *mock) serveDevices(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apply()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"meta": map[string]string{"rc": "ok"},
		"data": m.devices,
	})
}

func (m *mock) flipWAN(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	m.onBackup = !m.onBackup
	state := map[bool]string{false: "primary", true: "backup"}[m.onBackup]
	m.mu.Unlock()
	log.Printf("flipped: wan is now %s", state)
	_, _ = fmt.Fprintf(w, `{"wan":%q}`+"\n", state)
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
	state := map[bool]string{false: "online", true: "on-battery"}[m.onBatt]
	log.Printf("ups is now %s at %d%%", state, m.battLvl)
	_, _ = fmt.Fprintf(w, `{"ups":%q,"battery":%d}`+"\n", state, m.battLvl)
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
