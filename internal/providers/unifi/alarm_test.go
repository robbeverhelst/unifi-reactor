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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	testCSRF            = "csrf-token-from-the-jwt"
	testConsoleUser     = "reactor"
	testConsolePassword = "hunter2"

	keyTriggers = "triggers"
	keyActions  = "actions"
	keyTitle    = "title"
)

// testCtx carries a logger the way the manager does, since register reads it
// from the context rather than taking one.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	return logf.IntoContext(t.Context(), testr.New(t))
}

// sessionJWT builds a cookie shaped like the one UniFi OS issues: three
// dot-separated segments whose middle one is a base64url JSON payload carrying
// the csrfToken claim. Only that payload is ever read, so the header and
// signature are arbitrary.
func sessionJWT(t *testing.T, claims map[string]string) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encoding the claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// fakeConsole stands in for a UniFi OS console's alarms API, matching the
// shapes recorded in docs/unifi-alarm-manager-api.md.
type fakeConsole struct {
	manifest any
	rules    []any

	// created records the bodies POSTed to the rules endpoint, so a test can
	// assert on the arrays-of-arrays shape the API demands.
	created []map[string]any
	// csrfSeen records the CSRF header presented on the create request.
	csrfSeen string

	loginStatus    int
	manifestStatus int
	createStatus   int
	// noCSRFClaim issues a session cookie without the csrfToken claim.
	noCSRFClaim bool
	// csrfHeaderOnly issues a non-JWT cookie plus the response header fallback.
	csrfHeaderOnly bool
}

func (f *fakeConsole) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST "+alarmLoginPath, func(w http.ResponseWriter, _ *http.Request) {
		if f.loginStatus != 0 {
			w.WriteHeader(f.loginStatus)
			return
		}
		switch {
		case f.csrfHeaderOnly:
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "not-a-jwt"})
			w.Header().Set(csrfHeader, testCSRF)
		case f.noCSRFClaim:
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sessionJWT(t, map[string]string{"sub": "x"})})
		default:
			http.SetCookie(w, &http.Cookie{
				Name:  sessionCookieName,
				Value: sessionJWT(t, map[string]string{"csrfToken": testCSRF}),
			})
		}
		writeJSON(t, w, map[string]any{"unique_id": "00000000-0000-0000-0000-0000000000ff"})
	})

	mux.HandleFunc("GET "+alarmManifestPath, func(w http.ResponseWriter, _ *http.Request) {
		if f.manifestStatus != 0 {
			http.Error(w, "manifest unavailable", f.manifestStatus)
			return
		}
		writeJSON(t, w, f.manifest)
	})

	mux.HandleFunc("GET "+alarmRulesPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{envelopeData: f.rules})
	})

	mux.HandleFunc("POST "+alarmRulesPath, func(w http.ResponseWriter, r *http.Request) {
		f.csrfSeen = r.Header.Get(csrfHeader)
		if f.createStatus != 0 {
			http.Error(w, `{"message":"triggers_data: invalid type: sequence, expected a sequence of sequences"}`,
				f.createStatus)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the create body: %v", err)
		}
		f.created = append(f.created, body)
		body["id"] = "019ff10d-0000-0000-0000-000000000001"
		writeJSON(t, w, body)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("writing the response: %v", err)
	}
}

// fullManifest nests the identifiers the way a catalog plausibly would, to
// prove the lookup does not depend on where in the document they sit.
func fullManifest() any {
	return map[string]any{
		keyTriggers: []any{
			map[string]any{
				"id": "network:category_internet",
				"items": []any{
					map[string]any{"id": triggerInternetDisconnected, "options": map[string]any{}},
					map[string]any{"id": triggerHighLatency},
					map[string]any{"id": triggerPacketLoss},
					map[string]any{"id": "network:data_limit"},
				},
			},
		},
		keyActions: []any{
			map[string]any{"id": alarmWebhookActionID, "schema": map[string]any{"type": "object"}},
			map[string]any{"id": "network:slack"},
		},
	}
}

