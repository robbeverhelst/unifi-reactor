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
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
)

// The console fixtures. Every one of them is synthetic: no wlanconf response
// has ever been captured, which is exactly why the stub below asserts on what
// Reactor SENDS rather than pretending its own answers are ground truth.
const (
	testPassword = "hunter2-not-a-real-password"
	guestWLAN    = "test-guest"
	mainWLAN     = "test-main"
	guestWLANID  = "019ff10d-1111-0000-0000-000000000002"
	// envelopeData is the key every stat endpoint wraps its payload in.
	envelopeData = "data"
)

// consoleStub is a UniFi console far enough along to exercise the write path:
// it hands out a session cookie carrying a csrfToken claim, demands that claim
// back on writes, and records every request so a test can assert on the order
// and the bodies.
type consoleStub struct {
	mu sync.Mutex
	// wlans is the wlanconf table, keyed by id.
	wlans map[string]map[string]any
	// requests is every method+path the stub was asked for, in order.
	requests []string
	// bodies is every decoded write body, in order.
	bodies []map[string]any
	// failWLANList makes the read leg fail, so the check-before-write path can
	// be shown abandoning rather than writing anyway.
	failWLANList bool
	// silentWrite accepts the write and stores nothing, which is the failure
	// mode an undocumented endpoint is most likely to have.
	silentWrite bool

	server *httptest.Server
}

func newConsoleStub(t *testing.T) *consoleStub {
	t.Helper()
	stub := &consoleStub{wlans: map[string]map[string]any{
		"019ff10d-1111-0000-0000-000000000001": {
			fieldWLANID: "019ff10d-1111-0000-0000-000000000001", fieldWLANName: mainWLAN,
			fieldWLANEnabled: true, "security": "wpapsk",
		},
		guestWLANID: {
			fieldWLANID: guestWLANID, fieldWLANName: guestWLAN,
			fieldWLANEnabled: true, "security": "wpapsk", "wpa_mode": "wpa2",
		},
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", stub.login)
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r, nil)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /proxy/network/api/s/{site}/rest/wlanconf", stub.listWLANs)
	mux.HandleFunc("PUT /proxy/network/api/s/{site}/rest/wlanconf/{id}", stub.updateWLAN)

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *consoleStub) record(r *http.Request, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	if body != nil {
		s.bodies = append(s.bodies, body)
	}
}

func (s *consoleStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *consoleStub) written() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

func (s *consoleStub) login(w http.ResponseWriter, r *http.Request) {
	s.record(r, nil)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"csrfToken":"` + testCSRF + `"}`))
	http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "header." + payload + ".signature", Path: "/"})
	_, _ = w.Write([]byte(`{}`))
}

func (s *consoleStub) listWLANs(w http.ResponseWriter, r *http.Request) {
	s.record(r, nil)
	if s.failWLANList {
		http.Error(w, `{"message":"api.err.Invalid"}`, http.StatusInternalServerError)
		return
	}
	if _, err := r.Cookie("TOKEN"); err != nil {
		http.Error(w, `{"message":"api.err.LoginRequired"}`, http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	records := make([]any, 0, len(s.wlans))
	for _, wlan := range s.wlans {
		records = append(records, maps.Clone(wlan))
	}
	s.mu.Unlock()
	writeStubJSON(w, records)
}

func (s *consoleStub) updateWLAN(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"invalid json"}`, http.StatusBadRequest)
		return
	}
	s.record(r, body)

	if r.Header.Get(csrfHeader) != testCSRF {
		http.Error(w, `{"message":"csrf token mismatch"}`, http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, known := s.wlans[r.PathValue("id")]
	if !known {
		http.Error(w, `{"message":"api.err.ObjectNotFound"}`, http.StatusNotFound)
		return
	}
	if !s.silentWrite {
		stored[fieldWLANEnabled] = body[fieldWLANEnabled]
	}
	writeStubJSON(w, []any{maps.Clone(stored)})
}

func writeStubJSON(w http.ResponseWriter, data []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": "ok"}, envelopeData: data})
}

// writerFor builds a Writer pointed at the stub, with the named WLANs allowed.
func writerFor(t *testing.T, stub *consoleStub, allowed ...string) *Writer {
	t.Helper()
	writer, err := NewWriter(Config{
		URL:     stub.server.URL,
		Site:    defaultSite,
		Actions: ActionsConfig{AllowedWLANs: allowed},
		Webhook: WebhookConfig{Username: "reactor", Password: testPassword},
	})
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	return writer
}

func wlanAction(actionType, name string) reactorv1alpha1.Action {
	return reactorv1alpha1.Action{Type: actionType, WLAN: &reactorv1alpha1.WLAN{Name: name}}
}

func (s *consoleStub) enabled(t *testing.T, id string) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	enabled, ok := s.wlans[id][fieldWLANEnabled].(bool)
	if !ok {
		t.Fatalf("wlan %s has no boolean enabled", id)
	}
	return enabled
}

