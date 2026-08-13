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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newLocalListener reserves an ephemeral loopback port so a test knows where
// the receiver will be before it starts.
func newLocalListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// waitForListener blocks until the address accepts connections.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing started listening on %s", addr)
}

const (
	testToken      = "s3cr3t-shared-token"
	testCustomPath = "/hooks/reactor"
)

func testReceiver() *Receiver {
	return NewReceiver(WebhookConfig{Enabled: true, Token: testToken})
}

// deliver posts a body to the receiver with the given headers and reports the
// status and whether a re-observation ended up pending.
func deliver(t *testing.T, r *Receiver, method, path, body string, headers map[string]string) (int, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)

	select {
	case <-r.Requests():
		return rec.Code, true
	default:
		return rec.Code, false
	}
}

func authorized() map[string]string {
	return map[string]string{authorizationHeader: bearerPrefix + testToken}
}

func TestReceiverAcceptsAnAuthenticatedDelivery(t *testing.T) {
	code, requested := deliver(t, testReceiver(), http.MethodPost, DefaultWebhookPath, `{"alarm":"whatever"}`, authorized())
	if code != http.StatusAccepted {
		t.Errorf("expected 202 Accepted, got %d", code)
	}
	if !requested {
		t.Error("expected a re-observation to be pending")
	}
}

// The alternative header exists for a rule configured by hand in the UniFi UI,
// where the Alarm Manager's custom-headers list is the available mechanism.
func TestReceiverAcceptsTheAlternativeTokenHeader(t *testing.T) {
	code, requested := deliver(t, testReceiver(), http.MethodPost, DefaultWebhookPath, "{}",
		map[string]string{tokenHeader: testToken})
	if code != http.StatusAccepted || !requested {
		t.Errorf("expected an accepted delivery, got status %d, requested %v", code, requested)
	}
}

