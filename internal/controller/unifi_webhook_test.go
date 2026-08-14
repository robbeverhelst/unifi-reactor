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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
	"github.com/robbeverhelst/unifi-reactor/internal/providers/unifi"
)

const (
	webhookTestToken = "s3cr3t-shared-token"
	// The value the fake console reports once reportBackup has been called.
	wanBackupValue = "backup"
)

// fakeConsole serves the real captured device payload and counts how often it
// is asked for it, which is the only thing these tests measure: whether an
// observation happened, and when.
type fakeConsole struct {
	mu          sync.Mutex
	body        []byte
	health      []byte
	observation atomic.Int64
}

// reportBackup rewrites the served payload so the gateway's uplink moves to
// WAN2, which is what the provider derives "wan: backup" from.
func (f *fakeConsole) reportBackup(t *testing.T) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(f.body, &payload); err != nil {
		t.Fatalf("parsing the captured payload: %v", err)
	}
	for _, device := range payload["data"].([]any) {
		record, ok := device.(map[string]any)
		if !ok {
			continue
		}
		if wan1, ok := record["wan1"].(map[string]any); ok {
			wan1["is_uplink"] = false
		}
		if wan2, ok := record["wan2"].(map[string]any); ok {
			wan2["is_uplink"] = true
			wan2["up"] = true
		}
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = rewritten
}

func newFakeConsole(t *testing.T) (*fakeConsole, *httptest.Server) {
	t.Helper()
	// The real capture, so the observation being counted is a real one.
	body, err := os.ReadFile("../../testdata/unifi/api/stat-device-gateway.json")
	if err != nil {
		t.Fatalf("reading the captured device payload: %v", err)
	}
	health, err := os.ReadFile("../../testdata/unifi/api/stat-health.json")
	if err != nil {
		t.Fatalf("reading the captured health payload: %v", err)
	}
	console := &fakeConsole{body: body, health: health}
	// An observation reads two endpoints, so the counter follows the device
	// call only. What these tests measure is how many observations happened,
	// not how many requests one costs.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		console.mu.Lock()
		defer console.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "stat/health") {
			_, _ = w.Write(console.health)
			return
		}
		console.observation.Add(1)
		_, _ = w.Write(console.body)
	}))
	t.Cleanup(server.Close)
	return console, server
}

// waitForObservations blocks until the console has been asked at least want
// times, reporting whether it happened inside the deadline.
func (f *fakeConsole) waitForObservations(want int64, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if f.observation.Load() >= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return f.observation.Load() >= want
}

// waitForStoredState blocks until an observation has actually landed in the
// store. The console being asked is not the same instant as the answer being
// recorded, so a test that reads the store must wait for the store.
func waitForStoredState(t *testing.T, store *engine.StateStore) events.Observation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if observation, ok := store.Get(unifi.ProviderName); ok {
			return observation
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no observation reached the store")
	return events.Observation{}
}

// deliverTo posts an authenticated delivery through the receiver's real
// handler, so the test exercises the wire path rather than the channel.
func deliverTo(t *testing.T, receiver *unifi.Receiver) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, unifi.DefaultWebhookPath, strings.NewReader(`{"alarm":"whatever"}`))
	req.Header.Set("Authorization", "Bearer "+webhookTestToken)
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected the delivery to be accepted, got %d", rec.Code)
	}
}

// startPoller runs a poller against the fake console with a poll interval long
// enough that the ticker cannot be what causes any observation the test sees.
func startPoller(t *testing.T, url string, minInterval time.Duration) (*unifi.Receiver, *engine.StateStore) {
	t.Helper()
	return startPollerWith(t, url, minInterval, time.Hour, engine.NewStateStore())
}