func TestWLANDisableWritesTheRecordItRead(t *testing.T) {
	stub := newConsoleStub(t)
	writer := writerFor(t, stub, guestWLAN)

	result, err := writer.Apply(context.Background(), wlanAction(actions.TypeUniFiWLANDisable, guestWLAN), 0)
	if err != nil {
		t.Fatalf("disabling the wlan: %v", err)
	}
	if result.Origin != "unifi/wlan/"+guestWLAN {
		t.Errorf("origin = %q, want the console object", result.Origin)
	}
	if result.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly one: a console write is at-most-once", result.Attempts)
	}
	if stub.enabled(t, guestWLANID) {
		t.Error("the wlan is still enabled")
	}

	// Log in, read, write, log out — in that order. The read before the write is
	// the check, and the logout is the session not outliving the action.
	want := []string{
		"POST /api/auth/login",
		"GET /proxy/network/api/s/default/rest/wlanconf",
		"PUT /proxy/network/api/s/default/rest/wlanconf/" + guestWLANID,
		"POST /api/auth/logout",
	}
	if got := stub.seen(); !slicesEqual(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}

	// The body is the record as read, with one key changed. Anything else would
	// be Reactor inventing a value for a field it does not understand.
	written := stub.written()
	if len(written) != 1 {
		t.Fatalf("wrote %d bodies, want 1", len(written))
	}
	if written[0][fieldWLANEnabled] != false {
		t.Errorf("enabled = %v, want false", written[0][fieldWLANEnabled])
	}
	for key, want := range map[string]any{
		fieldWLANID: guestWLANID, fieldWLANName: guestWLAN, "wpa_mode": "wpa2",
	} {
		if written[0][key] != want {
			t.Errorf("%s = %v in the written body, want %v carried back untouched", key, written[0][key], want)
		}
	}
}

func TestWLANWriteIsSkippedWhenAlreadyThere(t *testing.T) {
	stub := newConsoleStub(t)
	writer := writerFor(t, stub, guestWLAN)

	// The WLAN is already enabled, so enabling it must read and stop. A repeated
	// transition should not be a repeated write to somebody's wireless config.
	if _, err := writer.Apply(
		context.Background(), wlanAction(actions.TypeUniFiWLANEnable, guestWLAN), 0); err != nil {
		t.Fatalf("enabling an already enabled wlan: %v", err)
	}
	for _, request := range stub.seen() {
		if strings.HasPrefix(request, http.MethodPut) {
			t.Errorf("wrote %s to a wlan that was already in the wanted state", request)
		}
	}
}

func TestWLANRefusedWithoutAnAllowlistEntry(t *testing.T) {
	for name, allowed := range map[string][]string{
		"nothing allowed":      nil,
		"another wlan allowed": {"test-main"},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newConsoleStub(t)
			writer := writerFor(t, stub, allowed...)

			_, err := writer.Apply(
				context.Background(), wlanAction(actions.TypeUniFiWLANDisable, guestWLAN), 0)
			if err == nil {
				t.Fatal("the action was allowed with no allowlist entry for it")
			}
			if !strings.Contains(err.Error(), "unifi.actions.allowedWlans") {
				t.Errorf("error %q does not name the value to set", err)
			}
			// A refused action must not even open a session on the console.
			if got := stub.seen(); len(got) != 0 {
				t.Errorf("a refused action still talked to the console: %v", got)
			}
		})
	}
}

func TestWLANRefusedWithoutConsoleCredentials(t *testing.T) {
	stub := newConsoleStub(t)
	writer, err := NewWriter(Config{
		URL:     stub.server.URL,
		Actions: ActionsConfig{AllowedWLANs: []string{guestWLAN}},
	})
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	if writer.Credentialed() {
		t.Fatal("a writer with no username or password reports itself credentialed")
	}

	_, err = writer.Apply(context.Background(), wlanAction(actions.TypeUniFiWLANDisable, guestWLAN), 0)
	if err == nil || !strings.Contains(err.Error(), "UNIFI_USERNAME") {
		t.Fatalf("error = %v, want one naming the credentials the write path needs", err)
	}
	if got := stub.seen(); len(got) != 0 {
		t.Errorf("an uncredentialed action still talked to the console: %v", got)
	}
}

