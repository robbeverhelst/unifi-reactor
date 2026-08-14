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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// homeAssistantNameChars is the character set a service domain and a service
// name may use. Home Assistant's own slug rules are narrower than this, and
// that is the point: these two values are the only part of the request path an
// Automation gets to choose, so they are restricted to something that cannot
// contain a slash, a dot-dot, a query or an escape.
//
// Without that, "call a service" would quietly become "make any request to an
// allowed host" — which is a real action with a real name, http.request, and it
// is subject to exactly the same allowlist. The difference is that an
// http.request says what it is.
const homeAssistantNameChars = "abcdefghijklmnopqrstuvwxyz0123456789_"

// maxHomeAssistantName bounds a domain or service name. It matches the CRD's
// own limit, and exists here too because this package is where the URL is
// actually built.
const maxHomeAssistantName = 64

// homeAssistantName is what the integration is called in an error message.
const homeAssistantName = "Home Assistant"

// HomeAssistantURL builds the endpoint one service call goes to, from the base
// address of an instance and the service being called. Only the last two path
// segments come from the Automation, and only after they are checked.
func HomeAssistantURL(base, domain, service string) (string, error) {
	if err := usableHomeAssistantName("domain", domain); err != nil {
		return "", err
	}
	if err := usableHomeAssistantName("service", service); err != nil {
		return "", err
	}
	return endpointOn(base, homeAssistantName, "api", "services", domain, service)
}

// usableHomeAssistantName rejects anything that is not a bare slug.
func usableHomeAssistantName(field, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("homeAssistant.%s is empty", field)
	case len(value) > maxHomeAssistantName:
		return fmt.Errorf("homeAssistant.%s is longer than %d characters", field, maxHomeAssistantName)
	case strings.ContainsFunc(value, func(r rune) bool {
		return !strings.ContainsRune(homeAssistantNameChars, r)
	}):
		return fmt.Errorf(
			"homeAssistant.%s may only use lowercase letters, digits and underscores, e.g. turn_on", field)
	}
	return nil
}

// HomeAssistantPayload turns rendered service data into the request body.
//
// Home Assistant reads the body as the service's data, so it has to be a JSON
// object. It is parsed here rather than posted blind because a template that
// rendered to something else — an unquoted state value, a truncated body, an
// empty string where an object was meant — would otherwise be reported by Home
// Assistant as a 400 with no indication of which Automation produced it.
//
// The rendered text is sent as written rather than re-encoded, so a body stays
// byte-for-byte what the author wrote.
func HomeAssistantPayload(data string) (Payload, error) {
	header := http.Header{}
	header.Set(headerContentType, contentTypeJSON)

	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		// A service with no data is ordinary — script.turn_on with the entity in
		// the service name, homeassistant.restart — and Home Assistant wants an
		// object rather than an empty body.
		return Payload{Body: []byte("{}"), Header: header}, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil || fields == nil {
		return Payload{}, errors.New(
			`homeAssistant.data did not render to a JSON object, ` +
				`which is what Home Assistant reads as service data, e.g. {"entity_id": "light.hall"}`)
	}
	return Payload{Body: []byte(trimmed), Header: header}, nil
}
