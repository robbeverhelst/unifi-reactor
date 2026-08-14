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
	"encoding/json"
	"testing"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
)

// Two things in this package read a switch's port_table, and they are
// deliberately not the same reader.
//
// The write path (write.go) navigates map[string]any fetched through the UniFi
// OS session, because it must not lose fields it does not know about, and it
// refuses on any field it cannot read: a guard that silently stops applying is
// worse than one that declines. The poe state key (poe.go) decodes a typed
// struct from the API-key poll, because absent must be distinguishable from
// zero on every number, and it omits its key rather than refusing anything.
// Neither could be the other without giving up the property that makes it
// correct.
//
// What they DO share is the field names, and Go cannot put a constant in a
// struct tag — so `poe_enable` and `port_idx` are spelled in two files, and a
// firmware that renamed one would be fixed in one place by whoever noticed.
// This test is the thing that notices: one port table, both readers, and an
// assertion that they agree about the same port.
func TestBothPoEReadersAgreeOnOnePortTable(t *testing.T) {
	// The documented shape, with poe_power as the string UniFi is documented to
	// use. It carries every field either reader looks at, which is the point:
	// this document is the contract between them.
	const document = `{"data":[{
		"mac": "aa:bb:cc:00:11:33", "model": "USW48P", "type": "usw",
		"name": "Switch", "state": 1, "adopted": true,
		"total_max_power": 195,
		"port_table": [
			{"port_idx": 1, "name": "uplink", "is_uplink": true,  "port_poe": true, "poe_enable": true,  "poe_power": "0.00"},
			{"port_idx": 7, "name": "ap",     "is_uplink": false, "port_poe": true, "poe_enable": true,  "poe_power": "13.20"},
			{"port_idx": 9, "name": "spare",  "is_uplink": false, "port_poe": true, "poe_enable": false, "poe_power": "0.00"}
		]
	}]}`

	t.Run("the state key measures it", func(t *testing.T) {
		var parsed deviceStatResponse
		if err := json.Unmarshal([]byte(document), &parsed); err != nil {
			t.Fatalf("typed decode: %v", err)
		}
		draw := parsed.Data[0].poe()
		if !draw.measurable {
			t.Fatalf("the switch should be measurable, got %+v", draw)
		}
		if draw.budget != 195 {
			t.Errorf("budget = %v, want 195: total_max_power is the denominator", draw.budget)
		}
		// The uplink port reports 0.00 and the spare is not powered, so the
		// whole draw is the one port delivering anything.
		if draw.watts != 13.2 {
			t.Errorf("watts = %v, want 13.2", draw.watts)
		}
		if draw.enabled != 2 {
			t.Errorf("enabled = %d, want 2: poe_enable is what powering means to both readers", draw.enabled)
		}
	})

	t.Run("the write path guards it", func(t *testing.T) {
		var raw struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(document), &raw); err != nil {
			t.Fatalf("untyped decode: %v", err)
		}
		table := raw.Data[0][fieldPortTable]

		port, found := portByIndex(table, testAPPort)
		if !found {
			t.Fatal("the write path did not find port 7; the two readers disagree about port_idx")
		}
		spec := reactorv1alpha1.PoEPort{Device: testSwitchMAC, Port: testAPPort, PortName: "ap"}
		if err := checkPort(spec.Device, spec, port); err != nil {
			t.Errorf("the write path refused a port the state key measures: %v", err)
		}

		// And the two floors still hold on the same document, which is what
		// says the state key's decorations did not weaken the guard.
		uplink, found := portByIndex(table, 1)
		if !found {
			t.Fatal("the write path did not find port 1")
		}
		uplinkSpec := reactorv1alpha1.PoEPort{Device: spec.Device, Port: 1, PortName: "uplink"}
		if err := checkPort(uplinkSpec.Device, uplinkSpec, uplink); err == nil {
			t.Error("cycling the uplink should be refused whatever else the record says")
		}
		off, found := portByIndex(table, 9)
		if !found {
			t.Fatal("the write path did not find port 9")
		}
		offSpec := reactorv1alpha1.PoEPort{Device: spec.Device, Port: 9, PortName: "spare"}
		if err := checkPort(offSpec.Device, offSpec, off); err == nil {
			t.Error("a port whose power is switched off should be refused")
		}
	})
}

// poe_enable means the same thing to both readers, and this is the assertion
// that keeps it that way: the write path treats an explicit false as "nothing
// to cycle", and the state key treats it as "this port draws nothing". A change
// to either reading that did not change the other would show up here.
func TestPoweredMeansTheSameToBothReaders(t *testing.T) {
	const document = `{"data":[{
		"mac": "aa:bb:cc:00:11:33", "type": "usw", "name": "Switch", "state": 1, "adopted": true,
		"total_max_power": 60,
		"port_table": [
			{"port_idx": 3, "name": "camera", "is_uplink": false, "port_poe": true, "poe_enable": false, "poe_power": "0.00"}
		]
	}]}`

	var parsed deviceStatResponse
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if draw := parsed.Data[0].poe(); draw.enabled != 0 || draw.watts != 0 {
		t.Errorf("an unpowered port should contribute nothing, got %+v", draw)
	}

	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(document), &raw); err != nil {
		t.Fatalf("untyped decode: %v", err)
	}
	port, found := portByIndex(raw.Data[0][fieldPortTable], 3)
	if !found {
		t.Fatal("the write path did not find port 3")
	}
	spec := reactorv1alpha1.PoEPort{Device: testSwitchMAC, Port: 3, PortName: "camera"}
	if err := checkPort(spec.Device, spec, port); err == nil {
		t.Error("the write path should refuse a port whose power is already off")
	}
}
