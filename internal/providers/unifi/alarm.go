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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// The Alarm Manager API is undocumented and reverse-engineered; every constant
// here comes from
// https://reactor.robbeverhelst.com/contributing/unifi-alarm-manager-api/,
// observed on UniFi Network 10.5.67. It lives at the UniFi OS layer, so none of
// these paths carry the /proxy/network prefix the poller uses.
const (
	alarmLoginPath    = "/api/auth/login"
	alarmRulesPath    = "/api/v2/alarms/network"
	alarmManifestPath = "/api/v2/alarms/network/manifest"

	alarmWebhookActionID = "network:webhook"

	triggerInternetDisconnected = "network:internet_disconnected"
	triggerHighLatency          = "network:high_latency_detected"
	triggerPacketLoss           = "network:packet_loss_detected"

	csrfHeader        = "x-csrf-token"
	sessionCookieName = "TOKEN"

	// DefaultAlarmRuleTitle titles the rule Reactor creates, and is how it
	// recognizes its own rule on a later start.
	DefaultAlarmRuleTitle = "unifi-reactor"

	// maxErrorBodyBytes bounds how much of a console error response is quoted
	// back into the log. The API's validation errors are how this API was
	// mapped in the first place, so they are worth showing — but not unbounded.
	maxErrorBodyBytes = 512
)

// DefaultAlarmTriggers are the Internet-category triggers that plausibly
// precede a WAN state change on Network 10.5.67. Reactor asks for all of them
// and keeps whichever the console's own manifest confirms it offers: which
// triggers exist varies by version and by how many uplinks are configured.
//
// Nothing downstream depends on which trigger fired. They are all just "look
// at the console again".
var DefaultAlarmTriggers = []string{
	triggerInternetDisconnected,
	triggerHighLatency,
	triggerPacketLoss,
}

// AlarmClient talks to the UniFi OS Alarm Manager API. It is separate from
// Client because it authenticates completely differently: the alarms API sits
// above the Network application and rejects the X-API-KEY header, requiring a
// cookie session plus a CSRF token carried inside that cookie.
type AlarmClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
	csrf     string
}

// NewAlarmClient builds a client with its own cookie jar; the session cookie
// never leaves it.
func NewAlarmClient(baseURL, username, password string, insecureSkipVerify bool) (*AlarmClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("creating a cookie jar for the alarms API: %w", err)
	}
	return &AlarmClient{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		username: username,
		password: password,
		http: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}, // #nosec G402 -- self-signed UniFi OS cert, opt-in
			},
		},
	}, nil
}

// Login opens a UniFi OS session and captures the CSRF token every mutating
// request has to echo.
func (c *AlarmClient) Login(ctx context.Context) error {
	body, err := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	if err != nil {
		return fmt.Errorf("encoding the login request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+alarmLoginPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("logging in to unifi os: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("logging in to unifi os: unexpected status %d", resp.StatusCode)
	}

	// The csrfToken claim inside the session JWT is what write requests must
	// match. The response header of the same name is only a fallback for a
	// console whose cookie does not look like a JWT.
	for _, cookie := range resp.Cookies() {
		if cookie.Name != sessionCookieName {
			continue
		}
		if token, err := csrfFromSessionJWT(cookie.Value); err == nil {
			c.csrf = token
			return nil
		}
	}
	if header := resp.Header.Get(csrfHeader); header != "" {
		c.csrf = header
		return nil
	}
	return errors.New("logging in to unifi os: no csrf token in the session cookie or response headers")
}

// csrfFromSessionJWT pulls the csrfToken claim out of the session cookie. Only
// the payload segment is decoded: the signature is the console's business, and
// this value is echoed back to the same console that issued it.
func csrfFromSessionJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", errors.New("the session cookie is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("decoding the session cookie payload: %w", err)
	}
	var claims struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parsing the session cookie payload: %w", err)
	}
	if claims.CSRFToken == "" {
		return "", errors.New("the session cookie carries no csrfToken claim")
	}
	return claims.CSRFToken, nil
}

// do issues one alarms API request and decodes the response into an untyped
// document. Untyped is deliberate: the GET representation is not the shape POST
// accepts, neither is documented, and both move between UniFi OS versions.
// Reading them structurally keeps a console reorganizing its JSON from turning
// into a decode error.
func (c *AlarmClient) do(ctx context.Context, method, path string, body any) (any, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding the alarms request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(csrfHeader, c.csrf)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode,
			strings.TrimSpace(string(detail)))
	}

	var decoded any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decoding the response to %s %s: %w", method, path, err)
	}
	return decoded, nil
}

