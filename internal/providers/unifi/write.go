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
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
)

// The Network application paths the write actions use. Unlike the Alarm Manager
// paths these carry the /proxy/network prefix the poller uses — but they
// authenticate the way the alarms API does, with a cookie session and the
// csrfToken claim from inside it, because the X-API-KEY header that reads
// stat/device does not write.
//
// EVERY ENDPOINT AND FIELD NAME BELOW IS INFERRED. No write to a real console
// has ever been made from this repository, and none of these paths appears in a
// committed capture. See docs/unifi-write-api.md, which splits what is known
// from what is assumed. The discipline that follows from that is the same one
// the Alarm Manager registration uses: check before writing, and abandon rather
// than guess.
const (
	wlanConfEndpoint = "rest/wlanconf"

	// fieldWLANID, fieldWLANName and fieldWLANEnabled are the three fields of a
	// WLAN record this package reads. Everything else in the record is carried
	// back to the console untouched.
	fieldWLANID      = "_id"
	fieldWLANName    = "name"
	fieldWLANEnabled = "enabled"
)

// defaultSite is the site every UniFi console has and the one Reactor reads
// and writes when the operator names none.
const defaultSite = "default"

// consoleWriteTimeout bounds the whole login-check-write exchange when an
// Automation names no timeout of its own. Each leg is bounded separately by the
// session client's own HTTP timeout, so this is the budget for the exchange
// rather than for any one request in it.
const consoleWriteTimeout = 30 * time.Second

// ActionsConfig is the operator's statement of what Reactor may change on this
// console. It is install-level configuration and deliberately absent from the
// Automation API, for exactly the reason the outbound destination allowlist is:
// spec.actions is writable by anyone who can create an Automation in their own
// namespace, and a namespace tenant must not be able to turn the WiFi off by
// writing one.
//
// It is empty by default, and empty refuses everything.
type ActionsConfig struct {
	// AllowedWLANs are the SSIDs unifi.wlan.enable and unifi.wlan.disable may
	// touch, matched exactly.
	AllowedWLANs []string
}

