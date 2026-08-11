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

// Package events defines the normalized event and state models every provider
// converts into. The engine only ever sees these types; provider-specific
// payloads stay behind the Payload field.
package events

import "time"

// Event is a normalized point-in-time occurrence, e.g. "client.connected".
// Events cannot be re-observed later; anything with an observable current
// value must be modeled as state instead.
type Event struct {
	// ID uniquely identifies the event for deduplication.
	ID string
	// Provider that emitted the event, e.g. "unifi".
	Provider string
	// Type is the normalized event type, e.g. "wan.failover".
	Type string
	// Timestamp is when the event occurred.
	Timestamp time.Time
	// Payload carries provider-specific data.
	Payload map[string]any
	// Metadata carries transport details (delivery, source address, ...).
	Metadata map[string]string
}

// Observation is a provider's full observed state at a point in time, e.g.
// {"wan": "backup"}. The current observation always wins; missed intermediate
// transitions are acceptable by design.
type Observation struct {
	Provider   string
	State      map[string]string
	ObservedAt time.Time
}