// The payload is never read, so the receiver has no opinion about it. This is
// the property that makes a forged delivery harmless: there is nothing in one
// that Reactor can be told, only that it should look at the console again.
func TestReceiverNeverJudgesTheBody(t *testing.T) {
	for name, body := range map[string]string{
		"empty body":   "",
		"not json":     "<xml>this is not what the console sends</xml>",
		"truncated":    `{"alarm":`,
		"json null":    "null",
		"oversize":     strings.Repeat("x", maxDeliveryBytes*2),
		"nested state": `{"state":{"wan":"backup"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, requested := deliver(t, testReceiver(), http.MethodPost, DefaultWebhookPath, body, authorized())
			if code != http.StatusAccepted || !requested {
				t.Errorf("expected an accepted delivery, got status %d, requested %v", code, requested)
			}
		})
	}
}

func TestReceiverRejectsUnauthenticatedDeliveries(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"no credentials":    {},
		"wrong token":       {authorizationHeader: bearerPrefix + "not-the-token"},
		"wrong scheme":      {authorizationHeader: "Basic " + testToken},
		"token prefix only": {authorizationHeader: bearerPrefix + testToken[:8]},
		"wrong header":      {"X-Api-Key": testToken},
		"empty bearer":      {authorizationHeader: bearerPrefix},
	} {
		t.Run(name, func(t *testing.T) {
			code, requested := deliver(t, testReceiver(), http.MethodPost, DefaultWebhookPath, "{}", headers)
			if code != http.StatusUnauthorized {
				t.Errorf("expected 401 Unauthorized, got %d", code)
			}
			if requested {
				t.Error("an unauthenticated delivery must not cause an observation")
			}
		})
	}
}

// A receiver with no secret configured must be inert rather than open.
func TestReceiverWithoutATokenAcceptsNothing(t *testing.T) {
	open := NewReceiver(WebhookConfig{Enabled: true})
	for _, headers := range []map[string]string{{}, {authorizationHeader: bearerPrefix}, {tokenHeader: ""}} {
		code, requested := deliver(t, open, http.MethodPost, DefaultWebhookPath, "{}", headers)
		if code != http.StatusUnauthorized || requested {
			t.Errorf("expected 401 and no observation, got status %d, requested %v", code, requested)
		}
	}
}

func TestReceiverRejectsOtherMethodsAndPaths(t *testing.T) {
	for name, request := range map[string]struct {
		method, path string
		want         int
	}{
		"GET on the delivery path": {http.MethodGet, DefaultWebhookPath, http.StatusMethodNotAllowed},
		"PUT on the delivery path": {http.MethodPut, DefaultWebhookPath, http.StatusMethodNotAllowed},
		"POST elsewhere":           {http.MethodPost, "/", http.StatusNotFound},
		"POST to a sibling path":   {http.MethodPost, "/webhooks/unifi/extra", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			code, requested := deliver(t, testReceiver(), request.method, request.path, "{}", authorized())
			if code != request.want {
				t.Errorf("expected status %d, got %d", request.want, code)
			}
			if requested {
				t.Error("expected no observation to be requested")
			}
		})
	}
}

// A burst must cost one observation, not one per delivery: a real outage fires
// several triggers at once, a retrying console repeats them, and someone who
// found the endpoint can repeat them as fast as they like.
func TestReceiverCoalescesABurstIntoOneObservation(t *testing.T) {
	r := testReceiver()
	for range 500 {
		req := httptest.NewRequest(http.MethodPost, DefaultWebhookPath, strings.NewReader("{}"))
		req.Header.Set(authorizationHeader, bearerPrefix+testToken)
		r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	pending := 0
	for {
		select {
		case <-r.Requests():
			pending++
			continue
		default:
		}
		break
	}
	if pending != 1 {
		t.Errorf("expected 500 deliveries to collapse into 1 pending observation, got %d", pending)
	}
}

// A custom path must be honoured exactly, and must not leave the default one
// answering as well.
func TestReceiverHonoursACustomPath(t *testing.T) {
	r := NewReceiver(WebhookConfig{Enabled: true, Token: testToken, Path: testCustomPath})
	if code, requested := deliver(t, r, http.MethodPost, testCustomPath, "{}", authorized()); code != http.StatusAccepted || !requested {
		t.Errorf("expected the custom path to accept, got status %d, requested %v", code, requested)
	}
	if code, _ := deliver(t, r, http.MethodPost, DefaultWebhookPath, "{}", authorized()); code != http.StatusNotFound {
		t.Errorf("expected the default path to be absent, got status %d", code)
	}
}

func TestNewReceiverFallsBackToDefaults(t *testing.T) {
	r := NewReceiver(WebhookConfig{Enabled: true, Token: testToken})
	if r.Addr != DefaultWebhookBindAddress || r.Path != DefaultWebhookPath {
		t.Errorf("expected defaults, got address %q path %q", r.Addr, r.Path)
	}
}

// Start must never report an error, however badly it goes: the manager stops
// on a Runnable error, and stopping the manager would stop the poller.
func TestReceiverStartReportsNoErrorWhenItCannotListen(t *testing.T) {
	r := NewReceiver(WebhookConfig{Enabled: true, Token: testToken, BindAddress: "127.0.0.1:-1"})
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("a receiver that cannot listen must not fail the manager: %v", err)
	}
}

func TestReceiverStartServesAndShutsDownWithTheContext(t *testing.T) {
	r := NewReceiver(WebhookConfig{Enabled: true, Token: testToken, BindAddress: "127.0.0.1:0"})
	// Bind an ephemeral port by hand so the test knows where to reach it.
	listener, err := newLocalListener()
	if err != nil {
		t.Fatalf("reserving a local port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	r.Addr = addr

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- r.Start(ctx) }()

	waitForListener(t, addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+DefaultWebhookPath, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building the delivery: %v", err)
	}
	req.Header.Set(authorizationHeader, bearerPrefix+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delivering over the wire: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 Accepted over the wire, got %d", resp.StatusCode)
	}
	select {
	case <-r.Requests():
	default:
		t.Error("expected a re-observation to be pending after a real delivery")
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("expected a clean shutdown, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the receiver did not stop when its context was cancelled")
	}
}
