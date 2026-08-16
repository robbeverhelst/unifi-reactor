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
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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

	// The PoE fixtures. Inside the documentation-ish MAC prefix the captures
	// use, so nothing here looks like a real device.
	testSwitchMAC = "aa:bb:cc:00:11:33"
	testAPPort    = int32(7)
	testAPName    = "test-ap"

	// The outlet fixtures. Synthetic like everything else here: the committed
	// capture carries no _id, no outlet_caps and no outlet_overrides, because
	// the projection that would have kept them was only written once the write
	// path existed. What these reproduce is the shape read off the real UPS on
	// 2026-08-15 — including the caps values, which are what tell a
	// battery-backed outlet from a surge-only one.
	testUPSMAC     = "aa:bb:cc:00:11:44"
	testUPSID      = "000000000000000000000042"
	testBatteryOut = int32(1)
	testSurgeOut   = int32(5)
	testBatteryNm  = "test-nas"
	testSurgeName  = "test-bench"

	// The observed bits: [0,2,3,16] on the battery-backed bank, [0,2,16] on the
	// surge-only one.
	testCapsBattery = float64(1<<0 | 1<<2 | 1<<3 | 1<<16)
	testCapsSurge   = float64(1<<0 | 1<<2 | 1<<16)

	// The request lines and refusal fragments the assertions below share.
	fieldRelayGroup = "relay_group"
	reqLogin        = "POST /api/auth/login"
	reqLogout       = "POST /api/auth/logout"
	wantNoSuchMAC   = "no device with mac"
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

	// ports is the synthetic switch's port table, and cycles records every
	// power-cycle command that got through. The stub accepts a cycle of any
	// port it has, exactly as a console would: refusing here would let the
	// checks that matter rot untested.
	ports  []map[string]any
	cycles []portCommand

	// The UPS. outlets is the outlet_table Reactor reads an outlet's identity
	// and position from, and overrides is the separate outlet_overrides array
	// it writes through — two tables describing the same eight sockets, which
	// is how the real device reports them.
	//
	// The stub accepts any relay it is sent, exactly as a console would. Every
	// refusal below is Reactor's, which is the point: on real hardware nothing
	// else is going to apply one.
	outlets     []map[string]any
	overrides   []map[string]any
	upsID       string
	noOverrides bool

	server *httptest.Server
}

// portCommand is one command the stub was sent, as it decoded it.
type portCommand struct {
	mac   string
	index int32
}

