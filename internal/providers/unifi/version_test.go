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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveInfo answers the Integration API's info endpoint with body, and records
// whether the API key reached it.
func serveInfo(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/proxy/network/integration/v1/info" {
			t.Errorf("unexpected path %q", got)
		}
		if got := r.Header.Get("X-API-KEY"); got != "test-key" {
			t.Errorf("expected the poller's API key to work here too, got %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, StaticAPIKey("test-key"), "", false)
}

// The captured response is the whole contract: one field, read with the same
// credential the poller already has.
func TestInfoAgainstTheCapturedResponse(t *testing.T) {
	c := serveInfo(t, http.StatusOK, string(captured(t, "integration-info.json")))

	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ApplicationVersion != VerifiedNetworkVersion {
		t.Errorf("applicationVersion = %q, want %q (the version the captures came from)",
			info.ApplicationVersion, VerifiedNetworkVersion)
	}
}

func TestClassifyVersion(t *testing.T) {
	tests := []struct {
		version string
		want    versionVerdict
	}{
		{VerifiedNetworkVersion, versionSupported},
		{"10.0.0", versionSupported},
		{"10.12.4", versionSupported},
		{"9.3.45", versionOlder},
		{"7.5.187", versionOlder},
		{"11.0.0", versionNewer},
		{"12.1.0", versionNewer},
		// A console that answers with something unexpected is not a console
		// that is known to be broken.
		{"", versionUnknown},
		{"unreleased", versionUnknown},
		{"v10.5.67", versionUnknown},
	}
	for _, tc := range tests {
		if got, _ := classifyVersion(tc.version); got != tc.want {
			t.Errorf("classifyVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// The guard is informative, never fatal: an unrecognised console is not
// evidence that anything is broken, and refusing to start against one that
// would have worked is the worse failure.
func TestVersionGuardNeverStopsTheOperator(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantLogged string
	}{
		{
			name:       "the tested version",
			status:     http.StatusOK,
			body:       `{"applicationVersion":"` + VerifiedNetworkVersion + `"}`,
			wantLogged: "UniFi Network version detected",
		},
		{
			name:       "an older major version",
			status:     http.StatusOK,
			body:       `{"applicationVersion":"9.3.45"}`,
			wantLogged: "older than anything Reactor has been tested against",
		},
		{
			name:       "a newer major version",
			status:     http.StatusOK,
			body:       `{"applicationVersion":"11.0.0"}`,
			wantLogged: "newer than anything Reactor has been tested against",
		},
		{
			name:       "a version in an unfamiliar format",
			status:     http.StatusOK,
			body:       `{"applicationVersion":"unreleased"}`,
			wantLogged: "unfamiliar format",
		},
		{
			// An endpoint that is not there at all is the case a UDR or a
			// Cloud Key on an older Network release might present.
			name:       "an endpoint that is not there",
			status:     http.StatusNotFound,
			body:       "",
			wantLogged: "Could not determine the UniFi Network version",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, logs := logged(t)
			guard := &VersionGuard{Client: serveInfo(t, tc.status, tc.body), Backoff: time.Millisecond}

			if err := guard.Start(ctx); err != nil {
				t.Fatalf("the guard must never stop the manager, got: %v", err)
			}
			if !strings.Contains(logs(), tc.wantLogged) {
				t.Errorf("expected the guard to report %q, logged:\n%s", tc.wantLogged, logs())
			}
		})
	}
}

// A console that is unreachable when the pod starts is the ordinary case, not
// a failure — the guard retries, then gives up quietly and leaves polling to
// report reachability.
func TestVersionGuardGivesUpQuietlyOnAnUnreachableConsole(t *testing.T) {
	ctx, logs := logged(t)
	guard := &VersionGuard{
		// A port nothing is listening on: the request fails at dial.
		Client:  NewClient("http://127.0.0.1:1", StaticAPIKey("test-key"), "", false),
		Backoff: time.Millisecond,
	}

	if err := guard.Start(ctx); err != nil {
		t.Fatalf("the guard must never stop the manager, got: %v", err)
	}
	if !strings.Contains(logs(), "Could not determine the UniFi Network version") {
		t.Errorf("expected the guard to say it gave up, logged:\n%s", logs())
	}
	if guard.NeedLeaderElection() {
		t.Error("every replica should log what console it is talking to, not just the leader")
	}
}

// The error raised when nothing is observable has to point at incompatibility,
// because that is what it usually means once the credentials are right.
func TestNothingObservableNamesTheTestedVersion(t *testing.T) {
	c := NewClient("", nil, "", false)
	_, err := c.stateFromDevices(context.Background(), deviceStatResponse{})
	if err == nil {
		t.Fatal("expected an error for an empty device list")
	}
	for _, want := range []string{VerifiedNetworkVersion, VerifiedConsoleModel, "compatibility matrix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got: %v", want, err)
		}
	}
}
