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

package unifi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// What this provider has actually been run against. Everything here is a
// statement about observation, not about intent: the parsers were written
// against captures from exactly one console, and the compatibility matrix in
// the README says so in the same words.
const (
	// VerifiedNetworkVersion is the UniFi Network version every capture in
	// testdata/unifi/ came from.
	VerifiedNetworkVersion = "10.5.67"
	// VerifiedConsoleModel is the console those captures came from.
	VerifiedConsoleModel = "UDM Pro"

	// MinSupportedNetworkMajor and FirstUnsupportedNetworkMajor bound the
	// versions this provider expects to work on. They are wider than what has
	// been verified because the fields being read — wan1/wan2, vbms_table,
	// last_wan_status — are long-standing ones, and narrow enough that a major
	// version bump is treated as a reason to look.
	MinSupportedNetworkMajor     = 10
	FirstUnsupportedNetworkMajor = 11
)

// ControllerInfo is the subset of GET /proxy/network/integration/v1/info this
// provider reads, captured in testdata/unifi/api/integration-info.json.
type ControllerInfo struct {
	ApplicationVersion string `json:"applicationVersion"`
}

// Info reports what the console is running. It uses the Integration API, which
// accepts the same X-API-KEY header as the legacy API the poller uses, so it
// needs no extra credential.
func (c *Client) Info(ctx context.Context) (ControllerInfo, error) {
	url := c.baseURL + "/proxy/network/integration/v1/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ControllerInfo{}, err
	}
	apiKey, err := c.apiKey()
	if err != nil {
		return ControllerInfo{}, err
	}
	req.Header.Set("X-API-KEY", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return ControllerInfo{}, fmt.Errorf("reading unifi controller info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ControllerInfo{}, fmt.Errorf("reading unifi controller info: unexpected status %d", resp.StatusCode)
	}
	var info ControllerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ControllerInfo{}, fmt.Errorf("decoding unifi controller info: %w", err)
	}
	return info, nil
}

// versionVerdict is what the compatibility check concluded about a version.
type versionVerdict int

const (
	// versionUnknown means the console reported something unparseable — which
	// is not evidence of incompatibility, only of an unfamiliar format.
	versionUnknown versionVerdict = iota
	// versionSupported means inside the expected range.
	versionSupported
	// versionOlder and versionNewer mean outside it, in either direction.
	versionOlder
	versionNewer
)

// classifyVersion places a reported application version against the range this
// provider expects to work on. Only the major version decides: a minor bump
// that moves a field is exactly the case nothing can predict, which is what
// the log line is for.
func classifyVersion(version string) (versionVerdict, int) {
	major, ok := majorVersion(version)
	if !ok {
		return versionUnknown, 0
	}
	switch {
	case major < MinSupportedNetworkMajor:
		return versionOlder, major
	case major >= FirstUnsupportedNetworkMajor:
		return versionNewer, major
	default:
		return versionSupported, major
	}
}

// majorVersion reads the leading integer of a dotted version string, ignoring
// whatever a console appends after it.
func majorVersion(version string) (int, bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	if head == "" {
		return 0, false
	}
	major, err := strconv.Atoi(head)
	if err != nil || major < 0 {
		return 0, false
	}
	return major, true
}

// SupportedNetworkVersions renders the expected range for humans.
func SupportedNetworkVersions() string {
	return fmt.Sprintf("%d.x (verified on %s)", MinSupportedNetworkMajor, VerifiedNetworkVersion)
}

// versionCheckAttempts and versionCheckBackoff bound how hard the guard tries
// before giving up. A console is often unreachable for the first few seconds
// of a pod's life, and the whole value of this check is one accurate line in
// the startup logs.
const (
	versionCheckAttempts = 3
	versionCheckBackoff  = 10 * time.Second
)

// VersionGuard reports what the console is running, once, at startup.
//
// It never stops the operator. An unrecognised version is not evidence that
// anything is broken — most of them will work fine — and refusing to start
// against a console that would have worked is a worse failure than a log line
// nobody reads until something else goes wrong. What it buys is that when a
// field has moved, "no gateway reporting WAN ports" is preceded by "this
// console is newer than anything this was tested against", which turns an
// apparent misconfiguration into an apparent incompatibility.
type VersionGuard struct {
	Client *Client
	// Backoff is the wait between attempts; zero means versionCheckBackoff.
	Backoff time.Duration
}

// Start implements manager.Runnable. It always returns nil: see VersionGuard.
func (g *VersionGuard) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("unifi-version")
	backoff := g.Backoff
	if backoff <= 0 {
		backoff = versionCheckBackoff
	}

	var info ControllerInfo
	var err error
	for attempt := 1; ; attempt++ {
		if info, err = g.Client.Info(ctx); err == nil {
			break
		}
		if attempt >= versionCheckAttempts {
			log.V(1).Info("Could not determine the UniFi Network version; continuing",
				"attempts", attempt, "error", err.Error())
			return nil
		}
		if !waitOrDone(ctx, backoff) {
			return nil
		}
	}

	verdict, major := classifyVersion(info.ApplicationVersion)
	switch verdict {
	case versionSupported:
		log.Info("UniFi Network version detected",
			"version", info.ApplicationVersion, "verifiedAgainst", VerifiedNetworkVersion,
			"verifiedConsole", VerifiedConsoleModel)
	case versionOlder:
		log.Info("This UniFi Network version is older than anything Reactor has been tested against; "+
			"if state keys are missing, an incompatible API is the first thing to suspect",
			"version", info.ApplicationVersion, "supported", SupportedNetworkVersions())
	case versionNewer:
		log.Info("This UniFi Network version is newer than anything Reactor has been tested against; "+
			"if state keys are missing, an incompatible API is the first thing to suspect",
			"version", info.ApplicationVersion, "supported", SupportedNetworkVersions(), "major", major)
	case versionUnknown:
		log.Info("The UniFi console reported a version in an unfamiliar format; continuing",
			"version", info.ApplicationVersion, "supported", SupportedNetworkVersions())
	}
	return nil
}

// NeedLeaderElection reports false so every replica logs what it is talking
// to. A standby that never becomes leader is exactly the pod whose logs
// someone reads when the leader is misbehaving.
func (g *VersionGuard) NeedLeaderElection() bool { return false }

// waitOrDone sleeps out d, reporting false if the context ended first.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
