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

// mock-unifi serves the captured UniFi stat/device payload from testdata so
// the operator can be developed and demoed on any machine without a UDM.
//
//	go run ./hack/mock-unifi [-addr :9443] [-testdata testdata/unifi/api/stat-device-gateway.json]
//
// POST /flip toggles which WAN is the active uplink (primary <-> backup),
// letting you rehearse a failover end-to-end:
//
//	curl -X POST http://localhost:9443/flip
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
)

func main() {
	addr := flag.String("addr", ":9443", "listen address")
	testdata := flag.String("testdata", "testdata/unifi/api/stat-device-gateway.json", "captured stat/device payload")
	flag.Parse()

	raw, err := os.ReadFile(*testdata)
	if err != nil {
		log.Fatalf("reading captured payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Fatalf("parsing captured payload: %v", err)
	}

	var mu sync.Mutex
	backup := false

	setUplink := func(onBackup bool) {
		data := payload["data"].([]any)
		for _, d := range data {
			device := d.(map[string]any)
			wan1, ok1 := device["wan1"].(map[string]any)
			wan2, ok2 := device["wan2"].(map[string]any)
			if !ok1 && !ok2 {
				continue
			}
			if ok1 {
				wan1["is_uplink"] = !onBackup
			}
			if ok2 {
				wan2["is_uplink"] = onBackup
				wan2["up"] = onBackup
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /proxy/network/api/s/{site}/stat/device", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		setUplink(backup)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("POST /flip", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backup = !backup
		state := map[bool]string{false: "primary", true: "backup"}[backup]
		mu.Unlock()
		log.Printf("flipped: wan is now %s", state)
		fmt.Fprintf(w, `{"wan":%q}`+"\n", state)
	})

	log.Printf("mock UniFi API on %s (wan=primary; POST /flip to fail over)", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux)) // #nosec G114 -- dev tool
}
