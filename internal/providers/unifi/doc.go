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

// Package unifi holds the UniFi provider: the state observer (the mechanism of
// record) and the webhook fast path in front of it.
//
// The two halves are deliberately asymmetric. Client.Observe reads the console
// and produces every value the engine ever sees; its parsers are written
// against the real captured payloads in testdata/unifi/, never against assumed
// formats. Receiver reads nothing at all — a delivery is a hint that an
// observation is worth doing early, and the observation decides the rest. That
// is what keeps a dropped, duplicated, replayed or forged delivery from being
// able to strand the cluster in a state the console does not report.
//
// The two halves also authenticate differently. Observation uses the X-API-KEY
// header, which works on both the Integration API
// (/proxy/network/integration/v1/...) and the legacy API
// (/proxy/network/api/s/<site>/...) as of Network 10.5. The Alarm Manager API
// used for optional self-registration sits at the UniFi OS layer, rejects that
// header, and needs a cookie session plus a CSRF token — see alarm.go and
// docs/unifi-alarm-manager-api.md, which is reverse-engineered and version
// fragile.
package unifi
