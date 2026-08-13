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
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
)

type mock struct {
	mu       sync.Mutex
	devices  []any
	onBackup bool
	onBatt   bool
	battLvl  int
}

func main() {
	addr := flag.String("addr", ":9443", "listen address")
	dir := flag.String("testdata", "testdata/unifi/api", "directory holding captured stat/device payloads")
	flag.Parse()

	m := &mock{battLvl: 100}
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