func testRegistrar(t *testing.T, console *httptest.Server) *AlarmRegistrar {
	t.Helper()
	registrar, err := NewAlarmRegistrar(Config{
		URL: console.URL,
		Webhook: WebhookConfig{
			Username:  testConsoleUser,
			Password:  testConsolePassword,
			Token:     testToken,
			PublicURL: "http://reactor.example.test:9090" + DefaultWebhookPath,
			RuleTitle: DefaultAlarmRuleTitle,
		},
	})
	if err != nil {
		t.Fatalf("building the registrar: %v", err)
	}
	return registrar
}

func TestRegistrarCreatesTheRuleInTheShapeTheAPIDemands(t *testing.T) {
	console := &fakeConsole{manifest: fullManifest()}
	server := console.start(t)
	registrar := testRegistrar(t, server)

	if err := registrar.register(testCtx(t)); err != nil {
		t.Fatalf("registering: %v", err)
	}
	if len(console.created) != 1 {
		t.Fatalf("expected exactly one rule created, got %d", len(console.created))
	}
	if console.csrfSeen != testCSRF {
		t.Errorf("expected the csrf token from the session cookie, got %q", console.csrfSeen)
	}

	rule := console.created[0]
	if rule["title"] != DefaultAlarmRuleTitle {
		t.Errorf("expected the rule to be titled %q, got %v", DefaultAlarmRuleTitle, rule["title"])
	}

	// triggers_data and actions_data are sequences of sequences; a flat array
	// of objects is rejected by the real API.
	triggers, ok := rule["triggers_data"].([]any)
	if !ok || len(triggers) != 1 {
		t.Fatalf("expected triggers_data to be one outer array, got %#v", rule["triggers_data"])
	}
	inner, ok := triggers[0].([]any)
	if !ok || len(inner) != len(DefaultAlarmTriggers) {
		t.Fatalf("expected %d triggers in the inner array, got %#v", len(DefaultAlarmTriggers), triggers[0])
	}

	actions, ok := rule["actions_data"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("expected actions_data to be one outer array, got %#v", rule["actions_data"])
	}
	action := actions[0].([]any)[0].(map[string]any)
	if action["id"] != alarmWebhookActionID {
		t.Errorf("expected the %q action, got %v", alarmWebhookActionID, action["id"])
	}
	data := action[envelopeData].(map[string]any)
	if data["url"] != registrar.CallbackURL || data["method"] != http.MethodPost {
		t.Errorf("expected a POST to the callback URL, got %v %v", data["method"], data["url"])
	}
	if auth := data["auth"].(map[string]any); auth["token"] != testToken {
		t.Errorf("expected the shared secret to be registered with the rule, got %v", auth)
	}
}

// A console whose catalog does not offer the webhook action must be left
// alone, not written to on the assumption the notes still hold.
func TestRegistrarWritesNothingWhenTheConsoleOffersNoWebhookAction(t *testing.T) {
	console := &fakeConsole{manifest: map[string]any{
		keyActions: []any{map[string]any{"id": "network:slack"}},
		keyTriggers: []any{
			map[string]any{"id": triggerInternetDisconnected},
		},
	}}
	registrar := testRegistrar(t, console.start(t))

	if err := registrar.register(testCtx(t)); err == nil {
		t.Error("expected registration to be refused when the webhook action is absent")
	}
	if len(console.created) != 0 {
		t.Errorf("expected nothing to be written to the console, got %d rules", len(console.created))
	}
}

// Triggers the console does not know about are dropped rather than sent, since
// which triggers exist varies by version and uplink count.
func TestRegistrarAsksOnlyForTriggersTheConsoleOffers(t *testing.T) {
	console := &fakeConsole{manifest: map[string]any{
		keyActions:  []any{map[string]any{"id": alarmWebhookActionID}},
		keyTriggers: []any{map[string]any{"id": triggerInternetDisconnected}},
	}}
	registrar := testRegistrar(t, console.start(t))

	if err := registrar.register(testCtx(t)); err != nil {
		t.Fatalf("registering: %v", err)
	}
	inner := console.created[0]["triggers_data"].([]any)[0].([]any)
	if len(inner) != 1 {
		t.Fatalf("expected only the offered trigger, got %#v", inner)
	}
	if id := inner[0].(map[string]any)["id"]; id != triggerInternetDisconnected {
		t.Errorf("expected the offered trigger, got %v", id)
	}
}

