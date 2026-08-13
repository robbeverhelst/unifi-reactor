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

const testSecret = "ntfy-credentials"

func TestCredentialsFromReadsTheRecognisedKeys(t *testing.T) {
	credentials, err := CredentialsFrom(testSecret, map[string][]byte{
		SecretKeyURL:           []byte(ntfyHost + secretPath + "\n"),
		SecretKeyAuthorization: []byte("Bearer tk_example"),
		"header-X-Api-Key":     []byte("example-key"),
		"unrelated":            []byte("ignored"),
	})
	if err != nil {
		t.Fatalf("CredentialsFrom = %v", err)
	}
	if credentials.URL != ntfyHost+secretPath {
		t.Fatalf("URL = %q, want the trailing newline trimmed", credentials.URL)
	}
	if credentials.Header.Get("Authorization") != "Bearer tk_example" {
		t.Fatalf("Authorization = %q", credentials.Header.Get("Authorization"))
	}
	if credentials.Header.Get("X-Api-Key") != "example-key" {
		t.Fatalf("X-Api-Key = %q", credentials.Header.Get("X-Api-Key"))
	}
	if len(credentials.Header) != 2 {
		t.Fatalf("header = %v, want only the two credential headers", credentials.Header)
	}
}

func TestCredentialsFromRejectsUnusableValues(t *testing.T) {
	cases := map[string]map[string][]byte{
		"an empty authorization":    {SecretKeyAuthorization: []byte("")},
		"a line break in a header":  {SecretKeyAuthorization: []byte("Bearer a\r\nX-Injected: 1")},
		"a header name that is not": {"header-bad name": []byte("x")},
		"an empty header name":      {"header-": []byte("x")},
	}
	for what, data := range cases {
		if _, err := CredentialsFrom(testSecret, data); err == nil {
			t.Errorf("CredentialsFrom with %s succeeded, want an error", what)
		}
	}
}

func TestCredentialErrorsNameTheKeyAndNotTheValue(t *testing.T) {
	const credential = "Bearer tk_the_actual_secret\r\nX-Injected: 1"
	_, err := CredentialsFrom(testSecret, map[string][]byte{SecretKeyAuthorization: []byte(credential)})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "tk_the_actual_secret") {
		t.Fatalf("the error quoted the credential: %v", err)
	}
	if !strings.Contains(err.Error(), SecretKeyAuthorization) {
		t.Fatalf("the error should name the key at fault: %v", err)
	}
}

func TestNoSecretYieldsNoCredentials(t *testing.T) {
	credentials, err := CredentialsFrom(testSecret, nil)
	if err != nil {
		t.Fatalf("CredentialsFrom = %v", err)
	}
	if credentials.URL != "" || len(credentials.Header) != 0 {
		t.Fatalf("credentials = %+v, want empty", credentials)
	}
}
