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

package main

import (
	"maps"
	"testing"
)

func TestUniFiDebounceDefaultsToImmediate(t *testing.T) {
	config, err := unifiDebounce()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Default != 1 {
		t.Fatalf("default = %d, want 1 so an unconfigured install reacts on the first observation", config.Default)
	}
	if len(config.PerKey) != 0 {
		t.Fatalf("perKey = %v, want empty", config.PerKey)
	}
}

// The chart renders this format, so it is the contract between the two.
func TestUniFiDebounceParsesChartFormat(t *testing.T) {
	t.Setenv("UNIFI_DEBOUNCE_DEFAULT", "2")
	t.Setenv("UNIFI_DEBOUNCE_KEYS", "alpha=3,ups.battery=2,zeta=5")

	config, err := unifiDebounce()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Default != 2 {
		t.Fatalf("default = %d, want 2", config.Default)
	}
	want := map[string]int{"alpha": 3, "ups.battery": 2, "zeta": 5}
	if !maps.Equal(config.PerKey, want) {
		t.Fatalf("perKey = %v, want %v", config.PerKey, want)
	}
}

func TestUniFiDebounceRejectsMalformedConfiguration(t *testing.T) {
	for name, value := range map[string]string{
		"missing separator": "ups.battery",
		"non-numeric":       "ups.battery=often",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("UNIFI_DEBOUNCE_KEYS", value)
			if _, err := unifiDebounce(); err == nil {
				t.Fatal("expected an error rather than silently ignoring the setting")
			}
		})
	}
}