func TestWLANNotOnTheConsoleIsAbandoned(t *testing.T) {
	stub := newConsoleStub(t)
	writer := writerFor(t, stub, "test-absent")

	_, err := writer.Apply(context.Background(), wlanAction(actions.TypeUniFiWLANDisable, "test-absent"), 0)
	if err == nil {
		t.Fatal("a wlan that does not exist was not reported")
	}
	if !strings.Contains(err.Error(), `no wlan named "test-absent"`) {
		t.Errorf("error = %q, want one naming the wlan asked for", err)
	}
	// The refusal must not name the SSIDs that DO exist: this text reaches
	// status, which anyone who can read the Automation can read.
	for _, leaked := range []string{mainWLAN, guestWLAN} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("error %q lists a wlan the author did not ask about", err)
		}
	}
	for _, request := range stub.seen() {
		if strings.HasPrefix(request, http.MethodPut) {
			t.Errorf("wrote %s after failing to find the wlan", request)
		}
	}
}

func TestWLANReadFailureWritesNothing(t *testing.T) {
	stub := newConsoleStub(t)
	stub.failWLANList = true
	writer := writerFor(t, stub, guestWLAN)

	if _, err := writer.Apply(
		context.Background(), wlanAction(actions.TypeUniFiWLANDisable, guestWLAN), 0); err == nil {
		t.Fatal("a failed read was not reported")
	}
	for _, request := range stub.seen() {
		if strings.HasPrefix(request, http.MethodPut) {
			t.Errorf("wrote %s without a successful read to check against", request)
		}
	}
}

func TestWLANWriteThatDidNotTakeIsReported(t *testing.T) {
	stub := newConsoleStub(t)
	stub.silentWrite = true
	writer := writerFor(t, stub, guestWLAN)

	_, err := writer.Apply(context.Background(), wlanAction(actions.TypeUniFiWLANDisable, guestWLAN), 0)
	if err == nil {
		t.Fatal("a 200 that changed nothing was reported as success")
	}
	if !strings.Contains(err.Error(), "still reports") {
		t.Errorf("error = %q, want one saying the console did not apply the write", err)
	}
}

func TestConsoleErrorsCarryNoPassword(t *testing.T) {
	stub := newConsoleStub(t)
	stub.failWLANList = true
	writer := writerFor(t, stub, guestWLAN)

	_, err := writer.Apply(context.Background(), wlanAction(actions.TypeUniFiWLANDisable, guestWLAN), 0)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("error %q carries the console password", err)
	}
}

func TestUnknownConsoleActionIsRefused(t *testing.T) {
	stub := newConsoleStub(t)
	writer := writerFor(t, stub, guestWLAN)

	if _, err := writer.Apply(context.Background(),
		reactorv1alpha1.Action{Type: "unifi.wlan.something"}, 0); err == nil {
		t.Fatal("an unknown console action was accepted")
	}
}

func TestWritePolicyRefusesEverythingByDefault(t *testing.T) {
	policy, err := NewWritePolicy(ActionsConfig{})
	if err != nil {
		t.Fatalf("parsing an empty policy: %v", err)
	}
	if !policy.Empty() {
		t.Error("an unconfigured policy is not empty, so it would allow something")
	}
	if policy.allowsWLAN(guestWLAN) {
		t.Error("an empty policy allows a wlan")
	}
}

func TestWLANAllowlistMatchesExactly(t *testing.T) {
	policy, err := NewWritePolicy(ActionsConfig{AllowedWLANs: []string{guestWLAN}})
	if err != nil {
		t.Fatalf("parsing the policy: %v", err)
	}
	for name, want := range map[string]bool{
		guestWLAN:                          true,
		strings.ToUpper(guestWLAN):         false,
		guestWLAN + " ":                    false,
		strings.TrimSuffix(guestWLAN, "t"): false,
	} {
		if got := policy.allowsWLAN(name); got != want {
			t.Errorf("allowsWLAN(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSplitListDropsBlanks(t *testing.T) {
	got := splitList(" a , ,b,")
	if !slicesEqual(got, []string{"a", "b"}) {
		t.Errorf("splitList = %v, want [a b]", got)
	}
	if entries := splitList(""); len(entries) != 0 {
		t.Errorf("an empty variable produced %v, want no entries at all", entries)
	}
}

func TestFindObjectWithNeedsEveryRequiredKey(t *testing.T) {
	// A document where the name matches on an object that is not a WLAN record.
	// Requiring the other keys is what stops the search matching it.
	doc := map[string]any{envelopeData: []any{
		map[string]any{fieldWLANName: guestWLAN},
		map[string]any{fieldWLANID: guestWLANID, fieldWLANName: guestWLAN, fieldWLANEnabled: true},
	}}
	found, ok := findObjectWith(doc, fieldWLANName, guestWLAN, fieldWLANID, fieldWLANEnabled)
	if !ok {
		t.Fatal("the wlan record was not found")
	}
	if found[fieldWLANID] != guestWLANID {
		t.Errorf("matched %v, want the record carrying every required key", found)
	}
}

func slicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