// splitList reads a comma-separated environment value into entries, dropping
// blanks so a trailing comma or an empty variable is not an entry.
func splitList(raw string) []string {
	var entries []string
	for entry := range strings.SplitSeq(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// WritePolicy is the parsed allowlist. Nothing outside it can be written, and
// there is no per-Automation override.
type WritePolicy struct {
	wlans map[string]bool
}

// NewWritePolicy parses the allowlist. It returns an error rather than dropping
// an entry it cannot read: silently ignoring one would leave an install that
// believes it allowed something refusing it at 3am instead.
func NewWritePolicy(cfg ActionsConfig) (WritePolicy, error) {
	policy := WritePolicy{wlans: map[string]bool{}}
	for _, name := range cfg.AllowedWLANs {
		policy.wlans[name] = true
	}
	return policy, nil
}

// Empty reports whether the policy allows nothing at all, which is the default
// and means every console write action is refused.
func (p WritePolicy) Empty() bool { return len(p.wlans) == 0 }

func (p WritePolicy) allowsWLAN(name string) bool { return p.wlans[name] }

// Writer performs the edge actions that write to the console.
//
// It holds no session. Each action opens one, uses it for the check and the
// write, and ends it — the same rule the qBittorrent action follows and for the
// same reason: a UniFi OS session cookie is a bearer of the same authority as
// the password that produced it, so caching one across reconciles would be
// exactly what this project refuses to do with the password itself. The cost is
// one extra round trip per action.
type Writer struct {
	baseURL  string
	site     string
	username string
	password string
	insecure bool
	policy   WritePolicy
}

// NewWriter builds the console writer from the provider configuration.
//
// It is constructed even when nothing is allowed, because a refusal that names
// the value to set is worth more than a nil that produces "no executor for
// action". Whether anything may actually be written is decided per action.
func NewWriter(cfg Config) (*Writer, error) {
	policy, err := NewWritePolicy(cfg.Actions)
	if err != nil {
		return nil, err
	}
	username, password := cfg.ConsoleCredentials()
	return &Writer{
		baseURL:  strings.TrimSuffix(cfg.URL, "/"),
		site:     siteOrDefault(cfg.Site),
		username: username,
		password: password,
		insecure: cfg.InsecureSkipVerify,
		policy:   policy,
	}, nil
}

func siteOrDefault(site string) string {
	if site == "" {
		return defaultSite
	}
	return site
}

// Enabled reports whether this install allows any console write at all. It is
// what the startup log reads; the refusal an Automation sees comes from Apply,
// which can say which list the missing entry belongs in.
func (w *Writer) Enabled() bool { return !w.policy.Empty() }

// Credentialed reports whether the console credentials the write path needs are
// present. The API key the poller uses does not work here, so an install that
// allows writes without them can do nothing, and should say so at startup
// rather than at the first outage.
func (w *Writer) Credentialed() bool { return w.username != "" && w.password != "" }

// Apply performs one console action.
//
// Every action follows the same three steps, and the order is the whole safety
// argument: log in, read the object and check it is the one the Automation
// meant, then write. A check that fails abandons the action with a sentence
// naming what did not match — it never writes anyway and it never writes
// something else.
//
// It is at-most-once and unconditionally so, which is recorded alongside the
// other per-type policies on maxActionAttempts. A WLAN write is a
// read-modify-write against an endpoint with no concurrency control, so a retry
// after an ambiguous failure re-reads a document the failed attempt may already
// have half-changed. The conservative reading of an ambiguous console write is
// that it happened.
func (w *Writer) Apply(
	ctx context.Context,
	action reactorv1alpha1.Action,
	timeout time.Duration,
) (actions.Result, error) {
	origin, err := describeConsoleTarget(action)
	if err != nil {
		return actions.Result{}, err
	}
	result := actions.Result{Origin: origin, Attempts: 1}

	if w.policy.Empty() {
		return result, errors.New(
			"console actions are disabled on this install: nothing is allowed, so this action was refused. " +
				"Set unifi.actions.allowedWlans to the SSIDs Reactor may change")
	}
	if !w.Credentialed() {
		return result, errors.New(
			"writing to the console needs UniFi OS console credentials (UNIFI_USERNAME and UNIFI_PASSWORD); " +
				"the API key the poller reads state with does not work on the write path")
	}
	if err := w.check(action); err != nil {
		return result, err
	}

	if timeout <= 0 {
		timeout = consoleWriteTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The same client the Alarm Manager registration uses, deliberately. It
	// already carries the one thing about this console that was expensive to
	// work out — that a write needs a cookie session plus the csrfToken claim
	// from inside that cookie — and a second implementation of it would be a
	// second place to get it wrong.
	client, err := NewAlarmClient(w.baseURL, w.username, w.password, w.insecure)
	if err != nil {
		return result, err
	}
	if err := client.Login(ctx); err != nil {
		return result, err
	}
	defer w.logout(ctx, client)

	return result, w.dispatch(ctx, client, action)
}

// check is everything that can be decided without talking to the console: is
// this action one this install allows at all. It runs before the login so a
// refused action never opens a session.
func (w *Writer) check(action reactorv1alpha1.Action) error {
	switch action.Type {
	case actions.TypeUniFiWLANEnable, actions.TypeUniFiWLANDisable:
		if !w.policy.allowsWLAN(action.WLAN.Name) {
			return fmt.Errorf(
				"wlan %q is not allowed by this install; add it to unifi.actions.allowedWlans, "+
					"which is empty by default and refuses every SSID",
				action.WLAN.Name)
		}
		return nil
	}
	return fmt.Errorf("no console executor for action %q", action.Type)
}

func (w *Writer) dispatch(ctx context.Context, client *AlarmClient, action reactorv1alpha1.Action) error {
	switch action.Type {
	case actions.TypeUniFiWLANEnable:
		return w.setWLANEnabled(ctx, client, action.WLAN.Name, true)
	case actions.TypeUniFiWLANDisable:
		return w.setWLANEnabled(ctx, client, action.WLAN.Name, false)
	}
	return fmt.Errorf("no console executor for action %q", action.Type)
}

// describeConsoleTarget names the object an action acts on, for status, Events
// and logs. The console's own address is deliberately not in it: it is install
// configuration, identical for every Automation, and what is worth reading is
// which object was touched.
func describeConsoleTarget(action reactorv1alpha1.Action) (string, error) {
	switch action.Type {
	case actions.TypeUniFiWLANEnable, actions.TypeUniFiWLANDisable:
		if action.WLAN == nil {
			return "", fmt.Errorf("%s needs a wlan block", action.Type)
		}
		return "unifi/wlan/" + action.WLAN.Name, nil
	}
	return "", fmt.Errorf("no console executor for action %q", action.Type)
}

// setWLANEnabled turns one wireless network on or off.
//
// The write is a read-modify-write, because that is what this endpoint offers:
// there is no field-level update and no version to compare against. Two things
// bound the damage that shape can do. Reactor sends back the object it just
// read with exactly one key changed, so it never invents a value for a field it
// does not understand — and it does not write at all when the WLAN is already
// where the Automation wants it, which is the common case for a repeated
// transition.
//
// What it cannot bound is the window: a change made in the UniFi UI between the
// read and the write is lost. That window is two adjacent requests wide, and it
// is stated in docs/unifi-write-api.md rather than pretended away.
func (w *Writer) setWLANEnabled(ctx context.Context, client *AlarmClient, name string, want bool) error {
	log := logf.FromContext(ctx).WithName("unifi-write")

	listed, err := client.do(ctx, http.MethodGet, w.networkPath(wlanConfEndpoint), nil)
	if err != nil {
		return err
	}
	record, found := findObjectWith(listed, fieldWLANName, name, fieldWLANID, fieldWLANEnabled)
	if !found {
		// Deliberately not listing the WLANs that do exist. This text reaches
		// status and Events, which anyone who can read the Automation can read,
		// and the network's SSIDs are not theirs to be told.
		return fmt.Errorf("no wlan named %q on site %q", name, w.site)
	}

	id, ok := record[fieldWLANID].(string)
	if !ok || id == "" {
		return fmt.Errorf("the wlan named %q carries no usable %s; refusing to guess at its address",
			name, fieldWLANID)
	}
	enabled, ok := record[fieldWLANEnabled].(bool)
	if !ok {
		return fmt.Errorf("the wlan named %q does not report %s as a boolean; refusing to write a state "+
			"this console does not describe the way this action assumes", name, fieldWLANEnabled)
	}
	if enabled == want {
		log.Info("The wlan is already in the wanted state; nothing written",
			"wlan", name, "enabled", want)
		return nil
	}

	body := maps.Clone(record)
	body[fieldWLANEnabled] = want
	updated, err := client.do(ctx, http.MethodPut, w.networkPath(wlanConfEndpoint)+"/"+url.PathEscape(id), body)
	if err != nil {
		return err
	}

	// The console answers a write with the object it stored. A 200 that did not
	// take is the failure mode an undocumented endpoint is most likely to have,
	// so it is checked rather than assumed.
	if applied, found := findObjectWith(updated, fieldWLANID, id, fieldWLANEnabled); found {
		if got, ok := applied[fieldWLANEnabled].(bool); ok && got != want {
			return fmt.Errorf("the console accepted the write but still reports wlan %q as %s=%v",
				name, fieldWLANEnabled, got)
		}
	}
	log.Info("Wrote the wlan state", "wlan", name, "enabled", want)
	return nil
}

// networkPath builds a site-scoped Network application path.
func (w *Writer) networkPath(endpoint string) string {
	return fmt.Sprintf("/proxy/network/api/s/%s/%s", url.PathEscape(w.site), endpoint)
}

// logout ends the session on the console rather than leaving it to expire.
//
// Best effort and deliberately silent: the action has already happened by the
// time this runs, and a session Reactor could not close is not a reason to tell
// an operator the action failed. The path is INFERRED like everything else
// here; a console that does not offer it simply lets the session age out, which
// is what would have happened anyway.
func (w *Writer) logout(ctx context.Context, client *AlarmClient) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, err := client.do(ctx, http.MethodPost, "/api/auth/logout", nil); err != nil {
		logf.FromContext(ctx).WithName("unifi-write").V(1).Info(
			"Could not end the console session; it will expire on its own", "reason", err.Error())
	}
}

// findObjectWith searches a decoded JSON document for an object whose key holds
// want and which also carries every one of the required keys.
//
// It searches structurally rather than assuming a path, for the reason
// jsonHasString does on the alarms API: neither the envelope these endpoints
// answer with nor the shape of a record is documented, and both move between
// UniFi OS versions. Requiring the other keys is what stops it matching some
// unrelated nested object that happens to have a name.
func findObjectWith(doc any, key, want string, required ...string) (map[string]any, bool) {
	switch value := doc.(type) {
	case []any:
		for _, item := range value {
			if found, ok := findObjectWith(item, key, want, required...); ok {
				return found, true
			}
		}
	case map[string]any:
		if got, ok := value[key].(string); ok && got == want && hasKeys(value, required) {
			return value, true
		}
		for _, item := range value {
			if found, ok := findObjectWith(item, key, want, required...); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func hasKeys(object map[string]any, keys []string) bool {
	for _, key := range keys {
		if _, present := object[key]; !present {
			return false
		}
	}
	return true
}
