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

// Package unifi will hold the UniFi provider: the WAN state poller (source of
// truth) and the webhook receiver (fast-path re-observation trigger).
//
// INTENTIONALLY EMPTY until the v0.0 spike completes: the parser and observer
// must be written against the real captured payloads in testdata/unifi/ —
// never against assumed formats. API auth uses the X-API-KEY header, which
// works on both the Integration API (/proxy/network/integration/v1/...) and
// the legacy API (/proxy/network/api/s/<site>/...) as of Network 10.5.
package unifi
