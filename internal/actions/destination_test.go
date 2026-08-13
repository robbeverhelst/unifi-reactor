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

const (
	ntfyHost   = "https://ntfy.example.com"
	secretPath = "/topic-that-is-the-credential"
)

func mustPolicy(t *testing.T, entries ...string) Policy {
	t.Helper()
	policy, err := ParsePolicy(entries)
	if err != nil {
		t.Fatalf("ParsePolicy(%v) = %v", entries, err)
	}
	return policy
}

func TestEmptyPolicyRefusesEverything(t *testing.T) {
	var policy Policy
	if !policy.Empty() {
		t.Fatal("a policy with no entries must report Empty")
	}
	_, origin, err := policy.Check(ntfyHost + secretPath)
	if err == nil {
		t.Fatal("an empty allowlist must refuse every destination")
	}
	if origin != "https://ntfy.example.com:443" {
		t.Fatalf("origin = %q, want the scheme, host and port", origin)
	}
	if strings.Contains(err.Error(), secretPath) {
		t.Fatalf("the refusal quoted the URL path, which is the credential: %v", err)
	}
}

func TestPolicyMatchesSchemeHostAndPort(t *testing.T) {
	policy := mustPolicy(t, ntfyHost, "http://hooks.example.com:8080")

	allowed := []string{
		ntfyHost + secretPath,
		"https://ntfy.example.com:443/other",
		"http://hooks.example.com:8080/path",
	}
	for _, raw := range allowed {
		if _, _, err := policy.Check(raw); err != nil {
			t.Errorf("Check(%q) = %v, want allowed", raw, err)
		}
	}

	// A different scheme, host or port is a different destination. The port
	// especially: an entry with no port means the scheme's default only.
	refused := []string{
		"http://ntfy.example.com/topic",
		"https://ntfy.example.com:8443/topic",
		"https://other.example.com/topic",
		"http://hooks.example.com/path",
	}
	for _, raw := range refused {
		if _, _, err := policy.Check(raw); err == nil {
			t.Errorf("Check(%q) was allowed, want refused", raw)
		}
	}
}

func TestPolicyWildcardMatchesOneLabelDeep(t *testing.T) {
	policy := mustPolicy(t, "https://*.example.com")

	if _, _, err := policy.Check("https://ntfy.example.com/topic"); err != nil {
		t.Errorf("a subdomain must match a wildcard entry: %v", err)
	}
	// The bare domain is not a subdomain of itself, and a lookalike domain that
	// merely ends in the same characters must not match either.
	for _, raw := range []string{"https://example.com/x", "https://evilexample.com/x"} {
		if _, _, err := policy.Check(raw); err == nil {
			t.Errorf("Check(%q) was allowed by a wildcard entry, want refused", raw)
		}
	}
}

func TestPolicyRejectsNonHTTPSchemesAndUserInfo(t *testing.T) {
	policy := mustPolicy(t, allowAny)

	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://example.com:70/_x",
		"https://user:password@example.com/x",
	} {
		if _, _, err := policy.Check(raw); err == nil {
			t.Errorf("Check(%q) was allowed, want refused", raw)
		}
	}
}

func TestParsePolicyRejectsUnusableEntries(t *testing.T) {
	for _, entry := range []string{
		"ntfy.example.com",               // no scheme
		"ftp://ntfy.example.com",         // wrong scheme
		"https://",                       // no host
		"https://ntfy.example.com/topic", // a path is not part of a destination
		"https://ntfy.*.example.com",     // wildcard anywhere but the leading label
	} {
		if _, err := ParsePolicy([]string{entry}); err == nil {
			t.Errorf("ParsePolicy(%q) succeeded, want an error", entry)
		}
	}
}

func TestAddressFloorHoldsEvenForAnAllowedHost(t *testing.T) {
	// "*" is the widest the allowlist goes, and it still does not reach these.
	policy := mustPolicy(t, allowAny)

	blocked := map[string]string{
		"127.0.0.1:80":          "loopback",
		"[::1]:443":             "IPv6 loopback",
		"169.254.169.254:80":    "the cloud instance metadata address",
		"[fe80::1]:80":          "IPv6 link-local",
		"[::ffff:127.0.0.1]:80": "an IPv4-mapped loopback address",
		"0.0.0.0:80":            "the unspecified address",
		"224.0.0.1:80":          "multicast",
	}
	for address, what := range blocked {
		if err := policy.controlAddress("tcp", address, nil); err == nil {
			t.Errorf("dialling %s (%s) was allowed, want refused", address, what)
		}
	}

	// A routable address is fine, including a private one: an ntfy box on the
	// LAN is a first-class destination for a homelab.
	for _, address := range []string{"192.0.2.10:443", "172.16.0.10:80"} {
		if err := policy.controlAddress("tcp", address, nil); err != nil {
			t.Errorf("dialling %s = %v, want allowed", address, err)
		}
	}
}

func TestPolicyFromEnvSplitsAndTrims(t *testing.T) {
	policy, err := PolicyFromEnv(func(name string) string {
		if name != EnvAllowedDestinations {
			return ""
		}
		return " https://ntfy.example.com , https://discord.com "
	})
	if err != nil {
		t.Fatalf("PolicyFromEnv = %v", err)
	}
	if _, _, err := policy.Check("https://discord.com/api/webhooks/1/token"); err != nil {
		t.Errorf("Check = %v, want allowed", err)
	}
}

func TestPolicyFromEnvUnsetIsEmpty(t *testing.T) {
	policy, err := PolicyFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("PolicyFromEnv = %v", err)
	}
	if !policy.Empty() {
		t.Fatal("an unset environment variable must leave outbound actions switched off")
	}
}