// alarmRule is the create-rule body. triggers_data and actions_data are arrays
// of arrays — the API rejects a flat array of objects — and this is not the
// shape GET returns. See
// https://reactor.robbeverhelst.com/contributing/unifi-alarm-manager-api/.
type alarmRule struct {
	Title        string          `json:"title"`
	Scope        alarmScope      `json:"scope"`
	TriggersData [][]alarmMember `json:"triggers_data"`
	ActionsData  [][]alarmMember `json:"actions_data"`
}

type alarmScope struct {
	Mode string            `json:"mode"`
	Data map[string]string `json:"data"`
}

type alarmMember struct {
	ID   string `json:"id"`
	Data any    `json:"data"`
}

// AlarmRegistrar creates Reactor's own Alarm Manager rule on the console, so
// the fast path can be switched on without anyone clicking through the UniFi
// UI.
//
// Every failure is logged and swallowed. This writes to the operator's gateway
// over an undocumented, version-fragile API on every start; when the console
// does not look the way the reverse-engineering notes describe, Reactor says so
// and keeps polling rather than pushing a guess onto their hardware.
type AlarmRegistrar struct {
	Client *AlarmClient
	// CallbackURL is the address the console will post to. It must be
	// reachable from the console, which Reactor has no way to verify.
	CallbackURL string
	// Token is the shared secret the console should present on each delivery.
	Token string
	// RuleTitle identifies Reactor's rule.
	RuleTitle string
	// Triggers are the trigger IDs to ask for, filtered against what the
	// console's manifest actually offers.
	Triggers []string
}

// NewAlarmRegistrar builds a registrar from the provider configuration.
func NewAlarmRegistrar(cfg Config) (*AlarmRegistrar, error) {
	client, err := NewAlarmClient(cfg.URL, cfg.Webhook.Username, cfg.Webhook.Password, cfg.InsecureSkipVerify)
	if err != nil {
		return nil, err
	}
	return &AlarmRegistrar{
		Client:      client,
		CallbackURL: cfg.Webhook.PublicURL,
		Token:       cfg.Webhook.Token,
		RuleTitle:   cfg.Webhook.RuleTitle,
		Triggers:    DefaultAlarmTriggers,
	}, nil
}

// Start implements manager.Runnable. It registers once and returns: the rule
// lives on the console, so there is nothing to keep running, and nothing here
// is allowed to fail loudly enough to matter.
func (a *AlarmRegistrar) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("unifi-alarm-registrar")
	if err := a.register(logf.IntoContext(ctx, log)); err != nil {
		log.Error(err, "Could not register the Alarm Manager rule; UniFi state still converges on the poll interval",
			"rule", a.title())
	}
	return nil
}

// NeedLeaderElection reports true: exactly one replica should write to the
// console.
func (a *AlarmRegistrar) NeedLeaderElection() bool { return true }

func (a *AlarmRegistrar) title() string {
	if a.RuleTitle == "" {
		return DefaultAlarmRuleTitle
	}
	return a.RuleTitle
}

func (a *AlarmRegistrar) register(ctx context.Context) error {
	log := logf.FromContext(ctx)
	if a.Token == "" {
		// Registering a rule the receiver would reject on arrival is worse than
		// not registering one: the console reports a working alarm action while
		// every delivery is refused.
		return errors.New("refusing to register a rule without a shared secret, which the receiver would reject")
	}
	if err := validateCallbackURL(a.CallbackURL); err != nil {
		return err
	}
	if err := a.Client.Login(ctx); err != nil {
		return err
	}

	manifest, err := a.Client.do(ctx, http.MethodGet, alarmManifestPath, nil)
	if err != nil {
		return err
	}
	if !jsonHasString(manifest, alarmWebhookActionID) {
		return fmt.Errorf("this console's alarm manifest does not offer the %q action", alarmWebhookActionID)
	}
	triggers := make([]string, 0, len(a.Triggers))
	for _, id := range a.Triggers {
		if jsonHasString(manifest, id) {
			triggers = append(triggers, id)
			continue
		}
		log.Info("Skipping a trigger this console does not offer", "trigger", id)
	}
	if len(triggers) == 0 {
		return errors.New("this console's alarm manifest offers none of the triggers Reactor asks for")
	}

	rules, err := a.Client.do(ctx, http.MethodGet, alarmRulesPath, nil)
	if err != nil {
		return err
	}
	if id, found := findRuleID(rules, a.title()); found {
		// Reactor creates its rule and then leaves it alone forever. Editing
		// and deleting are verbs the reverse-engineering notes never confirmed
		// against a real console, and guessing them wrong means silently
		// breaking somebody's alerting.
		log.Info("Alarm Manager rule already exists; leaving it untouched",
			"rule", a.title(), "id", id,
			"note", "delete it in the UniFi UI if its destination no longer matches")
		return nil
	}

	created, err := a.Client.do(ctx, http.MethodPost, alarmRulesPath, a.ruleBody(triggers))
	if err != nil {
		return err
	}
	id, _ := findRuleID(created, a.title())
	log.Info("Created the Alarm Manager rule", "rule", a.title(), "id", id, "triggers", triggers)
	return nil
}

