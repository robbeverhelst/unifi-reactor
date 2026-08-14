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

// Package actions executes edge actions: the ones that express an occurrence
// rather than a level, and therefore leave the cluster.
//
// It is provider-agnostic by construction — nothing here knows what a state key
// means — and it is where Reactor's outbound reach is bounded. An operator that
// issues HTTP requests on demand is a confused deputy: it sits inside the
// cluster with a ServiceAccount and a network position, and spec.actions is
// writable by anyone who can create an Automation in their own namespace. So
// the destination is decided by the operator at install time, not by the
// Automation, and a floor of addresses is refused regardless.
package actions

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
	// allowAny is the allowlist entry that permits any host. It is an explicit,
	// documented choice to run without a host allowlist; it does not lift the
	// address floor in controlAddress.
	allowAny = "*"
	// wildcardPrefix starts an entry matching one level of subdomain, e.g.
	// "https://*.example.com".
	wildcardPrefix = "*."
)

// destination is one entry of the operator's allowlist.
type destination struct {
	scheme string
	// host is the literal host, or the suffix (including the leading dot) when
	// wildcard is set.
	host     string
	port     string
	wildcard bool
	any      bool
}

// Policy is the operator's statement about where Reactor may send a request.
//
// It is install-level configuration and deliberately absent from the Automation
// API. Anyone who can create an Automation in their own namespace can ask
// Reactor to make a request, and that request goes out with the operator's
// network position rather than the author's — reaching in-cluster Services, the
// API server, and anything else the pod can route to. Choosing which
// destinations that is worth is the operator's decision, and the default is
// none.
type Policy struct {
	allowed []destination
	// allowLoopback lifts the loopback rule for tests, which need a stub server
	// on 127.0.0.1. Unexported and set nowhere outside this package's own
	// tests, so no configuration can reach it.
	allowLoopback bool
}

// ParsePolicy reads an allowlist. Each entry is a scheme and host with an
// optional port — "https://ntfy.example.com", "http://hooks.example.com:8080",
// "https://*.example.com" — or the single entry "*" to allow any host.
//
// An empty list is not an error: it is the default, and it means every outbound
// action is refused.
func ParsePolicy(entries []string) (Policy, error) {
	var policy Policy
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == allowAny {
			policy.allowed = append(policy.allowed, destination{any: true})
			continue
		}
		parsed, err := parseDestination(entry)
		if err != nil {
			return Policy{}, err
		}
		policy.allowed = append(policy.allowed, parsed)
	}
	return policy, nil
}

// PolicyFromEnv reads the allowlist from a comma-separated environment
// variable. Lookup is injected so the mapping can be tested without mutating
// process state, matching how the UniFi provider is configured.
func PolicyFromEnv(lookup func(string) string) (Policy, error) {
	return ParsePolicy(strings.Split(lookup(EnvAllowedDestinations), ","))
}

// EnvAllowedDestinations is the environment variable the chart writes the
// allowlist into, named here so the contract with the chart is stated once.
const EnvAllowedDestinations = "REACTOR_ACTION_ALLOWED_DESTINATIONS"

func parseDestination(entry string) (destination, error) {
	parsed, err := url.Parse(entry)
	if err != nil {
		return destination{}, fmt.Errorf("allowed destination %q is not a URL: %w", entry, err)
	}
	if parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS {
		return destination{}, fmt.Errorf(
			"allowed destination %q needs an http or https scheme, e.g. https://ntfy.example.com", entry)
	}
	if parsed.Host == "" {
		return destination{}, fmt.Errorf("allowed destination %q names no host", entry)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return destination{}, fmt.Errorf(
			"allowed destination %q has a path; destinations are matched on scheme, host and port only", entry)
	}

	host, port := splitHostPort(parsed.Host, parsed.Scheme)
	host = strings.ToLower(host)
	if suffix, found := strings.CutPrefix(host, wildcardPrefix); found {
		return destination{scheme: parsed.Scheme, host: "." + suffix, port: port, wildcard: true}, nil
	}
	if strings.Contains(host, allowAny) {
		return destination{}, fmt.Errorf(
			"allowed destination %q may only use a wildcard as a leading %q label", entry, wildcardPrefix)
	}
	return destination{scheme: parsed.Scheme, host: host, port: port}, nil
}

// splitHostPort returns the host and the effective port, defaulting the port
// from the scheme. An entry that names no port therefore matches the scheme's
// default port only — a destination on an unusual port has to be written out,
// which is the conservative reading and one the error messages spell out.
func splitHostPort(hostport, scheme string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, defaultPort(scheme)
	}
	if port == "" {
		port = defaultPort(scheme)
	}
	return strings.Trim(host, "[]"), port
}

