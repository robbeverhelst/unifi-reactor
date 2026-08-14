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
	"regexp"
	"strconv"
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
	// deviceStatEndpoint is the same read the poller makes. The PoE check goes
	// through the session rather than the API key so that one action is one
	// authenticated exchange, and so a console that stops accepting the key for
	// reads does not leave the check silently unmade.
	deviceStatEndpoint = "stat/device"
	// devMgrEndpoint is the device command endpoint, and cmdPowerCycle is the
	// command a PoE cycle sends to it.
	devMgrEndpoint = "cmd/devmgr"
	cmdPowerCycle  = "power-cycle"

	// fieldWLANID, fieldWLANName and fieldWLANEnabled are the three fields of a
	// WLAN record this package reads. Everything else in the record is carried
	// back to the console untouched.
	fieldWLANID      = "_id"
	fieldWLANName    = "name"
	fieldWLANEnabled = "enabled"

	// The device and port_table fields the PoE check reads. Every one of them
	// is a refusal if it is absent: a guard that silently does not apply is
	// worse than one that declines.
	//
	// poe.go reads the same port_table through a typed struct to measure the
	// PoE budget for the poe state key. That reader omits its key where this
	// one refuses, which is why they are not one decoder;
	// TestBothPoEReadersAgreeOnOnePortTable holds them to the same field names.
	fieldMAC       = "mac"
	fieldPortTable = "port_table"
	fieldPortIndex = "port_idx"
	fieldPortName  = "name"
	fieldIsUplink  = "is_uplink"
	fieldPortPoE   = "port_poe"
	fieldPoEEnable = "poe_enable"
)

// defaultSite is the site every UniFi console has and the one Reactor reads
// and writes when the operator names none.
const defaultSite = "default"

// consoleWriteTimeout bounds the whole login-check-write exchange when an
// Automation names no timeout of its own. Each leg is bounded separately by the
// session client's own HTTP timeout, so this is the budget for the exchange
// rather than for any one request in it.
const consoleWriteTimeout = 30 * time.Second