// startPollerWith is the same, with the poll interval and the store — and so
// the debounce policy — under the test's control.
func startPollerWith(t *testing.T, url string, minInterval, interval time.Duration,
	store *engine.StateStore) (*unifi.Receiver, *engine.StateStore) {
	t.Helper()
	receiver := unifi.NewReceiver(unifi.WebhookConfig{Enabled: true, Token: webhookTestToken})
	poller := &UniFiPoller{
		Client:             unifi.NewClient(url, unifi.StaticAPIKey("key"), "default", false),
		Store:              store,
		Interval:           interval,
		Nudge:              receiver.Requests(),
		MinObserveInterval: minInterval,
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- poller.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("expected the poller to stop cleanly, got %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the poller did not stop when its context was cancelled")
		}
	})
	return receiver, store
}

// The acceptance criterion: a delivery causes state to be re-observed straight
// away instead of at the next tick, which here is an hour off.
func TestADeliveryTriggersReObservationImmediately(t *testing.T) {
	console, server := newFakeConsole(t)
	receiver, store := startPoller(t, server.URL, time.Millisecond)

	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}
	waitForStoredState(t, store)

	start := time.Now()
	deliverTo(t, receiver)
	if !console.waitForObservations(2, time.Second) {
		t.Fatalf("expected a delivery to trigger a re-observation within a second, waited %s", time.Since(start))
	}
}

// A delivery is a hint, never a fact: whatever it claims, the state that lands
// in the store is the one the console reports on the poll that follows.
func TestADeliveryCannotPutStateInTheStore(t *testing.T) {
	console, server := newFakeConsole(t)
	receiver, store := startPoller(t, server.URL, time.Millisecond)
	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}
	before := waitForStoredState(t, store)

	// A delivery insisting the WAN failed over, sent while the console keeps
	// reporting the primary uplink.
	req := httptest.NewRequest(http.MethodPost, unifi.DefaultWebhookPath,
		strings.NewReader(`{"state":{"wan":"backup"},"triggers":["network:internet_disconnected"]}`))
	req.Header.Set("Authorization", "Bearer "+webhookTestToken)
	receiver.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if !console.waitForObservations(2, 5*time.Second) {
		t.Fatal("expected the delivery to trigger a re-observation")
	}
	after, _ := store.Get(unifi.ProviderName)
	if after.State["wan"] != before.State["wan"] {
		t.Errorf("a delivery moved state from %q to %q; only an observation may do that",
			before.State["wan"], after.State["wan"])
	}
}

// An unauthenticated delivery must not even cost a poll: otherwise the endpoint
// is a way to make Reactor hammer somebody's gateway for free.
func TestAnUnauthenticatedDeliveryCausesNoObservation(t *testing.T) {
	console, server := newFakeConsole(t)
	receiver, _ := startPoller(t, server.URL, time.Millisecond)
	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}

	req := httptest.NewRequest(http.MethodPost, unifi.DefaultWebhookPath, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	time.Sleep(200 * time.Millisecond)
	if got := console.observation.Load(); got != 1 {
		t.Errorf("expected the console to be left alone, it was asked %d times", got)
	}
}

// A flood of valid deliveries must not become a flood of console requests. The
// floor is what stands between a chatty (or hostile) sender and the gateway.
func TestADeliveryFloodIsRateLimited(t *testing.T) {
	const floor = 400 * time.Millisecond
	console, server := newFakeConsole(t)
	receiver, _ := startPoller(t, server.URL, floor)
	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}

	for range 200 {
		deliverTo(t, receiver)
	}
	// Well inside the floor, the console must not have been asked again.
	time.Sleep(floor / 4)
	if got := console.observation.Load(); got != 1 {
		t.Errorf("expected the flood to be held off, the console was asked %d times", got)
	}
	// And the deliveries must not be lost either — one observation still owed.
	if !console.waitForObservations(2, 5*time.Second) {
		t.Error("expected the held-off deliveries to still produce one observation")
	}
	if got := console.observation.Load(); got > 3 {
		t.Errorf("expected 200 deliveries to collapse into about one observation, got %d", got)
	}
}