func newConsoleStub(t *testing.T) *consoleStub {
	t.Helper()
	stub := &consoleStub{ports: []map[string]any{
		{
			fieldPortIndex: 1.0, fieldPortName: "test-uplink",
			fieldIsUplink: true, fieldPortPoE: true, fieldPoEEnable: true,
		},
		{
			fieldPortIndex: float64(testAPPort), fieldPortName: testAPName,
			fieldIsUplink: false, fieldPortPoE: true, fieldPoEEnable: true,
		},
		{
			fieldPortIndex: 8.0, fieldPortName: "test-desk",
			fieldIsUplink: false, fieldPortPoE: false,
		},
	}, upsID: testUPSID, outlets: []map[string]any{
		{
			fieldOutletIndex: float64(testBatteryOut), fieldOutletName: testBatteryNm,
			fieldRelayState: true, fieldRelayGroup: 1.0, fieldOutletCaps: testCapsBattery,
		},
		{
			fieldOutletIndex: float64(testSurgeOut), fieldOutletName: testSurgeName,
			fieldRelayState: true, fieldRelayGroup: 2.0, fieldOutletCaps: testCapsSurge,
		},
		{
			// Still carrying the console's placeholder, which is what every
			// outlet on this hardware ships with.
			fieldOutletIndex: 6.0, fieldOutletName: "Outlet 6",
			fieldRelayState: true, fieldRelayGroup: 2.0, fieldOutletCaps: testCapsSurge,
		},
		{
			// A named outlet whose bank cannot be read.
			fieldOutletIndex: 7.0, fieldOutletName: "test-uncapped",
			fieldRelayState: true, fieldRelayGroup: 2.0,
		},
	}, wlans: map[string]map[string]any{
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
	mux.HandleFunc(reqLogin, stub.login)
	mux.HandleFunc(reqLogout, func(w http.ResponseWriter, r *http.Request) {
		stub.record(r, nil)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /proxy/network/api/s/{site}/rest/wlanconf", stub.listWLANs)
	mux.HandleFunc("PUT /proxy/network/api/s/{site}/rest/wlanconf/{id}", stub.updateWLAN)
	mux.HandleFunc("GET /proxy/network/api/s/{site}/stat/device", stub.listDevices)
	mux.HandleFunc("POST /proxy/network/api/s/{site}/cmd/devmgr", stub.deviceCommand)
	mux.HandleFunc("PUT /proxy/network/api/s/{site}/rest/device/{id}", stub.updateDevice)

	// The overrides array mirrors the table: same outlets, same names, same
	// positions, plus the cycle_enabled the PDU write path carries. It is built
	// from the table rather than written out twice so a test that changes an
	// outlet cannot leave the two disagreeing by accident.
	for _, outlet := range stub.outlets {
		stub.overrides = append(stub.overrides, map[string]any{
			fieldOutletIndex: outlet[fieldOutletIndex],
			fieldOutletName:  outlet[fieldOutletName],
			fieldRelayState:  outlet[fieldRelayState],
			"cycle_enabled":  false,
		})
	}

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

func (s *consoleStub) listDevices(w http.ResponseWriter, r *http.Request) {
	s.record(r, nil)
	s.mu.Lock()
	table := make([]any, 0, len(s.ports))
	for _, port := range s.ports {
		table = append(table, maps.Clone(port))
	}
	s.mu.Unlock()
	writeStubJSON(w, []any{map[string]any{
		// A gateway with no port table, so the search has something to skip
		// past before it finds the switch.
		fieldMAC: "aa:bb:cc:00:11:22", fieldWLANName: "test-gateway",
	}, map[string]any{
		fieldMAC: testSwitchMAC, fieldWLANName: "test-switch", fieldPortTable: table,
	}, s.upsRecord()})
}

// upsRecord is the UPS as stat/device reports it.
func (s *consoleStub) upsRecord() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	table := make([]any, 0, len(s.outlets))
	for _, outlet := range s.outlets {
		table = append(table, maps.Clone(outlet))
	}
	record := map[string]any{
		fieldMAC: testUPSMAC, fieldWLANName: "test-ups", fieldOutletTable: table,
	}
	if s.upsID != "" {
		record[fieldDeviceID] = s.upsID
	}
	if !s.noOverrides {
		overrides := make([]any, 0, len(s.overrides))
		for _, override := range s.overrides {
			overrides = append(overrides, maps.Clone(override))
		}
		record[fieldOutletOverrides] = overrides
	}
	return record
}

// updateDevice accepts the outlet write and stores it, so a re-read reports
// what was written — which is exactly as much as a console can tell you, and
// exactly as much as this stub is entitled to claim.
func (s *consoleStub) updateDevice(w http.ResponseWriter, r *http.Request) {
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
	if r.PathValue("id") != testUPSID {
		http.Error(w, `{"message":"api.err.ObjectNotFound"}`, http.StatusNotFound)
		return
	}
	submitted, ok := body[fieldOutletOverrides].([]any)
	if !ok {
		http.Error(w, `{"message":"api.err.InvalidPayload"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if !s.silentWrite {
		s.overrides = s.overrides[:0]
		for _, entry := range submitted {
			if override, ok := entry.(map[string]any); ok {
				s.overrides = append(s.overrides, maps.Clone(override))
				s.applyRelayLocked(override)
			}
		}
	}
	stored := make([]any, 0, len(s.overrides))
	for _, override := range s.overrides {
		stored = append(stored, maps.Clone(override))
	}
	s.mu.Unlock()

	writeStubJSON(w, []any{map[string]any{
		fieldDeviceID: testUPSID, fieldOutletOverrides: stored,
	}})
}

// applyRelayLocked moves the outlet_table position to match a stored override,
// so the two tables agree on the next read the way the console's would.
func (s *consoleStub) applyRelayLocked(override map[string]any) {
	index, ok := override[fieldOutletIndex].(float64)
	if !ok {
		return
	}
	for _, outlet := range s.outlets {
		if got, ok := outlet[fieldOutletIndex].(float64); ok && got == index {
			outlet[fieldRelayState] = override[fieldRelayState]
			return
		}
	}
}

// outletWrites is every PUT to the device endpoint that actually reached the
// stub. Almost every outlet test below asserts this is empty: the whole value
// of the checks is that nothing is sent, not that it is sent and regretted.
func (s *consoleStub) outletWrites() []map[string]any {
	var writes []map[string]any
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, body := range s.bodies {
		if _, ok := body[fieldOutletOverrides]; ok {
			writes = append(writes, body)
		}
	}
	return writes
}

// setOutlet changes one field of the surge-only outlet in one of the two
// tables, which is how each identity check is broken in turn. Passing nil
// deletes the field; naming fieldOutletTable or fieldOutletOverrides restricts
// the change to that table, so a disagreement between the two can be built
// deliberately.
//
// It only ever addresses the outlet the happy paths use, which is deliberate: a
// refusal is only interesting on the outlet that would otherwise have been
// switched.
func (s *consoleStub) setOutlet(t *testing.T, table, key string, value any) {
	const index = testSurgeOut
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for _, entries := range []struct {
		name string
		rows []map[string]any
	}{{fieldOutletTable, s.outlets}, {fieldOutletOverrides, s.overrides}} {
		if table != "" && table != entries.name {
			continue
		}
		for _, row := range entries.rows {
			if got, ok := row[fieldOutletIndex].(float64); !ok || int32(got) != index {
				continue
			}
			found = true
			if value == nil {
				delete(row, key)
				continue
			}
			row[key] = value
		}
	}
	if !found {
		t.Fatalf("no outlet %d in the stub", index)
	}
}

func (s *consoleStub) deviceCommand(w http.ResponseWriter, r *http.Request) {
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
	mac, _ := body[fieldMAC].(string)
	index, _ := body[fieldPortIndex].(float64)

	s.mu.Lock()
	s.cycles = append(s.cycles, portCommand{mac: mac, index: int32(index)})
	s.mu.Unlock()
	writeStubJSON(w, []any{})
}

// cycled is every power-cycle that actually reached the console. Almost every
// PoE test below asserts this is empty: the point of the checks is that the
// command is never sent, not that it is sent and then regretted.
func (s *consoleStub) cycled() []portCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]portCommand(nil), s.cycles...)
}

// setPort changes one field of one port, which is how each identity check is
// broken in turn.
func (s *consoleStub) setPort(t *testing.T, index int32, key string, value any) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, port := range s.ports {
		if got, _ := port[fieldPortIndex].(float64); int32(got) == index {
			if value == nil {
				delete(port, key)
			} else {
				port[key] = value
			}
			return
		}
	}
	t.Fatalf("no port %d in the stub", index)
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

// poeWriterFor builds a Writer with the named ports allowed.
func poeWriterFor(t *testing.T, stub *consoleStub, allowed ...string) *Writer {
	t.Helper()
	writer, err := NewWriter(Config{
		URL:     stub.server.URL,
		Site:    defaultSite,
		Actions: ActionsConfig{AllowedPoEPorts: allowed},
		Webhook: WebhookConfig{Username: testConsoleUser, Password: testPassword},
	})
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	return writer
}

func poeAction(port int32, name string) reactorv1alpha1.Action {
	return reactorv1alpha1.Action{
		Type: actions.TypeUniFiPoECycle,
		PoE:  &reactorv1alpha1.PoEPort{Device: testSwitchMAC, Port: port, PortName: name},
	}
}

// allowedAP is the allowlist entry for the one port these tests are allowed to
// cycle.
var allowedAP = fmt.Sprintf("%s/%d", testSwitchMAC, testAPPort)

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
		reqLogin,
		"GET /proxy/network/api/s/default/rest/wlanconf",
		"PUT /proxy/network/api/s/default/rest/wlanconf/" + guestWLANID,
		reqLogout,
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

// TestPoECycleChecksTheSwitchBeforeCuttingPower is the happy path, and it is
// mostly an assertion about ORDER: the port table is read and checked, and only
// then is the command sent.
func TestPoECycleChecksTheSwitchBeforeCuttingPower(t *testing.T) {
	stub := newConsoleStub(t)
	writer := poeWriterFor(t, stub, allowedAP)

	result, err := writer.Apply(context.Background(), poeAction(testAPPort, testAPName), 0)
	if err != nil {
		t.Fatalf("cycling the port: %v", err)
	}
	if want := fmt.Sprintf("unifi/port/%s/%d", testSwitchMAC, testAPPort); result.Origin != want {
		t.Errorf("origin = %q, want %q", result.Origin, want)
	}
	if result.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly one: a power cut is at-most-once", result.Attempts)
	}

	want := []string{
		reqLogin,
		"GET /proxy/network/api/s/default/stat/device",
		"POST /proxy/network/api/s/default/cmd/devmgr",
		reqLogout,
	}
	if got := stub.seen(); !slicesEqual(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if got := stub.cycled(); len(got) != 1 || got[0].mac != testSwitchMAC || got[0].index != testAPPort {
		t.Errorf("cycles = %v, want one on %s port %d", got, testSwitchMAC, testAPPort)
	}
	// The command addresses the port by the console's own index, not by a
	// position in the table it happened to be found at.
	written := stub.written()
	if len(written) != 1 || written[0]["cmd"] != "power-cycle" {
		t.Fatalf("body = %v, want one power-cycle command", written)
	}
	if written[0][fieldPortIndex] != float64(testAPPort) {
		t.Errorf("port_idx = %v, want %d", written[0][fieldPortIndex], testAPPort)
	}
}

// TestPoECycleRefusesAPortThatDrifted is the reason portName is required. The
// index still exists and is still allowlisted; what is plugged into it has a
// different name, so it is probably a different thing.
func TestPoECycleRefusesAPortThatDrifted(t *testing.T) {
	stub := newConsoleStub(t)
	stub.setPort(t, testAPPort, fieldPortName, "test-something-else")
	writer := poeWriterFor(t, stub, allowedAP)

	_, err := writer.Apply(context.Background(), poeAction(testAPPort, testAPName), 0)
	if err == nil {
		t.Fatal("a renamed port was cycled anyway")
	}
	for _, expected := range []string{"test-something-else", testAPName} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not say what the port is called now versus what was expected", err)
		}
	}
	if got := stub.cycled(); len(got) != 0 {
		t.Errorf("power was cut to %v despite the name not matching", got)
	}
}

// TestPoECycleRefusesTheUplink is the floor. The allowlist says yes and the
// answer is still no, because this port carries everything behind the switch.
func TestPoECycleRefusesTheUplink(t *testing.T) {
	stub := newConsoleStub(t)
	writer := poeWriterFor(t, stub, fmt.Sprintf("%s/1", testSwitchMAC))

	_, err := writer.Apply(context.Background(), poeAction(1, "test-uplink"), 0)
	if err == nil {
		t.Fatal("the switch uplink was cycled")
	}
	if !strings.Contains(err.Error(), "uplink") || !strings.Contains(err.Error(), "whatever the allowlist says") {
		t.Errorf("error = %q, want one saying the uplink is refused regardless of the allowlist", err)
	}
	if got := stub.cycled(); len(got) != 0 {
		t.Errorf("power was cut to the uplink: %v", got)
	}
}

// TestPoECycleRefusesWhatItCannotCheck covers the fields that are read
// strictly. A switch that does not report one of them is refused rather than
// assumed safe: a guard that silently stops applying is worse than one that
// declines.
func TestPoECycleRefusesWhatItCannotCheck(t *testing.T) {
	for name, breakage := range map[string]struct {
		key   string
		value any
		want  string
	}{
		"no port name":     {key: fieldPortName, value: nil, want: "reports no name"},
		"no uplink flag":   {key: fieldIsUplink, value: nil, want: fieldIsUplink},
		"no poe flag":      {key: fieldPortPoE, value: nil, want: fieldPortPoE},
		"not a poe port":   {key: fieldPortPoE, value: false, want: "does not supply PoE"},
		"poe switched off": {key: fieldPoEEnable, value: false, want: "switched off"},
		"uplink not a bool": {
			key: fieldIsUplink, value: "yes", want: "cannot tell whether it is the switch's uplink",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newConsoleStub(t)
			stub.setPort(t, testAPPort, breakage.key, breakage.value)
			writer := poeWriterFor(t, stub, allowedAP)

			_, err := writer.Apply(context.Background(), poeAction(testAPPort, testAPName), 0)
			if err == nil {
				t.Fatal("the port was cycled despite the check being unmakeable")
			}
			if !strings.Contains(err.Error(), breakage.want) {
				t.Errorf("error = %q, want one containing %q", err, breakage.want)
			}
			if got := stub.cycled(); len(got) != 0 {
				t.Errorf("power was cut anyway: %v", got)
			}
		})
	}
}

func TestPoECycleRefusesAnUnknownDeviceOrPort(t *testing.T) {
	for name, test := range map[string]struct {
		action reactorv1alpha1.Action
		allow  string
		want   string
	}{
		"no such switch": {
			action: reactorv1alpha1.Action{
				Type: actions.TypeUniFiPoECycle,
				PoE: &reactorv1alpha1.PoEPort{
					Device: "aa:bb:cc:00:11:99", Port: testAPPort, PortName: testAPName,
				},
			},
			allow: fmt.Sprintf("aa:bb:cc:00:11:99/%d", testAPPort),
			want:  wantNoSuchMAC,
		},
		"no such port": {
			action: poeAction(42, testAPName),
			allow:  fmt.Sprintf("%s/42", testSwitchMAC),
			want:   "has no port 42",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newConsoleStub(t)
			writer := poeWriterFor(t, stub, test.allow)

			_, err := writer.Apply(context.Background(), test.action, 0)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
			if got := stub.cycled(); len(got) != 0 {
				t.Errorf("power was cut anyway: %v", got)
			}
		})
	}
}

func TestPoECycleRefusedWithoutAnAllowlistEntry(t *testing.T) {
	for name, allowed := range map[string][]string{
		"nothing allowed":      nil,
		"another port allowed": {fmt.Sprintf("%s/8", testSwitchMAC)},
		"another switch allowed": {
			fmt.Sprintf("aa:bb:cc:00:11:44/%d", testAPPort),
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newConsoleStub(t)
			writer := poeWriterFor(t, stub, allowed...)

			_, err := writer.Apply(context.Background(), poeAction(testAPPort, testAPName), 0)
			if err == nil {
				t.Fatal("the port was cycled with no allowlist entry for it")
			}
			if !strings.Contains(err.Error(), "unifi.actions.allowedPoePorts") {
				t.Errorf("error %q does not name the value to set", err)
			}
			// A refused action must not even open a session on the console.
			if got := stub.seen(); len(got) != 0 {
				t.Errorf("a refused action still talked to the console: %v", got)
			}
		})
	}
}

// TestPoEAllowlistNeedsBothHalves pins the format decision from #25: a port
// index on its own means something different after somebody re-patches a rack,
// so an entry that names no switch is a configuration error rather than a
// wildcard.
func TestPoEAllowlistNeedsBothHalves(t *testing.T) {
	for name, entry := range map[string]string{
		"no switch":         "7",
		"no port":           testSwitchMAC + "/",
		"port zero":         testSwitchMAC + "/0",
		"negative port":     testSwitchMAC + "/-1",
		"not a mac":         "test-switch/7",
		"port not a number": testSwitchMAC + "/seven",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWritePolicy(ActionsConfig{AllowedPoEPorts: []string{entry}}); err == nil {
				t.Fatalf("allowlist entry %q was accepted", entry)
			}
		})
	}
}

// TestPoEAllowlistNormalizesTheMAC keeps a capital letter or a hyphen from
// being the reason an allowed port is refused during an incident.
func TestPoEAllowlistNormalizesTheMAC(t *testing.T) {
	policy, err := NewWritePolicy(ActionsConfig{
		AllowedPoEPorts: []string{" AA-BB-CC-00-11-33/7 "},
	})
	if err != nil {
		t.Fatalf("parsing the policy: %v", err)
	}
	if !policy.allowsPort(portRef{mac: testSwitchMAC, port: 7}) {
		t.Error("an allowlist entry written with hyphens and capitals did not match")
	}
	if policy.allowsPort(portRef{mac: testSwitchMAC, port: 8}) {
		t.Error("allowing port 7 also allowed port 8")
	}
}

// ---------------------------------------------------------------------------
// Outlets.
//
// The tests below are the ones that replaced TestNoOutletWritePathExists, and
// their shape follows from what that guard was protecting. It stood in front of
// a device that reports eight relays through one array, where the difference
// between cutting one socket and cutting all eight is which entries of that
// array Reactor sends back. So the load-bearing assertions here are not "the
// write happened" — they are about the body: what is in it, and what is not.

// outletWriterFor builds a Writer with the named outlets allowed, and says
// whether the battery-backed bank is on the table.
func outletWriterFor(t *testing.T, stub *consoleStub, battery bool, allowed ...string) *Writer {
	t.Helper()
	writer, err := NewWriter(Config{
		URL:  stub.server.URL,
		Site: defaultSite,
		Actions: ActionsConfig{
			AllowedOutlets:            allowed,
			AllowBatteryBackedOutlets: battery,
		},
		Webhook: WebhookConfig{Username: testConsoleUser, Password: testPassword},
	})
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	return writer
}

// outletAction builds one unifi.outlet.* action.
func outletAction(actionType string, index int32, name string) reactorv1alpha1.Action {
	return reactorv1alpha1.Action{
		Type:   actionType,
		Outlet: &reactorv1alpha1.Outlet{Device: testUPSMAC, Index: index, Name: name},
	}
}

// surgeEntry is the allowlist entry for the surge-only outlet the happy paths
// use, and batteryEntry for the battery-backed one.
func surgeEntry() string {
	return fmt.Sprintf("%s/%d/%s", testUPSMAC, testSurgeOut, testSurgeName)
}

func batteryEntry() string {
	return fmt.Sprintf("%s/%d/%s", testUPSMAC, testBatteryOut, testBatteryNm)
}

// TestOutletCutLogsInReadsWritesAndLogsOut pins the order, which is the whole
// safety argument: nothing is written before the UPS has been asked what is
// actually in that socket.
func TestOutletCutLogsInReadsWritesAndLogsOut(t *testing.T) {
	stub := newConsoleStub(t)
	writer := outletWriterFor(t, stub, false, surgeEntry())

	if _, err := writer.Apply(context.Background(),
		outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName), 0); err != nil {
		t.Fatalf("cutting the outlet: %v", err)
	}

	want := []string{
		reqLogin,
		"GET /proxy/network/api/s/default/stat/device",
		"PUT /proxy/network/api/s/default/rest/device/" + testUPSID,
		reqLogout,
	}
	if got := stub.seen(); !slices.Equal(got, want) {
		t.Errorf("request order:\n got %v\nwant %v", got, want)
	}
}

// TestOutletWriteChangesOnlyTheAddressedRelay is the guard #61 shipped, moved
// to where it can now say something stronger.
//
// The old one asserted that no code anywhere could build outlet_overrides. This
// one asserts what the code that does build it actually produces: the array the
// console just served, entry for entry, with exactly one relay_state changed
// and nothing else touched anywhere. That is the property that makes "cut
// outlet 5" mean outlet 5 — on hardware where the alternative is all eight.
func TestOutletWriteChangesOnlyTheAddressedRelay(t *testing.T) {
	stub := newConsoleStub(t)
	before := stub.upsRecord()[fieldOutletOverrides].([]any)
	writer := outletWriterFor(t, stub, false, surgeEntry())

	if _, err := writer.Apply(context.Background(),
		outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName), 0); err != nil {
		t.Fatalf("cutting the outlet: %v", err)
	}

	writes := stub.outletWrites()
	if len(writes) != 1 {
		t.Fatalf("expected exactly one write, got %d", len(writes))
	}
	body := writes[0]
	if len(body) != 1 {
		t.Errorf("the body carries %d keys; it may carry only %s", len(body), fieldOutletOverrides)
	}
	sent, ok := body[fieldOutletOverrides].([]any)
	if !ok {
		t.Fatalf("the body carries no %s array", fieldOutletOverrides)
	}
	if len(sent) != len(before) {
		t.Fatalf("sent %d overrides, read %d: the array must be the one just read",
			len(sent), len(before))
	}

	var changed []string
	for i := range sent {
		was, _ := before[i].(map[string]any)
		now, _ := sent[i].(map[string]any)
		if was == nil || now == nil {
			t.Fatalf("override %d is not an object in one of the two arrays", i)
		}
		if len(was) != len(now) {
			t.Errorf("override %d gained or lost a key: %v -> %v", i, was, now)
		}
		for key, value := range was {
			got, present := now[key]
			if !present {
				t.Errorf("override %d dropped %s", i, key)
				continue
			}
			if fmt.Sprint(got) != fmt.Sprint(value) {
				changed = append(changed, fmt.Sprintf("outlet %v %s", now[fieldOutletIndex], key))
			}
		}
	}
	want := []string{fmt.Sprintf("outlet %d %s", testSurgeOut, fieldRelayState)}
	if !slices.Equal(changed, want) {
		t.Errorf("the write changed %v; it may change only %v", changed, want)
	}
}

// TestRepeatedOutletTransitionWritesNothing. A relay that is already open is
// not cut again, which matters more here than for a WLAN: this is the action
// with no retry, so every write it does make is one somebody meant.
func TestRepeatedOutletTransitionWritesNothing(t *testing.T) {
	stub := newConsoleStub(t)
	writer := outletWriterFor(t, stub, false, surgeEntry())
	action := outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName)

	for i := range 2 {
		if _, err := writer.Apply(context.Background(), action, 0); err != nil {
			t.Fatalf("cut %d: %v", i+1, err)
		}
	}
	if writes := stub.outletWrites(); len(writes) != 1 {
		t.Errorf("cutting an already-open outlet wrote again: %d writes", len(writes))
	}
}

// TestOutletRestoreClosesTheRelay, and reads the position back off the table
// rather than assuming the override took.
func TestOutletRestoreClosesTheRelay(t *testing.T) {
	stub := newConsoleStub(t)
	writer := outletWriterFor(t, stub, false, surgeEntry())

	for _, actionType := range []string{actions.TypeUniFiOutletCut, actions.TypeUniFiOutletRestore} {
		if _, err := writer.Apply(context.Background(),
			outletAction(actionType, testSurgeOut, testSurgeName), 0); err != nil {
			t.Fatalf("%s: %v", actionType, err)
		}
	}
	if writes := stub.outletWrites(); len(writes) != 2 {
		t.Fatalf("expected a write each way, got %d", len(writes))
	}

	table, _ := stub.upsRecord()[fieldOutletTable].([]any)
	outlet, found := objectByIndex(table, fieldOutletIndex, testSurgeOut)
	if !found {
		t.Fatal("the outlet vanished from the table")
	}
	if state, _ := outlet[fieldRelayState].(bool); !state {
		t.Error("the outlet is still open after a restore")
	}
}

// TestOutletWriteThatDidNotTakeIsReported. A 200 that stored nothing is the
// failure mode an undocumented endpoint is most likely to have, and reporting
// it as success would leave an operator believing a relay moved.
func TestOutletWriteThatDidNotTakeIsReported(t *testing.T) {
	stub := newConsoleStub(t)
	stub.silentWrite = true
	writer := outletWriterFor(t, stub, false, surgeEntry())

	_, err := writer.Apply(context.Background(),
		outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName), 0)
	if err == nil {
		t.Fatal("a write the console did not store was reported as success")
	}
	if !strings.Contains(err.Error(), "accepted the write") {
		t.Errorf("the error does not say the console kept the old value: %v", err)
	}
}

// TestBatteryBackedOutletNeedsItsOwnConsent is the floor that exists because
// the one outlet worth cutting to extend runtime is also the one that can drop
// the gateway mid-outage.
func TestBatteryBackedOutletNeedsItsOwnConsent(t *testing.T) {
	stub := newConsoleStub(t)
	action := outletAction(actions.TypeUniFiOutletCut, testBatteryOut, testBatteryNm)

	refused := outletWriterFor(t, stub, false, batteryEntry())
	_, err := refused.Apply(context.Background(), action, 0)
	if err == nil {
		t.Fatal("a battery-backed outlet was cut without the second consent")
	}
	if !strings.Contains(err.Error(), "allowBatteryBackedOutlets") {
		t.Errorf("the refusal does not name the value to set: %v", err)
	}
	if writes := stub.outletWrites(); len(writes) != 0 {
		t.Fatalf("a refused battery-backed cut still wrote: %v", writes)
	}

	allowed := outletWriterFor(t, stub, true, batteryEntry())
	if _, err := allowed.Apply(context.Background(), action, 0); err != nil {
		t.Fatalf("the same cut with consent was refused: %v", err)
	}
	if writes := stub.outletWrites(); len(writes) != 1 {
		t.Errorf("the consented cut did not write: %d writes", len(writes))
	}
}

// TestBatteryConsentAloneAllowsNothing. It qualifies the allowlist; it is not
// one, and an install that set only it has allowed nothing.
func TestBatteryConsentAloneAllowsNothing(t *testing.T) {
	stub := newConsoleStub(t)
	writer := outletWriterFor(t, stub, true)

	if writer.Enabled() {
		t.Error("an install with only the battery consent set reports console writes as enabled")
	}
	_, err := writer.Apply(context.Background(),
		outletAction(actions.TypeUniFiOutletCut, testBatteryOut, testBatteryNm), 0)
	if err == nil {
		t.Fatal("the battery consent on its own allowed an outlet")
	}
	if len(stub.seen()) != 0 {
		t.Errorf("a refused action still opened a session: %v", stub.seen())
	}
}

// TestOutletRefusals is every way of being wrong about which socket this is,
// and the assertion that matters is the same in all of them: NOTHING was
// written. A relay that was not moved can be moved later; one that was cannot
// be un-cut.
func TestOutletRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		allowed []string
		action  reactorv1alpha1.Action
		breaks  func(t *testing.T, stub *consoleStub)
		wants   string
	}{
		"not allowlisted": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, 6, "test-other"),
			wants:   "allowedOutlets",
		},
		"allowlisted by index but renamed in the automation": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, "test-something-else"),
			wants:   "allowedOutlets",
		},
		// The allowlist cannot even carry this one — NewWritePolicy refuses a
		// placeholder entry — so the only way an action can name one is with
		// something else allowed, and it is refused before the allowlist is
		// consulted at all.
		"still carrying the console placeholder": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, 6, "Outlet 6"),
			wants:   "nobody has named",
		},
		"renamed on the console since": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks: func(t *testing.T, stub *consoleStub) {
				stub.setOutlet(t, fieldOutletTable, fieldOutletName, "test-moved")
			},
			wants: `is called "test-moved"`,
		},
		"no name on the console at all": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks: func(t *testing.T, stub *consoleStub) {
				stub.setOutlet(t, fieldOutletTable, fieldOutletName, nil)
			},
			wants: "reports no name",
		},
		"unknown ups": {
			allowed: []string{fmt.Sprintf("aa:bb:cc:00:11:99/%d/%s", testSurgeOut, testSurgeName)},
			action: reactorv1alpha1.Action{
				Type: actions.TypeUniFiOutletCut,
				Outlet: &reactorv1alpha1.Outlet{
					Device: "aa:bb:cc:00:11:99", Index: testSurgeOut, Name: testSurgeName,
				},
			},
			wants: "no device with mac",
		},
		"unknown outlet": {
			allowed: []string{fmt.Sprintf("%s/9/%s", testUPSMAC, testSurgeName)},
			action:  outletAction(actions.TypeUniFiOutletCut, 9, testSurgeName),
			wants:   "has no outlet 9",
		},
		"bank cannot be read": {
			allowed: []string{fmt.Sprintf("%s/7/test-uncapped", testUPSMAC)},
			action:  outletAction(actions.TypeUniFiOutletCut, 7, "test-uncapped"),
			wants:   fieldOutletCaps,
		},
		"position cannot be read": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks: func(t *testing.T, stub *consoleStub) {
				stub.setOutlet(t, fieldOutletTable, fieldRelayState, nil)
			},
			wants: "cannot tell which way it is set",
		},
		"no overrides to modify": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks:  func(_ *testing.T, stub *consoleStub) { stub.noOverrides = true },
			wants:   wantNoSuchMAC,
		},
		"no override for this outlet": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks: func(t *testing.T, stub *consoleStub) {
				stub.setOutlet(t, fieldOutletOverrides, fieldOutletIndex, 99.0)
			},
			wants: "no entry in it for outlet",
		},
		"the two tables disagree about the name": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks: func(t *testing.T, stub *consoleStub) {
				stub.setOutlet(t, fieldOutletOverrides, fieldOutletName, "test-elsewhere")
			},
			wants: "disagree about which outlet this is",
		},
		"no address to write to": {
			allowed: []string{surgeEntry()},
			action:  outletAction(actions.TypeUniFiOutletCut, testSurgeOut, testSurgeName),
			breaks:  func(_ *testing.T, stub *consoleStub) { stub.upsID = "" },
			wants:   "no usable _id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newConsoleStub(t)
			if tc.breaks != nil {
				tc.breaks(t, stub)
			}
			writer := outletWriterFor(t, stub, false, tc.allowed...)

			_, err := writer.Apply(context.Background(), tc.action, 0)
			if err == nil {
				t.Fatal("the outlet was switched")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal does not say %q: %v", tc.wants, err)
			}
			if writes := stub.outletWrites(); len(writes) != 0 {
				t.Errorf("a refused action still wrote to the console: %v", writes)
			}
		})
	}
}

// TestAllowedOutletEntriesAreRefusedWithoutAllThreeParts. A bare index would go
// on allowing whatever ends up in that socket, and two parts would allow
// whatever ends up in it on that UPS. Only the third makes the entry a thing.
func TestAllowedOutletEntriesAreRefusedWithoutAllThreeParts(t *testing.T) {
	for name, entry := range map[string]string{
		"bare index":          "5",
		"mac only":            testUPSMAC,
		"mac and index":       testUPSMAC + "/5",
		"no index":            testUPSMAC + "//test-nas",
		"index zero":          testUPSMAC + "/0/test-nas",
		"negative index":      testUPSMAC + "/-1/test-nas",
		"not a mac":           "test-ups/5/test-nas",
		"index not a number":  testUPSMAC + "/five/test-nas",
		"empty name":          testUPSMAC + "/5/  ",
		"the placeholder":     testUPSMAC + "/5/Outlet 5",
		"the placeholder too": testUPSMAC + "/5/outlet-5",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWritePolicy(ActionsConfig{AllowedOutlets: []string{entry}}); err == nil {
				t.Fatalf("allowlist entry %q was accepted", entry)
			}
		})
	}
}

// TestAllowedOutletEntriesKeepTheWholeName, so an outlet somebody called
// "rack/nas" can still be allowlisted, and a capital or a hyphen in the MAC is
// not the reason a cut is refused during an incident.
func TestAllowedOutletEntriesKeepTheWholeName(t *testing.T) {
	policy, err := NewWritePolicy(ActionsConfig{
		AllowedOutlets: []string{" AA-BB-CC-00-11-44/5/rack/nas "},
	})
	if err != nil {
		t.Fatalf("parsing the policy: %v", err)
	}
	if !policy.allowsOutlet(outletRef{mac: testUPSMAC, index: 5, name: "rack/nas"}) {
		t.Error("an entry whose name contains a slash did not match")
	}
	if policy.allowsOutlet(outletRef{mac: testUPSMAC, index: 5, name: "rack"}) {
		t.Error("the name was truncated at the first slash")
	}
	if policy.allowsOutlet(outletRef{mac: testUPSMAC, index: 6, name: "rack/nas"}) {
		t.Error("allowing outlet 5 also allowed outlet 6")
	}
}

// TestBothOutletReadersAgreeOnPlaceholderNames.
//
// Two places decide whether an outlet has been named: the state key, which
// falls back to the index when it has not, and the write path, which refuses.
// They now share one function, and this is what stops that being undone — if
// they ever diverged, an outlet could publish under a name it cannot be
// switched by, or read as unnamed while remaining switchable.
//
// This test caught exactly that when the two rules were written separately:
// "Outlet3" was a name to one and a placeholder to the other.
func TestBothOutletReadersAgreeOnPlaceholderNames(t *testing.T) {
	for name, placeholder := range map[string]bool{
		"Outlet 1": true, "Outlet 8": true, "outlet 3": true, " Outlet 5 ": true,
		"outlet-7": true, "Outlet_2": true,
		"test-nas": false, "Rack NAS": false, "Outlet cupboard": false, "3": false,
		"outlet-nas": false, "Outlet3": false,
	} {
		t.Run(name, func(t *testing.T) {
			if got := isPlaceholderOutletName(name); got != placeholder {
				t.Fatalf("isPlaceholderOutletName(%q) = %v, want %v", name, got, placeholder)
			}
			// The state key addresses a placeholder by its index and anything
			// else by its slug, which is the same reading expressed as an
			// address rather than as a refusal.
			index := 4
			suffix, addressable := outletSuffix(&index, name)
			if !addressable {
				t.Fatal("a named outlet with an index was not addressable")
			}
			if wantIndex := "4"; placeholder && suffix != wantIndex {
				t.Errorf("a placeholder published as outlet.%s, not by its index", suffix)
			}
			if slug := slugify(name); !placeholder && suffix != slug {
				t.Errorf("a name published as outlet.%s, not as outlet.%s", suffix, slug)
			}
		})
	}
}