func (a *AlarmRegistrar) ruleBody(triggers []string) alarmRule {
	members := make([]alarmMember, 0, len(triggers))
	for _, id := range triggers {
		members = append(members, alarmMember{ID: id, Data: map[string]any{}})
	}
	return alarmRule{
		Title: a.title(),
		Scope: alarmScope{Mode: "include", Data: map[string]string{"site_id": "ALL_ITEMS"}},
		// One inner array each: the outer sequence is how the API models
		// alternatives, and Reactor only ever wants the one group.
		TriggersData: [][]alarmMember{members},
		ActionsData: [][]alarmMember{{{
			ID: alarmWebhookActionID,
			Data: map[string]any{
				"url":    a.CallbackURL,
				"method": http.MethodPost,
				// The manifest documents variants none|basic|bearer. The field
				// name carrying the bearer value is INFERRED, not verified
				// against a console: if a console rejects this body, the error
				// it returns is logged verbatim and the rule can be created by
				// hand in the UniFi UI instead — the receiver also accepts the
				// secret in an X-Reactor-Token header, which the Alarm
				// Manager's custom-headers list can send.
				"auth": map[string]any{"variant": "bearer", "token": a.Token},
			},
		}}},
	}
}

// validateCallbackURL rejects up front what the console rejects on submission,
// so a bad value fails with a sentence instead of an opaque 400 from the
// gateway. It cannot check the one thing that matters most — whether the
// console can actually reach this address.
func validateCallbackURL(raw string) error {
	if raw == "" {
		return errors.New("webhook self-registration needs the public URL the console should post to")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing the public webhook URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("the public webhook URL must be http or https, got %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("the public webhook URL %q has no host", raw)
	}
	// The console refuses loopback destinations, and it would be right to: a
	// loopback address there means the gateway itself, not this pod.
	if strings.EqualFold(host, "localhost") {
		return errors.New("the public webhook URL must be an address the console can reach, not localhost")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return fmt.Errorf("the public webhook URL must be an address the console can reach, not the loopback %s", ip)
	}
	return nil
}

// jsonHasString reports whether want appears as a string value anywhere in a
// decoded JSON document.
//
// The Alarm Manager manifest is a large, undocumented, version-dependent
// catalog of trigger and action IDs. Asking whether an ID is in it at all keeps
// working when the catalog is reorganized; hard-coding a path through it would
// turn a cosmetic change into a broken fast path.
func jsonHasString(doc any, want string) bool {
	switch value := doc.(type) {
	case string:
		return value == want
	case []any:
		for _, item := range value {
			if jsonHasString(item, want) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if jsonHasString(item, want) {
				return true
			}
		}
	}
	return false
}

// findRuleID looks for a rule object carrying the given title and returns its
// id. Whether the list arrives bare or wrapped in an envelope is not
// documented, so this finds the object rather than assuming where it lives.
func findRuleID(doc any, title string) (string, bool) {
	switch value := doc.(type) {
	case []any:
		for _, item := range value {
			if id, found := findRuleID(item, title); found {
				return id, true
			}
		}
	case map[string]any:
		if got, ok := value["title"].(string); ok && got == title {
			id, _ := value["id"].(string)
			return id, true
		}
		for _, item := range value {
			if id, found := findRuleID(item, title); found {
				return id, true
			}
		}
	}
	return "", false
}