func TestRegistrarWritesNothingWhenNoTriggerIsOffered(t *testing.T) {
	console := &fakeConsole{manifest: map[string]any{
		keyActions:  []any{map[string]any{"id": alarmWebhookActionID}},
		keyTriggers: []any{map[string]any{"id": "network:something_else"}},
	}}
	registrar := testRegistrar(t, console.start(t))

	if err := registrar.register(testCtx(t)); err == nil {
		t.Error("expected registration to be refused when no requested trigger exists")
	}
	if len(console.created) != 0 {
		t.Errorf("expected nothing to be written to the console, got %d rules", len(console.created))
	}
}

// Reactor creates its rule once and never touches it again: editing and
// deleting are verbs the reverse-engineering notes never confirmed.
func TestRegistrarLeavesAnExistingRuleAlone(t *testing.T) {
	console := &fakeConsole{
		manifest: fullManifest(),
		rules: []any{map[string]any{
			"id":     "019ff10d-5b3e-7930-0000-000000000000",
			keyTitle: DefaultAlarmRuleTitle,
		}},
	}
	registrar := testRegistrar(t, console.start(t))

	if err := registrar.register(testCtx(t)); err != nil {
		t.Fatalf("registering: %v", err)
	}
	if len(console.created) != 0 {
		t.Errorf("expected an existing rule to be left untouched, got %d writes", len(console.created))
	}
}

// Somebody else's rules must not be mistaken for Reactor's own.
func TestRegistrarIgnoresRulesItDoesNotOwn(t *testing.T) {
	console := &fakeConsole{
		manifest: fullManifest(),
		rules: []any{
			map[string]any{"id": "a", keyTitle: "Page me on packet loss"},
			map[string]any{"id": "b", keyTitle: "unifi-reactor-old"},
		},
	}
	registrar := testRegistrar(t, console.start(t))

	if err := registrar.register(testCtx(t)); err != nil {
		t.Fatalf("registering: %v", err)
	}
	if len(console.created) != 1 {
		t.Errorf("expected Reactor's own rule to be created, got %d writes", len(console.created))
	}
}

func TestRegistrarSurfacesTheConsolesOwnValidationError(t *testing.T) {
	console := &fakeConsole{manifest: fullManifest(), createStatus: http.StatusBadRequest}
	registrar := testRegistrar(t, console.start(t))

	err := registrar.register(testCtx(t))
	if err == nil {
		t.Fatal("expected the rejected create to be reported")
	}
	// The console's own message is what makes this API debuggable at all.
	if !strings.Contains(err.Error(), "sequence of sequences") {
		t.Errorf("expected the console's validation message to be reported, got %v", err)
	}
}

func TestRegistrarFailsSoftAndNeverStopsTheManager(t *testing.T) {
	for name, console := range map[string]*fakeConsole{
		"login refused":       {loginStatus: http.StatusUnauthorized, manifest: fullManifest()},
		"no csrf claim":       {noCSRFClaim: true, manifest: fullManifest()},
		"manifest unreadable": {manifest: fullManifest(), manifestStatus: http.StatusInternalServerError},
		"create rejected":     {manifest: fullManifest(), createStatus: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			registrar := testRegistrar(t, console.start(t))
			if err := registrar.Start(context.Background()); err != nil {
				t.Errorf("Start must swallow every failure, got %v", err)
			}
		})
	}
}

// The response header is only a fallback: the claim inside the cookie is what
// the notes say write requests must match.
func TestLoginFallsBackToTheCSRFResponseHeader(t *testing.T) {
	console := &fakeConsole{manifest: fullManifest(), csrfHeaderOnly: true}
	registrar := testRegistrar(t, console.start(t))

	if err := registrar.register(testCtx(t)); err != nil {
		t.Fatalf("registering: %v", err)
	}
	if console.csrfSeen != testCSRF {
		t.Errorf("expected the header fallback to be used, got %q", console.csrfSeen)
	}
}

