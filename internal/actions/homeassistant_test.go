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

package actions

import (
	"strings"
	"testing"
)

// homeAssistantBase is the instance every test here addresses. It resolves
// nowhere and is never dialled: these tests build URLs and bodies.
const homeAssistantBase = "https://home-assistant.example.com"

func TestHomeAssistantURLIsTheServiceEndpoint(t *testing.T) {
	got, err := HomeAssistantURL(homeAssistantBase, "light", "turn_on")
	if err != nil {
		t.Fatalf("HomeAssistantURL = %v", err)
	}
	if want := homeAssistantBase + "/api/services/light/turn_on"; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestHomeAssistantURLKeepsABasePath(t *testing.T) {
	got, err := HomeAssistantURL(homeAssistantBase+"/home-assistant", "script", "turn_on")
	if err != nil {
		t.Fatalf("HomeAssistantURL = %v", err)
	}
	want := homeAssistantBase + "/home-assistant/api/services/script/turn_on"
	if got != want {
		t.Fatalf("url = %q, want %q: an instance behind a reverse proxy keeps its prefix", got, want)
	}
}

// The domain and the service are the only part of the path an Automation
// chooses, so they are the only place a path traversal could start. This is the
// test that keeps "call a service" from becoming "request anything on an
// allowed host" — which is a real action with a real name, and this is not it.
func TestHomeAssistantURLRefusesAnythingThatIsNotASlug(t *testing.T) {
	for _, name := range []string{
		"../../auth/token",
		"light/turn_on",
		"light?x=1",
		"light#x",
		"Light",
		"light on",
		"",
		strings.Repeat("a", maxHomeAssistantName+1),
	} {
		if _, err := HomeAssistantURL(homeAssistantBase, name, "turn_on"); err == nil {
			t.Fatalf("domain %q was accepted", name)
		}
		if _, err := HomeAssistantURL(homeAssistantBase, "light", name); err == nil {
			t.Fatalf("service %q was accepted", name)
		}
	}
}

func TestHomeAssistantURLRefusesAQueryOnTheBase(t *testing.T) {
	for _, base := range []string{homeAssistantBase + "?token=example", homeAssistantBase + "#fragment"} {
		if _, err := HomeAssistantURL(base, "light", "turn_on"); err == nil {
			t.Fatalf("base %q was accepted", base)
		}
	}
}

func TestHomeAssistantPayloadSendsTheRenderedObject(t *testing.T) {
	body := `{"entity_id": "light.hall", "brightness": 128}`
	payload, err := HomeAssistantPayload(body)
	if err != nil {
		t.Fatalf("HomeAssistantPayload = %v", err)
	}
	if string(payload.Body) != body {
		t.Fatalf("body = %q, want it sent as written", payload.Body)
	}
	if payload.Header.Get(headerContentType) != contentTypeJSON {
		t.Fatalf("content type = %q", payload.Header.Get(headerContentType))
	}
}

func TestHomeAssistantPayloadDefaultsToAnEmptyObject(t *testing.T) {
	payload, err := HomeAssistantPayload("  ")
	if err != nil {
		t.Fatalf("HomeAssistantPayload = %v", err)
	}
	if string(payload.Body) != "{}" {
		t.Fatalf("body = %q, want an empty object", payload.Body)
	}
}

// A template that rendered to something other than an object is caught here
// rather than by Home Assistant, which would report it as a 400 with nothing
// naming the Automation that produced it.
func TestHomeAssistantPayloadRefusesAnythingButAnObject(t *testing.T) {
	for _, data := range []string{
		`["light.hall"]`,
		`"light.hall"`,
		`null`,
		`{"entity_id": }`,
		`{"a":1}{"b":2}`,
	} {
		if _, err := HomeAssistantPayload(data); err == nil {
			t.Fatalf("data %q was accepted", data)
		}
	}
}