// The fast path being absent is the default, and must change nothing.
func TestAPollerWithoutTheFastPathStillPolls(t *testing.T) {
	console, server := newFakeConsole(t)
	poller := &UniFiPoller{
		Client:   unifi.NewClient(server.URL, unifi.StaticAPIKey("key"), "default", false),
		Store:    engine.NewStateStore(),
		Interval: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = poller.Start(ctx) }()

	if !console.waitForObservations(3, 5*time.Second) {
		t.Errorf("expected polling to continue with no nudge channel, got %d observations",
			console.observation.Load())
	}
}

// debouncedStore requires a changed value to hold for samples consecutive
// observations before it is reported, which is what #54 added and what a
// delivery must not be able to shortcut.
func debouncedStore(samples int) *engine.StateStore {
	return engine.NewStateStore(engine.WithDebounce(unifi.ProviderName,
		engine.DebounceConfig{Default: samples}))
}

// The interaction between the fast path and debounce, and the one that would
// be a security problem if it went the other way: a delivery may make Reactor
// look sooner, but it must never supply the evidence that promotes a value.
// Otherwise anyone who can reach the endpoint could push a debounced key
// straight through the settling time its operator asked for.
func TestADeliveryBurstCannotFastForwardADebouncedKey(t *testing.T) {
	console, server := newFakeConsole(t)
	// A poll interval long enough that no tick can contribute a sample: every
	// observation in this test is one a delivery asked for.
	receiver, store := startPollerWith(t, server.URL, time.Millisecond, time.Hour, debouncedStore(3))

	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}
	if got := waitForStoredState(t, store).State["wan"]; got != "primary" {
		t.Fatalf("expected the first observation to report wan=primary, got %q", got)
	}

	// The console now genuinely reports the failover, so every read from here
	// on backs the new value. Only the number of reads is in question.
	console.reportBackup(t)

	for range 200 {
		deliverTo(t, receiver)
	}
	time.Sleep(500 * time.Millisecond)

	if got, _ := store.Get(unifi.ProviderName); got.State["wan"] != "primary" {
		t.Errorf("200 deliveries promoted a debounced key to %q; only the poll cadence may do that",
			got.State["wan"])
	}
	// One delivery is allowed to start the debounce — that is "look sooner".
	// Every later one is refused while the value is still proving itself.
	if got := console.observation.Load(); got > 2 {
		t.Errorf("expected at most 2 observations (one on start, one from the first delivery), got %d", got)
	}
}

// The flip side: refusing deliveries mid-debounce must not wedge the poller.
// The poll cadence still supplies the samples and the key is still promoted.
func TestADebouncedKeyIsStillPromotedByThePollCadence(t *testing.T) {
	console, server := newFakeConsole(t)
	receiver, store := startPollerWith(t, server.URL, time.Millisecond, 100*time.Millisecond, debouncedStore(3))

	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}
	waitForStoredState(t, store)
	console.reportBackup(t)

	// Deliveries throughout, to prove they neither help nor hinder.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		deliverTo(t, receiver)
		if got, _ := store.Get(unifi.ProviderName); got.State["wan"] == wanBackupValue {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := store.Get(unifi.ProviderName)
	t.Errorf("the debounced key was never promoted by the poll cadence, still %q", got.State["wan"])
}

// One delivery is one sample. Without debounce configured the very first
// delivery promotes the change, which is the fast path doing its job — the
// contrast with the test above is entirely the operator's debounce policy.
func TestWithoutDebounceADeliveryPromotesImmediately(t *testing.T) {
	console, server := newFakeConsole(t)
	receiver, store := startPoller(t, server.URL, time.Millisecond)

	if !console.waitForObservations(1, 5*time.Second) {
		t.Fatal("expected the poller to observe once on start")
	}
	waitForStoredState(t, store)
	console.reportBackup(t)

	deliverTo(t, receiver)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := store.Get(unifi.ProviderName); got.State["wan"] == wanBackupValue {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("a single delivery did not bring the change forward with no debounce configured")
}