// macPattern is the address form this package compares on: lowercase,
// colon-separated. It is enforced again at admission on the API type, so an
// Automation carrying anything else never reaches here — this is for the
// allowlist, which comes from Helm values and is not validated by anything
// else.
var macPattern = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// ActionsConfig is the operator's statement of what Reactor may change on this
// console. It is install-level configuration and deliberately absent from the
// Automation API, for exactly the reason the outbound destination allowlist is:
// spec.actions is writable by anyone who can create an Automation in their own
// namespace, and a namespace tenant must not be able to turn the WiFi off or
// cut power to a switch port by writing one.
//
// Both lists are empty by default, and empty refuses everything.
type ActionsConfig struct {
	// AllowedWLANs are the SSIDs unifi.wlan.enable and unifi.wlan.disable may
	// touch, matched exactly.
	AllowedWLANs []string
	// AllowedPoEPorts are the switch ports unifi.poe.cycle may power-cycle, as
	// "<mac>/<port index>".
	AllowedPoEPorts []string
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

// portRef identifies one switch port: which switch, and which port on it.
type portRef struct {
	mac  string
	port int32
}

func (p portRef) String() string { return fmt.Sprintf("%s/%d", p.mac, p.port) }

// WritePolicy is the parsed allowlist. Nothing outside it can be written, and
// there is no per-Automation override.
type WritePolicy struct {
	wlans map[string]bool
	ports map[portRef]bool
}

// NewWritePolicy parses the allowlists. It returns an error rather than
// dropping an entry it cannot read: silently ignoring one would leave an
// install that believes it allowed something refusing it at 3am instead.
func NewWritePolicy(cfg ActionsConfig) (WritePolicy, error) {
	policy := WritePolicy{wlans: map[string]bool{}, ports: map[portRef]bool{}}
	for _, name := range cfg.AllowedWLANs {
		policy.wlans[name] = true
	}
	for _, entry := range cfg.AllowedPoEPorts {
		ref, err := parsePortRef(entry)
		if err != nil {
			return WritePolicy{}, err
		}
		policy.ports[ref] = true
	}
	return policy, nil
}

// parsePortRef reads one "<mac>/<port>" allowlist entry.
//
// Both halves are required, and that is the point of the format rather than an
// accident of it: a port index on its own means something different after
// somebody re-patches a rack, so an allowlist written in indices alone would go
// on allowing whatever ends up in slot 7.
func parsePortRef(entry string) (portRef, error) {
	mac, index, found := strings.Cut(strings.TrimSpace(entry), "/")
	if !found {
		return portRef{}, fmt.Errorf(
			"allowed PoE port %q must be a switch MAC and a port index, e.g. aa:bb:cc:00:11:22/7", entry)
	}
	mac = NormalizeMAC(mac)
	if !macPattern.MatchString(mac) {
		return portRef{}, fmt.Errorf("allowed PoE port %q does not start with a MAC address", entry)
	}
	port, err := strconv.ParseInt(strings.TrimSpace(index), 10, 32)
	if err != nil || port < 1 {
		return portRef{}, fmt.Errorf("allowed PoE port %q has no positive port index after the MAC", entry)
	}
	return portRef{mac: mac, port: int32(port)}, nil
}

// NormalizeMAC puts an address in the form everything here compares on:
// lowercase and colon-separated. Consoles and humans write MACs several ways,
// and an allowlist that failed to match because of a capital letter would be a
// support burden rather than a control.
func NormalizeMAC(mac string) string {
	mac = strings.ToLower(strings.TrimSpace(mac))
	return strings.ReplaceAll(strings.ReplaceAll(mac, "-", ":"), ".", ":")
}

// Empty reports whether the policy allows nothing at all, which is the default
// and means every console write action is refused.
func (p WritePolicy) Empty() bool { return len(p.wlans) == 0 && len(p.ports) == 0 }

func (p WritePolicy) allowsWLAN(name string) bool { return p.wlans[name] }

func (p WritePolicy) allowsPort(ref portRef) bool { return p.ports[ref] }

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
// other per-type policies on maxActionAttempts. A PoE cycle is a power cut, and
// repeating one after an ambiguous failure is a second power cut rather than a
// correction. A WLAN write is a read-modify-write against an endpoint with no
// concurrency control, so a retry re-reads a document the failed attempt may
// already have half-changed. In both cases the conservative reading of an
// ambiguous console write is that it happened.
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
				"Set unifi.actions.allowedWlans or unifi.actions.allowedPoePorts " +
				"to the objects Reactor may change")
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
	case actions.TypeUniFiPoECycle:
		ref := portRef{mac: NormalizeMAC(action.PoE.Device), port: action.PoE.Port}
		if !w.policy.allowsPort(ref) {
			return fmt.Errorf(
				"port %s is not allowed by this install; add it to unifi.actions.allowedPoePorts, "+
					"which is empty by default and refuses every port",
				ref)
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
	case actions.TypeUniFiPoECycle:
		return w.cyclePoEPort(ctx, client, *action.PoE)
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
	case actions.TypeUniFiPoECycle:
		if action.PoE == nil {
			return "", fmt.Errorf("%s needs a poe block", action.Type)
		}
		return fmt.Sprintf("unifi/port/%s/%d", NormalizeMAC(action.PoE.Device), action.PoE.Port), nil
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

// cyclePoEPort cuts and restores power to one switch port.
//
// Almost all of this function is the check, and that is the right proportion.
// The command itself is three fields; what makes it safe is everything read
// from the switch's own port table before it is sent, because the console will
// accept a cycle of the wrong port exactly as readily as the right one and
// Reactor would never hear about the difference.
//
// Each refusal below is a distinct way of being wrong about which port this is,
// and each names what did not match rather than saying the action failed:
//
//   - the switch is not there, or is not the one this MAC names
//   - the port index does not exist on it
//   - the port is called something else now, which is what a re-patched rack
//     looks like from here
//   - the port is the switch's uplink, which carries everything behind it
//   - the port is not PoE-capable, or the switch does not say whether it is
func (w *Writer) cyclePoEPort(ctx context.Context, client *AlarmClient, spec reactorv1alpha1.PoEPort) error {
	log := logf.FromContext(ctx).WithName("unifi-write")
	mac := NormalizeMAC(spec.Device)

	devices, err := client.do(ctx, http.MethodGet, w.networkPath(deviceStatEndpoint), nil)
	if err != nil {
		return err
	}
	device, found := findObjectWith(devices, fieldMAC, mac, fieldPortTable)
	if !found {
		return fmt.Errorf("no device with mac %s reporting a port table on site %q", mac, w.site)
	}
	port, found := portByIndex(device[fieldPortTable], spec.Port)
	if !found {
		return fmt.Errorf("device %s has no port %d", mac, spec.Port)
	}
	if err := checkPort(mac, spec, port); err != nil {
		return err
	}

	// port_idx rather than the array position: the table is not guaranteed to
	// be ordered or complete, and the console addresses ports by their own
	// index.
	body := map[string]any{"cmd": cmdPowerCycle, fieldMAC: mac, fieldPortIndex: spec.Port}
	if _, err := client.do(ctx, http.MethodPost, w.networkPath(devMgrEndpoint), body); err != nil {
		return err
	}
	log.Info("Power-cycled a PoE port", "mac", mac, "port", spec.Port, "portName", spec.PortName)
	return nil
}

// checkPort is every reason not to cut power to this port.
//
// The three booleans are read strictly: present, and a boolean, and the value
// that permits the write. A switch that does not report one of them is refused
// rather than assumed safe — the alternative is a guard that quietly stops
// applying on some firmware, which is exactly the shape of thing nobody notices
// until it matters. If a real console turns out not to report these, that is a
// code change and the error says which field was missing so it can be reported
// rather than worked around.
func checkPort(mac string, spec reactorv1alpha1.PoEPort, port map[string]any) error {
	name, ok := port[fieldPortName].(string)
	if !ok {
		return fmt.Errorf("port %d on %s reports no name to check against %q; refusing to cycle a port "+
			"whose identity cannot be confirmed", spec.Port, mac, spec.PortName)
	}
	if name != spec.PortName {
		// The re-patched rack, caught. The index still exists; what is on it
		// has a different name, so it is probably a different thing.
		return fmt.Errorf("port %d on %s is called %q, not %q; refusing to cycle it. "+
			"If the wiring changed, change the automation to match rather than the other way round",
			spec.Port, mac, name, spec.PortName)
	}

	uplink, ok := port[fieldIsUplink].(bool)
	if !ok {
		return fmt.Errorf("port %d on %s does not report %s, so Reactor cannot tell whether it is the "+
			"switch's uplink; refusing to cycle it", spec.Port, mac, fieldIsUplink)
	}
	if uplink {
		// The floor, and it applies whatever the allowlist says — the same way
		// the outbound dialer refuses loopback whatever the destination
		// allowlist says. This port carries everything behind the switch, quite
		// possibly including Reactor's own path to the console.
		return fmt.Errorf("port %d on %s is the switch's uplink; it is never cycled, "+
			"whatever the allowlist says, because it carries everything behind the switch",
			spec.Port, mac)
	}

	capable, ok := port[fieldPortPoE].(bool)
	if !ok {
		return fmt.Errorf("port %d on %s does not report %s, so Reactor cannot tell whether it supplies "+
			"power at all; refusing to cycle it", spec.Port, mac, fieldPortPoE)
	}
	if !capable {
		return fmt.Errorf("port %d on %s does not supply PoE, so there is nothing to cycle; "+
			"check that this is the port you meant", spec.Port, mac)
	}
	// Unlike the three above, this one is only checked when the switch offers
	// it: a port that is capable but reports no current PoE state is a thing
	// Reactor can act on, while a port whose power is explicitly off is one
	// where the identity is probably wrong.
	if enabled, present := port[fieldPoEEnable].(bool); present && !enabled {
		return fmt.Errorf("PoE is switched off on port %d of %s, so there is nothing to cycle; "+
			"check that this is the port you meant", spec.Port, mac)
	}
	return nil
}

// portByIndex finds the port table entry the console numbers with want.
//
// A JSON number decodes as float64, so the comparison goes through one: the
// index is small and exact in a float, and reading it as anything else would
// mean assuming a decoder this package does not control.
func portByIndex(table any, want int32) (map[string]any, bool) {
	entries, ok := table.([]any)
	if !ok {
		return nil, false
	}
	for _, entry := range entries {
		port, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if index, ok := port[fieldPortIndex].(float64); ok && int32(index) == want {
			return port, true
		}
	}
	return nil, false
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
