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
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// The keys an action credential Secret may hold. Anything else in the Secret is
// ignored rather than rejected, so one Secret can carry unrelated data.
const (
	// SecretKeyURL holds the full destination URL. For every notification
	// transport this is the credential — a Discord webhook URL, an ntfy topic —
	// which is why notifications take it from here and nowhere else.
	SecretKeyURL = "url"
	// SecretKeyAuthorization is sent verbatim as the Authorization header,
	// e.g. "Bearer tk_example".
	SecretKeyAuthorization = "authorization"
	// SecretKeyHeaderPrefix marks a key whose remainder is a header name, for
	// the services that want their own, e.g. "header-X-Api-Key".
	SecretKeyHeaderPrefix = "header-"
)

// headerNameChars is the token character set a header name may use, per
// RFC 9110. Validating it here means a malformed Secret is rejected with an
// explanation rather than by net/http with a message quoting the value.
const headerNameChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~"

// Credentials is what one Secret contributes to a request.
type Credentials struct {
	// URL is the destination from the Secret, empty when the Secret has none.
	URL string
	// Header carries the credential headers.
	Header http.Header
}

// CredentialsFrom reads the recognised keys out of a Secret's data.
//
// Errors here name the Secret key at fault and never its value, because this
// error text ends up in the Automation's status where anyone who can read the
// resource can see it — which is a wider audience than can read the Secret.
func CredentialsFrom(secretName string, data map[string][]byte) (Credentials, error) {
	credentials := Credentials{Header: http.Header{}}

	// Sorted so a Secret with several header keys always produces the same
	// request, whatever order the map iterates in.
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	for _, key := range keys {
		value := strings.TrimSpace(string(data[key]))
		switch {
		case key == SecretKeyURL:
			credentials.URL = value
		case key == SecretKeyAuthorization:
			if err := usableHeaderValue(secretName, key, value); err != nil {
				return Credentials{}, err
			}
			credentials.Header.Set("Authorization", value)
		case strings.HasPrefix(key, SecretKeyHeaderPrefix):
			name := strings.TrimPrefix(key, SecretKeyHeaderPrefix)
			if name == "" || strings.ContainsFunc(name, func(r rune) bool {
				return !strings.ContainsRune(headerNameChars, r)
			}) {
				return Credentials{}, fmt.Errorf(
					"secret %q key %q does not name a usable header", secretName, key)
			}
			if err := usableHeaderValue(secretName, key, value); err != nil {
				return Credentials{}, err
			}
			credentials.Header.Set(name, value)
		}
	}
	return credentials, nil
}

// usableHeaderValue rejects the values net/http would refuse, and the empty one
// — a Secret key that exists but is blank is a deployment mistake worth naming
// rather than a header worth sending.
func usableHeaderValue(secretName, key, value string) error {
	if value == "" {
		return fmt.Errorf("secret %q key %q is empty", secretName, key)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("secret %q key %q contains a line break and cannot be sent as a header", secretName, key)
	}
	return nil
}