func TestCSRFFromSessionJWT(t *testing.T) {
	valid := sessionJWT(t, map[string]string{"csrfToken": testCSRF})
	got, err := csrfFromSessionJWT(valid)
	if err != nil || got != testCSRF {
		t.Errorf("expected %q, got %q (%v)", testCSRF, got, err)
	}

	for name, cookie := range map[string]string{
		"not a jwt":     "opaque-session-value",
		"bad base64":    "header.!!!not-base64!!!.signature",
		"not json":      "header." + base64.RawURLEncoding.EncodeToString([]byte("plain text")) + ".signature",
		"no csrf claim": sessionJWT(t, map[string]string{"sub": "reactor"}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := csrfFromSessionJWT(cookie); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// The console rejects loopback destinations, and would be right to: loopback
// there means the gateway itself, not the Reactor pod.
func TestValidateCallbackURL(t *testing.T) {
	for name, raw := range map[string]string{
		"no url":         "",
		"no scheme":      "reactor.example.test:9090/webhooks/unifi",
		"wrong scheme":   "ftp://reactor.example.test/webhooks/unifi",
		"no host":        "http:///webhooks/unifi",
		"localhost":      "http://localhost:9090/webhooks/unifi",
		"loopback ipv4":  "http://127.0.0.1:9090/webhooks/unifi",
		"loopback ipv6":  "http://[::1]:9090/webhooks/unifi",
		"uppercase host": "http://LOCALHOST:9090/webhooks/unifi",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCallbackURL(raw); err == nil {
				t.Errorf("expected %q to be rejected", raw)
			}
		})
	}
	for name, raw := range map[string]string{
		"http by name":  "http://reactor.example.test:9090/webhooks/unifi",
		"https by name": "https://reactor.example.test/webhooks/unifi",
		"by address":    "http://198.51.100.20:9090/webhooks/unifi",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCallbackURL(raw); err != nil {
				t.Errorf("expected %q to be accepted, got %v", raw, err)
			}
		})
	}
}

func TestJSONHasString(t *testing.T) {
	doc := fullManifest()
	for _, want := range DefaultAlarmTriggers {
		if !jsonHasString(doc, want) {
			t.Errorf("expected %q to be found", want)
		}
	}
	if !jsonHasString(doc, alarmWebhookActionID) {
		t.Errorf("expected %q to be found", alarmWebhookActionID)
	}
	for _, absent := range []string{"network:wan_failover", "", "network:internet_disconnecte"} {
		if jsonHasString(doc, absent) {
			t.Errorf("did not expect %q to be found", absent)
		}
	}
}

// Neither the bare nor the wrapped list shape is documented, so both must work.
func TestFindRuleID(t *testing.T) {
	rule := map[string]any{"id": "rule-1", keyTitle: DefaultAlarmRuleTitle}
	for name, doc := range map[string]any{
		"bare array":      []any{rule},
		"wrapped in data": map[string]any{envelopeData: []any{rule}},
		"deeply nested":   map[string]any{"result": map[string]any{"items": []any{rule}}},
	} {
		t.Run(name, func(t *testing.T) {
			id, found := findRuleID(doc, DefaultAlarmRuleTitle)
			if !found || id != "rule-1" {
				t.Errorf("expected rule-1, got %q (found %v)", id, found)
			}
		})
	}

	if _, found := findRuleID([]any{rule}, "some other rule"); found {
		t.Error("did not expect a different title to match")
	}
	// A rule with the right title but no readable id still counts as existing:
	// the point is to not create a second one.
	id, found := findRuleID([]any{map[string]any{keyTitle: DefaultAlarmRuleTitle}}, DefaultAlarmRuleTitle)
	if !found || id != "" {
		t.Errorf("expected a titled rule with no id to be found, got %q (found %v)", id, found)
	}
}