func defaultPort(scheme string) string {
	if scheme == schemeHTTPS {
		return "443"
	}
	return "80"
}

// endpointOn appends a fixed path to the base address of a service.
//
// It is what makes a named integration narrower than the http.request the same
// allowlist entry already permits: the caller supplies the path segments, the
// Automation supplies only the base, and the two are joined with escaping
// rather than concatenated. A base may carry a path — an instance behind a
// reverse proxy at /home-assistant is ordinary — but not a query or a fragment,
// which on something described as a base address is a sign it is being used to
// reach past the part of the URL Reactor controls.
//
// service names the integration for the error text, because "the url is not
// valid" is not worth reading when an Automation has two of them.
func endpointOn(base, service string, segments ...string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		// Deliberately not wrapping: a parse error quotes the whole input back,
		// and this text reaches the Automation's status.
		return "", fmt.Errorf("the %s url is not a valid URL", service)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf(
			"the %s url is the base address of the instance and takes no query or fragment; "+
				"Reactor appends the path itself", service)
	}
	return parsed.JoinPath(segments...).String(), nil
}

// Empty reports whether the policy allows nothing at all, which is the default
// and means outbound actions are switched off. Callers check it before
// resolving credentials, so a disabled install never reads a Secret it has no
// use for.
func (p Policy) Empty() bool {
	return len(p.allowed) == 0
}

// Check validates a destination URL and reports the origin — scheme, host and
// port — which is the only part of it that is ever safe to log, put in status,
// or attach to an Event. The path and query are left out on purpose: for every
// notification transport shipped, that is where the credential lives.
func (p Policy) Check(raw string) (*url.URL, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		// Deliberately not wrapping: a parse error quotes the whole input back.
		return nil, "", errors.New("destination is not a valid URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != schemeHTTP && scheme != schemeHTTPS {
		return nil, "", fmt.Errorf("destination scheme %q is not http or https", scheme)
	}
	if parsed.User != nil {
		return nil, "", errors.New("destination carries user information in the URL; use the Secret's authorization key")
	}
	if parsed.Host == "" {
		return nil, "", errors.New("destination names no host")
	}

	host, port := splitHostPort(parsed.Host, scheme)
	if host == "" {
		return nil, "", errors.New("destination names no host")
	}
	origin := fmt.Sprintf("%s://%s:%s", scheme, host, port)

	if p.Empty() {
		return nil, origin, fmt.Errorf(
			"outbound actions are disabled on this install: no destination is allowed, so %s was refused", origin)
	}
	for _, allowed := range p.allowed {
		if allowed.matches(scheme, host, port) {
			return parsed, origin, nil
		}
	}
	return nil, origin, fmt.Errorf(
		"destination %s is not allowed by this install; add it to the operator's allowed destinations", origin)
}

func (d destination) matches(scheme, host, port string) bool {
	if d.any {
		return true
	}
	if d.scheme != scheme || d.port != port {
		return false
	}
	if d.wildcard {
		return strings.HasSuffix(host, d.host) && len(host) > len(d.host)
	}
	return d.host == host
}

// controlAddress is the floor, applied to the address actually dialled rather
// than to the hostname. Checking here is what makes a DNS name that resolves to
// a blocked address — deliberately, as in a rebinding attack, or by accident —
// fail anyway, and it applies even to a host the allowlist permits.
//
// Private ranges are not blocked: reaching an ntfy box on the LAN is a first
// class use case for a homelab operator, and no rule here can tell that apart
// from a ClusterIP Service. The allowlist is what draws that line. What is
// blocked is the set of addresses no legitimate notification target has: the
// loopback interface, and link-local — which is where cloud instance metadata
// services, and the credentials they hand out, live.
func (p Policy) controlAddress(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("refusing to dial an unparsable address: %w", err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("refusing to dial %q, which is not an IP address", host)
	}
	addr = addr.Unmap()

	switch {
	case addr.IsLoopback():
		if p.allowLoopback {
			return nil
		}
		return blocked(addr, "the loopback interface")
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		return blocked(addr, "a link-local address, where instance metadata services live")
	case addr.IsUnspecified():
		return blocked(addr, "the unspecified address")
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast():
		return blocked(addr, "a multicast address")
	}
	return nil
}

func blocked(addr netip.Addr, what string) error {
	return fmt.Errorf("refusing to dial %s: it is %s", addr, what)
}
