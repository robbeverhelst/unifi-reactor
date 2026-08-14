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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// StateTrigger fires while a provider's observed state matches. Actions run on
// entering the matching state; OnExit actions run on leaving it. Repeated
// identical observations are no-ops by design.
type StateTrigger struct {
	// Provider is the event/state provider, e.g. "unifi".
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// State is the provider-scoped key/value condition, e.g. {"wan": "backup"}.
	// All entries must match the observed state for the trigger to be active.
	// +kubebuilder:validation:MinProperties=1
	State map[string]string `json:"state"`
}

// TargetRef identifies the Kubernetes object an action operates on.
//
// The cluster-scope rule below reads __namespace__ rather than namespace, and
// has to: namespace is a CEL reserved word, so Kubernetes exposes the property
// under its escaped name. Written the obvious way the rule does not fail at
// admission — it fails to compile, which makes the whole CRD unapplyable and
// takes every other field down with it. No unit test catches that, because a
// CRD is only compiled by a real API server; the e2e suites are what found it.
//
// It lives on TargetRef rather than on Action because the constraint is a
// property of a target reference, not of any particular action that holds one.
// +kubebuilder:validation:XValidation:rule="self.kind != 'Node' || !has(self.__namespace__)",message="target.namespace: a Node is cluster-scoped and takes no namespace"
type TargetRef struct {
	// Kind of the target resource.
	//
	// kubernetes.scale works through the scale subresource, so its executor is
	// kind-agnostic: any object exposing /scale can be held at a replica count
	// without Reactor knowing where that kind keeps its replicas. This list is
	// nevertheless closed, and that is the deliberate half of the trade-off
	// #17 asks about.
	//
	// An open field would buy nothing an operator can use. A kind is only
	// reachable if the chart granted RBAC for it, and RBAC has to name
	// resources explicitly — so an open enum would accept a kind Reactor cannot
	// touch and turn a typo into a Forbidden discovered during the outage the
	// Automation was written for, instead of a rejected write at admission.
	// Adding a kind is an entry here and a rule in the chart, and no executor
	// code either way.
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;CronJob;Node
	Kind string `json:"kind"`

	// Name of the target resource.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the target resource. Defaults to the Automation's own
	// namespace. Cross-namespace targets require the controller to run with
	// cluster-wide RBAC; otherwise the Automation reports Ready=False.
	//
	// Rejected on a cluster-scoped kind. A Node addressed inside a namespace is
	// not a different Node, it is a lookup that cannot succeed.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SecretReference names a Secret in the Automation's own namespace.
//
// There is deliberately no namespace field. An Automation may only ever read
// credentials from the namespace it lives in, because anyone able to create an
// Automation can already create a Secret there — while a cross-namespace read
// would let them borrow the operator's cluster-wide reach to pull a credential
// they have no access to themselves.
type SecretReference struct {
	// Name of the Secret in this Automation's namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// HTTPHeader is one header sent with an http.request action.
//
// Values are literal and never templated, and this is not where credentials
// go: everything here is readable by anyone who can read the Automation.
// Authorization and API-key headers come from the referenced Secret.
type HTTPHeader struct {
	// Name of the header. Authorization is rejected here — it comes from the
	// Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern="^[A-Za-z0-9!#$%&'*+.^_|~-]+$"
	Name string `json:"name"`

	// Value of the header. Literal, never templated.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Value string `json:"value,omitempty"`
}

// HTTPRequest describes an outbound request for an http.request action.
//
// The destination is constrained twice over, because an operator that issues
// requests on demand is reachable by anyone who can create an Automation: the
// install-level allowlist decides which hosts Reactor may talk to at all, and
// loopback and link-local addresses are refused whatever the allowlist says.
type HTTPRequest struct {
	// Method of the request. Defaults to POST.
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH
	// +kubebuilder:default=POST
	// +optional
	Method string `json:"method,omitempty"`

	// URL to request. Must be http or https, must carry no user information,
	// and must be allowed by the install's destination allowlist. Never
	// templated: the destination is a fixed decision, not something observed
	// state gets to influence.
	//
	// Omit it to take the URL from the Secret's url key instead, which is how
	// a URL that is itself a credential stays out of the Automation. Exactly
	// one of the two must supply it.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	URL string `json:"url,omitempty"`

	// SecretRef names a Secret in this Automation's namespace holding the
	// credentials for this request. Recognised keys: url, authorization, and
	// any key prefixed header- whose remainder is the header name.
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`

	// Headers sent with the request, in addition to those from the Secret.
	// Literal values only.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Headers []HTTPHeader `json:"headers,omitempty"`

	// Body of the request, rendered as a Go text/template against the
	// transition. Available fields are Automation, Namespace, Name, Provider,
	// Matching, Key, From, To, State and Time; a json function quotes a value
	// safely for embedding in JSON. See the README for the syntax.
	//
	// The body is the only part of the request state can reach, and it only
	// ever carries values this Automation already observes, to a destination
	// the operator allowed.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Body string `json:"body,omitempty"`

	// Idempotent declares that repeating this request is harmless, which is
	// what lets Reactor retry it after a timeout or a 5xx. GET and PUT are
	// treated as idempotent without saying so; POST and PATCH are not, and are
	// attempted exactly once so a transient failure cannot turn into a second
	// order, message or payment.
	// +optional
	Idempotent *bool `json:"idempotent,omitempty"`
}

// HomeAssistantService is one Home Assistant service call.
//
// It is a shape over the same outbound transport http.request uses — the same
// install-level destination allowlist, the same address floor in the dialer,
// the same rule that credentials come only from a Secret in this Automation's
// own namespace. What it adds is that the request path is built from a domain
// and a service rather than written out, so the action states what it is and
// cannot be turned into an arbitrary request to an allowed host.
//
// The direction matters and is the reason this exists. Home Assistant can
// already see UniFi; what it cannot see is the cluster. This is the seam
// Reactor reaches it through, and it is also why Reactor does not observe
// presence itself.
type HomeAssistantService struct {
	// URL is the base address of the Home Assistant instance, e.g.
	// https://home-assistant.example.com. It may carry a path, for an instance
	// behind a reverse proxy, and takes no query or fragment. The service path
	// is appended by Reactor and is not expressible here.
	//
	// Omit it to take the base address from the Secret's url key instead.
	// Exactly one of the two must supply it.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	URL string `json:"url,omitempty"`

	// SecretRef names a Secret in this Automation's namespace holding the
	// long-lived access token, under the authorization key and in the form
	// "Bearer <token>". It may also hold the base address under url.
	//
	// A token is required: Home Assistant authenticates every API call, and
	// there is no unauthenticated shape of this action to fall back to.
	SecretRef SecretReference `json:"secretRef"`

	// Domain of the service being called, e.g. "light", "script", "notify".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern="^[a-z0-9_]+$"
	Domain string `json:"domain"`

	// Service to call within the domain, e.g. "turn_on".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern="^[a-z0-9_]+$"
	Service string `json:"service"`

	// Data is the service data, rendered as a Go text/template against the
	// transition and sent as the JSON request body. It must render to a JSON
	// object; omitting it sends an empty one. Available fields are Automation,
	// Namespace, Name, Provider, Matching, Key, From, To, State and Time, and a
	// json function quotes a value safely for embedding. See the README.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Data string `json:"data,omitempty"`

	// Idempotent declares that calling this service twice is the same as
	// calling it once, which is what lets Reactor retry it after a timeout or a
	// 5xx. It defaults to false, and has to: light.turn_on is idempotent and
	// script.turn_on, notify.mobile_app and button.press are not, and Reactor
	// cannot tell which one it was handed. A duplicate announcement or a second
	// press is worse than a missed one when nobody knows what was pressed.
	// +optional
	Idempotent *bool `json:"idempotent,omitempty"`
}

// QBittorrent is the instance a qbittorrent.pause or qbittorrent.resume acts on.
//
// Pausing is a level in the world — paused or running — and an edge action
// here, which is the one thing about this type worth understanding before
// using it.
//
// A desired-state action is arbitrated across every Automation claiming its
// target, and what makes that possible is not the fold: it is that the target
// is a Kubernetes object, so the value it held before Reactor first touched it
// can be recorded as an annotation ON that object, where it outlives both the
// Automation and Reactor itself. A qBittorrent instance reached over HTTP has
// no such place. It has no Kubernetes identity to arbitrate over, no annotation
// to hold a baseline, and no way for the pre-delete sweep — which reads those
// annotations with no credentials and no allowlist — to hand it back.
//
// So the honest shape is an edge action, and two limitations follow from that
// rather than being oversights:
//
//   - It is not arbitrated. Two Automations pausing the same instance for
//     unrelated reasons do not resolve to one claim; each fires on its own
//     transition, and whichever resumes first resumes everything.
//   - It has no baseline. A resume resumes every torrent, including ones
//     paused by hand before Reactor ever ran. Nothing here can tell those
//     apart, because nothing recorded which they were.
//
// A design for non-Kubernetes desired-state targets — somewhere legitimate to
// keep a baseline and a claim for a thing with no object to hang them on — does
// not exist yet, and inventing one inside this type would be the worst place to
// try. See the README for the alternatives that were considered.
type QBittorrent struct {
	// URL is the base address of the qBittorrent WebUI, e.g.
	// http://qbittorrent.media.svc.cluster.local:8080. The API paths are
	// appended by Reactor and are not expressible here.
	//
	// Omit it to take the base address from the Secret's url key instead.
	// Exactly one of the two must supply it.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	URL string `json:"url,omitempty"`

	// SecretRef names a Secret in this Automation's namespace holding the WebUI
	// username and password, under the username and password keys. It may also
	// hold the base address under url.
	//
	// Both are required. qBittorrent issues a session cookie rather than
	// accepting a static token, and that login is the entire reason this action
	// exists rather than being an http.request — an instance configured to
	// bypass authentication for its subnet is expressible as one POST with
	// http.request, and that is the honest thing to write for it.
	SecretRef SecretReference `json:"secretRef"`
}

// WLAN is the wireless network a unifi.wlan.enable or unifi.wlan.disable acts
// on.
//
// This is the first action that writes to the UniFi console, and the first that
// can take something away from people who are not running the cluster. Read the
// two paragraphs below before using it.
//
// It is an edge action, and the reason is the rule the QBittorrent type states:
// a desired-state action is arbitrated, and what makes that possible is not the
// fold but that the target is a Kubernetes object, so the value it held before
// Reactor claimed it can be recorded as an annotation ON that object. A UniFi
// WLAN has no such place. Writing Reactor's bookkeeping into the WLAN's own
// configuration is the same mistake a torrent tag would have been — it is the
// user's config, they can edit it, and the write that carries it is a
// read-modify-write with no concurrency control. And a baseline nobody can read
// is not a baseline: releasing a WLAN means a credentialed write to the
// console, which the pre-delete sweep during an uninstall is designed to be
// incapable of.
//
// So two limitations follow, and they are louder here than for a torrent
// client:
//
//   - It is not arbitrated. Two Automations disabling the same WLAN do not
//     resolve to one claim; whichever enables it first enables it.
//   - Nothing hands it back. If the exit transition never arrives — the
//     Automation is deleted, Reactor is uninstalled, the state key stops being
//     observable — the WLAN stays as Reactor last left it. A guest network that
//     was turned off stays off until a human turns it back on.
//
// The HorizontalPodAutoscaler decline path is the clearest illustration of what
// is missing here. A scalable target an HPA already drives can be refused and,
// if Reactor was already holding it, put back where it was — and it can be put
// back precisely because the baseline is an annotation on the object. A WLAN has
// no equivalent, so there is no state it could be declined back to.
//
// Which SSIDs may be touched at all is the operator's decision at install time,
// not the Automation's: unifi.actions.allowedWlans is empty by default and
// empty refuses everything.
type WLAN struct {
	// Name is the SSID exactly as the console spells it, matched
	// case-sensitively against the WLAN configuration on the site Reactor
	// polls. It must also appear in the install's allowed WLAN list.
	//
	// A name that matches nothing is refused rather than guessed at, and the
	// refusal does not list the WLANs that do exist: an Automation is readable
	// by anyone in its namespace, and the network's SSIDs are not theirs.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
}

// PoEPort is the switch port a unifi.poe.cycle power-cycles.
//
// This is the most dangerous action Reactor has, and the danger is not the
// write — it is the identity. Cutting power to the wrong port drops an access
// point, a camera, or the uplink carrying the cluster, and it does so silently
// from Reactor's point of view: the console accepts the command either way.
//
// So a port is identified by three things that must all agree, checked against
// the switch's own port table immediately before the command is sent:
//
//   - device, the switch's MAC. Not its name, which is a label somebody can
//     change without changing which hardware it is.
//   - port, the index on that switch.
//   - portName, the name that port carries in the switch's configuration.
//
// The third is the one doing the real work, and it is required rather than
// optional for that reason. A port index alone means something different after
// somebody re-patches a rack: slot 7 is still slot 7, and the thing plugged
// into it is not. Naming what is supposed to be there turns a re-patch from a
// silent mis-cycle into a refused action with a sentence saying the port is
// called something else now.
//
// Three refusals apply whatever the install's allowlist says, in the same way
// the outbound dialer refuses loopback whatever the destination allowlist says.
// A port the switch reports as its uplink is never cycled — that is the port
// carrying everything behind the switch, including, possibly, Reactor's own
// path to the console. A port the switch does not report as PoE-capable is
// never cycled, because there is nothing there to cycle and the identity is
// probably wrong. And a switch that does not report those fields at all is
// refused rather than assumed safe: a guard that silently does not apply is
// worse than one that declines.
//
// Which ports may be cycled at all is the operator's decision at install time —
// unifi.actions.allowedPoePorts, empty by default and refusing everything.
type PoEPort struct {
	// Device is the MAC address of the switch, lowercase and colon-separated,
	// e.g. "aa:bb:cc:00:11:22". A MAC rather than a device name because a name
	// is a label: renaming a switch would silently repoint this action, and a
	// MAC identifies the hardware.
	// +kubebuilder:validation:Pattern="^[0-9a-f]{2}(:[0-9a-f]{2}){5}$"
	Device string `json:"device"`

	// Port is the port index on that switch, as the console numbers it — the
	// number on the front panel, starting at 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Port int32 `json:"port"`

	// PortName is the name that port carries in the switch's configuration, and
	// it is checked before anything is sent. It is required, and it is the whole
	// defence against a re-patched rack: an index means "whatever is in slot 7
	// now", and this means "the thing I meant".
	//
	// If it stops matching, the action is refused and says so. That is the
	// intended outcome — name your ports, and a change to the wiring becomes a
	// visible refusal instead of a power cut to something else.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PortName string `json:"portName"`
}

// Notification is the message a notification.* action sends.
//
// The destination is not expressible here at all: it comes from the referenced
// Secret, because for every transport shipped the URL is itself the credential.
type Notification struct {
	// SecretRef names a Secret in this Automation's namespace holding the
	// destination. Required keys: url. Optional: authorization, sent as the
	// Authorization header.
	SecretRef SecretReference `json:"secretRef"`

	// Title of the notification, rendered as a Go text/template against the
	// transition. Transports without a title concept prepend it to the message.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Title string `json:"title,omitempty"`

	// Message body, rendered as a Go text/template against the transition.
	// Available fields are Automation, Namespace, Name, Provider, Matching,
	// Key, From, To, State and Time. See the README for the syntax.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Message string `json:"message"`
}

// Action is a single normalized action. Type selects the action provider;
// the provider-specific fields are flat and validated per type.
//
// Types divide into two kinds. A desired-state action (kubernetes.scale,
// kubernetes.cronjob.suspend) declares a level and is arbitrated continuously
// across every Automation sharing its target. An edge action (kubernetes.restart,
// http.request, notification.*, homeassistant.service, qbittorrent.*,
// unifi.wlan.*, unifi.poe.cycle) expresses an occurrence: it fires on this Automation's own
// transitions, owns no target and arbitrates with nothing.
//
// The dividing line is not "does this express a level" — pausing a torrent
// client plainly does. It is whether there is somewhere to record the value the
// target held before Reactor claimed it, so that release can put it back. For a
// Kubernetes object that is an annotation on the object; for anything else
// there is no answer yet, which is why qbittorrent.* is an edge action and is
// named as a verb. See the QBittorrent type.
//
// A desired-state action's level is an integer the arbiter orders and nothing
// more, so a boolean level is carried as its own field — replicas for a count,
// suspended and cordoned for a switch — rather than by overloading one of them.
// The units differ; the ordering does not.
// +kubebuilder:validation:XValidation:rule="(self.type == 'http.request') == has(self.request)",message="spec.actions: request is required by http.request and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="(self.type == 'homeassistant.service') == has(self.homeAssistant)",message="spec.actions: homeAssistant is required by homeassistant.service and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type.startsWith('qbittorrent.') == has(self.qbittorrent)",message="spec.actions: qbittorrent is required by the qbittorrent.* types and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type.startsWith('unifi.wlan.') == has(self.wlan)",message="spec.actions: wlan is required by the unifi.wlan.* types and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="(self.type == 'unifi.poe.cycle') == has(self.poe)",message="spec.actions: poe is required by unifi.poe.cycle and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type.startsWith('notification.') == has(self.notification)",message="spec.actions: notification is required by the notification.* types and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type.startsWith('kubernetes.') == has(self.target)",message="spec.actions: target is required by the kubernetes.* actions and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type == 'kubernetes.scale' || !has(self.replicas)",message="spec.actions: replicas belongs to kubernetes.scale"
// +kubebuilder:validation:XValidation:rule="self.type == 'kubernetes.cronjob.suspend' || !has(self.suspended)",message="spec.actions: suspended belongs to kubernetes.cronjob.suspend"
// +kubebuilder:validation:XValidation:rule="self.type == 'kubernetes.cordon' || !has(self.cordoned)",message="spec.actions: cordoned belongs to kubernetes.cordon"
// +kubebuilder:validation:XValidation:rule="!has(self.target) || self.type != 'kubernetes.cordon' || self.target.kind == 'Node'",message="spec.actions: kubernetes.cordon targets a Node"
// +kubebuilder:validation:XValidation:rule="!has(self.target) || self.type != 'kubernetes.scale' || self.target.kind in ['Deployment', 'StatefulSet']",message="spec.actions: kubernetes.scale targets a kind with a scale subresource: Deployment or StatefulSet"
// +kubebuilder:validation:XValidation:rule="!has(self.target) || self.type != 'kubernetes.cronjob.suspend' || self.target.kind == 'CronJob'",message="spec.actions: kubernetes.cronjob.suspend targets a CronJob"
// +kubebuilder:validation:XValidation:rule="!has(self.target) || self.type != 'kubernetes.restart' || self.target.kind in ['Deployment', 'StatefulSet']",message="spec.actions: kubernetes.restart targets a kind with a pod template: Deployment or StatefulSet"
type Action struct {
	// Type of the action, e.g. "kubernetes.scale".
	// +kubebuilder:validation:Enum=kubernetes.scale;kubernetes.cronjob.suspend;kubernetes.cordon;kubernetes.restart;http.request;notification.ntfy;notification.discord;notification.slack;homeassistant.service;qbittorrent.pause;qbittorrent.resume;unifi.wlan.enable;unifi.wlan.disable;unifi.poe.cycle
	Type string `json:"type"`

	// Target of a kubernetes.* action.
	// +optional
	Target *TargetRef `json:"target,omitempty"`

	// Replicas is the desired replica count for kubernetes.scale.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Suspended is whether kubernetes.cronjob.suspend wants the target CronJob
	// suspended. Omitting it means true, which is what the action is named
	// after; write suspended: false in spec.onExit to ask for it back.
	//
	// Suspending stops new Jobs being created. It deliberately does nothing to
	// a Job already running: killing work in flight is a different and much
	// more dangerous action than declining to start more of it, and mid-flight
	// deletion is not something an outage should decide on your behalf.
	// +optional
	Suspended *bool `json:"suspended,omitempty"`

	// Cordoned is whether kubernetes.cordon wants the target Node closed to new
	// scheduling. Omitting it means true, which is what the action is named
	// after; write cordoned: false in spec.onExit to reopen it explicitly.
	//
	// Cordoning stops new Pods being scheduled onto the Node and moves nothing
	// that is already running. Evicting those — draining — is not offered, and
	// not because it was not built: an eviction cannot be reversed, so it has no
	// level to arbitrate and no reversal to declare, which is the one property
	// every other action here has. See docs/spec.md.
	//
	// Node actions need cluster-scoped RBAC that the chart grants only when
	// rbac.allowNodeActions is set. Without it the Automation reports the target
	// as unreachable and names the value to set.
	// +optional
	Cordoned *bool `json:"cordoned,omitempty"`

	// Request describes the outbound call an http.request action makes.
	// +optional
	Request *HTTPRequest `json:"request,omitempty"`

	// Notification is the message a notification.* action sends.
	// +optional
	Notification *Notification `json:"notification,omitempty"`

	// HomeAssistant is the service call a homeassistant.service action makes.
	// +optional
	HomeAssistant *HomeAssistantService `json:"homeAssistant,omitempty"`

	// QBittorrent is the instance a qbittorrent.* action acts on.
	// +optional
	QBittorrent *QBittorrent `json:"qbittorrent,omitempty"`

	// WLAN is the wireless network a unifi.wlan.* action acts on.
	// +optional
	WLAN *WLAN `json:"wlan,omitempty"`

	// PoE is the switch port a unifi.poe.cycle power-cycles.
	// +optional
	PoE *PoEPort `json:"poe,omitempty"`

	// TimeoutSeconds bounds a single attempt at this action, so an
	// unreachable target or endpoint cannot occupy a reconcile indefinitely.
	// Defaults to 30 for the kubernetes.* actions and for the unifi.* console
	// ones — which are a login, a check and a write rather than a single request
	// — and to 10 for the outbound ones, which may retry within the same
	// reconcile. Exceeding it is recorded as a failed execution, not held open.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=600
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
}

// ReversalPolicy selects what an Automation wants for its targets while its
// condition does not hold.
//
// A target's value is arbitrated across every Automation that references it,
// so an Automation is never simply "done" — it always either claims the target
// or declares what it wants once nothing claims it any more.
// +kubebuilder:validation:Enum=Declared;Baseline;None
type ReversalPolicy string

const (
	// ReversalDeclared restores the values in spec.onExit. The default when
	// spec.onExit is set.
	ReversalDeclared ReversalPolicy = "Declared"
	// ReversalBaseline restores what the target was set to before Reactor
	// first claimed it. The default when spec.onExit is omitted.
	ReversalBaseline ReversalPolicy = "Baseline"
	// ReversalNone leaves the target wherever it was left. Reactor stops
	// asserting a value for it entirely.
	ReversalNone ReversalPolicy = "None"
)

// AutomationSpec defines the desired automation: the state condition to watch
// and the actions to run while it holds.
//
// v1alpha1 has one trigger kind. The event-shaped `spec.trigger` this schema
// used to accept was removed because nothing implemented it: no captured
// delivery payload exists to match against, and every action type is a
// desired-state action that is arbitrated continuously rather than fired on an
// occurrence. It returns in a later API version once both exist. Nothing about
// `when` changes when it does.
// +kubebuilder:validation:XValidation:rule="!has(self.reversal) || self.reversal != 'Declared' || has(self.onExit)",message="spec.reversal: Declared requires spec.onExit"
type AutomationSpec struct {
	// When is a state trigger: active while the provider state matches.
	// +required
	When *StateTrigger `json:"when"`

	// Actions run while the state condition holds.
	// +kubebuilder:validation:MinItems=1
	Actions []Action `json:"actions"`

	// OnExit declares what this Automation wants for its targets once its
	// condition stops holding. For desired-state actions this is a level that
	// is arbitrated against every other Automation sharing the target, not a
	// list executed on the exit edge: leaving the matching state does not
	// raise a target another Automation still holds down.
	// +optional
	OnExit []Action `json:"onExit,omitempty"`

	// Reversal selects what this Automation wants while its condition does not
	// hold. Defaults to Declared when spec.onExit is set, and to Baseline —
	// restore what the target was before Reactor first touched it — when it is
	// not. Set None to leave targets wherever they were left.
	// +optional
	Reversal ReversalPolicy `json:"reversal,omitempty"`

	// DryRun evaluates this Automation fully and reports what it would do,
	// without touching anything.
	//
	// It is out of force exactly as Suspend is: it claims no target, writes
	// nothing, and cannot change what any other Automation's targets resolve
	// to — which is what makes it safe to apply one next to policies that are
	// live. What it adds is status.targets[].preview: the arbitration
	// recomputed as if this Automation's condition held and it were in force,
	// naming what each target would be held at, who would outvote it, who it
	// would outvote, and what it would hand back afterwards.
	//
	// Turning it on for an Automation that is currently holding a target is a
	// release, exactly as suspending it is. It stops being in force, so the
	// target goes back to whatever the remaining claims want.
	//
	// A preview is a fact about the moment it was computed and not a promise
	// about the next one: the peers, the observed state and the target can all
	// change before the condition it describes actually holds. See the README.
	// +optional
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`

	// Suspend takes this Automation out of force without deleting it: it goes
	// on observing state and reporting it, and stops claiming its targets
	// entirely.
	//
	// Suspending is a reversible delete, not a freeze. Targets are arbitrated
	// as if this Automation did not exist, so whatever it was holding down is
	// handed back to the other Automations claiming it — or, if none do, to
	// this Automation's own spec.reversal, exactly as deleting it would. Which
	// also means a suspended Automation writes nothing and can hold nothing
	// down: scale its targets by hand while you work.
	//
	// Resuming re-evaluates against current state rather than replaying
	// anything, so an Automation whose condition still holds re-claims its
	// targets on the next reconcile.
	// +optional
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// EffectiveReversal resolves the reversal policy actually in force, applying
// the defaults documented on the field. Callers must use this rather than
// reading spec.reversal, which is empty on every Automation written before the
// field existed.
func (s *AutomationSpec) EffectiveReversal() ReversalPolicy {
	if s.Reversal != "" {
		return s.Reversal
	}
	if len(s.OnExit) > 0 {
		return ReversalDeclared
	}
	return ReversalBaseline
}

// StateTransition records the last observed provider state transition that
// changed this Automation's matching.
type StateTransition struct {
	// Key is the state key that transitioned, e.g. "wan".
	Key string `json:"key,omitempty"`
	// From is the previous value.
	From string `json:"from,omitempty"`
	// To is the new value.
	To string `json:"to,omitempty"`
	// Time is when the transition was observed.
	Time metav1.Time `json:"time,omitempty"`
}

// ExecutionStatus records the outcome of the most recent action execution.
type ExecutionStatus struct {
	// Status is "Success" or "Failed".
	Status string `json:"status,omitempty"`
	// Reason holds a human-readable error when Status is "Failed".
	Reason string `json:"reason,omitempty"`
	// Time is when execution finished.
	Time metav1.Time `json:"time,omitempty"`
	// OnExit is true when this execution ran the onExit actions.
	OnExit bool `json:"onExit,omitempty"`
	// Attempts counts consecutive failures. Retries stop once the budget is
	// exhausted, leaving the Automation to recover on the next state change
	// rather than retrying a hopeless action forever.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
}

// EdgeExecutionStatus records what one edge action did on this Automation's
// last transition.
//
// Edge actions are reported separately from LastExecution because they fail
// differently: a desired-state action that fails is corrected by the next
// reconcile, so its failure is the Automation's problem. An edge action fires
// on an occurrence that has already passed, so a failure is a thing that did
// not happen — worth reporting, but not a reason to call an Automation whose
// workload was scaled correctly unhealthy.
type EdgeExecutionStatus struct {
	// Type is the action type this entry describes, e.g. "notification.ntfy".
	Type string `json:"type"`

	// Status is "Success", "Failed", or "Skipped".
	Status string `json:"status"`

	// Reason explains a failure or a skip. It never contains a credential: a
	// destination is reported as scheme, host and port only, with the path and
	// query — the part of a webhook URL that is the secret — left out, and a
	// response body is never included.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Destination is the scheme, host and port the request went to, for the
	// same reason and with the same omissions.
	//
	// An action that writes to a provider's own console reports the object it
	// acted on instead — "unifi/wlan/Guest" — because the console's address is
	// install configuration that is the same for every Automation, while which
	// object was touched is the part worth reading.
	// +optional
	Destination string `json:"destination,omitempty"`

	// Attempts counts how many times this action was tried. More than one only
	// happens for actions declared safe to repeat.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// OnExit is true when this action ran from spec.onExit.
	// +optional
	OnExit bool `json:"onExit,omitempty"`

	// Time is when the attempt finished.
	// +optional
	Time metav1.Time `json:"time,omitempty"`
}

// TargetStatus reports what this Automation wants for one target and what the
// arbitration across every Automation sharing that target actually resolved
// to. When the two differ, DeferredBy names who is holding it there.
type TargetStatus struct {
	// Ref is the target this entry describes, as "Kind/namespace/name", or
	// "Kind/name" for a cluster-scoped target.
	Ref string `json:"ref"`

	// Desired is the level this Automation alone wants right now, whether it
	// is currently matching or reversing. Absent when it wants nothing —
	// reversal None, or a reversal value it has no way to know.
	//
	// A level is ordered and nothing more: lower is more restrictive, and the
	// arbiter resolves a shared target by taking the lowest. What the number
	// counts depends on the action that produced it — replicas for
	// kubernetes.scale, and 0 suspended / 1 running for
	// kubernetes.cronjob.suspend. Level spells the same value out in words.
	// +optional
	Desired *int32 `json:"desired,omitempty"`

	// Effective is the level arbitration resolved to across every Automation
	// claiming this target. Absent while nothing claims it.
	// +optional
	Effective *int32 `json:"effective,omitempty"`

	// Level renders Effective in the units of the action that set it — "3
	// replicas", "suspended" — because a bare number stops explaining itself
	// once a target's level is a switch rather than a count.
	// +optional
	Level string `json:"level,omitempty"`

	// DeferredBy names the Automations whose more restrictive claim is holding
	// the target away from Desired, as "namespace/name". Empty when this
	// Automation's intent is the one in effect.
	// +optional
	DeferredBy []string `json:"deferredBy,omitempty"`

	// Preview is what would happen here if this Automation were in force,
	// reported while it deliberately is not.
	// +optional
	Preview *TargetPreview `json:"preview,omitempty"`

	// ManagedBy names a controller other than Reactor that already drives this
	// target's level, as "Kind/namespace/name".
	//
	// Arbitration reaches only the Automations, so a claimant that is not one
	// cannot be folded in and cannot be resolved against — it can only be
	// fought, which neither side wins. Reactor therefore declines to claim a
	// target named here and writes nothing to it, and Applied is False with
	// reason TargetManagedByHPA. The Automation stays Ready: it is correctly
	// configured, it simply cannot act on this target.
	//
	// Currently only a HorizontalPodAutoscaler, and only on an install that
	// turned detection on. Nothing here promises the field is uncontested when
	// it is empty: KEDA, a GitOps controller correcting drift and a cron job
	// running kubectl own spec.replicas just as hard, and none of them is
	// discoverable.
	// +optional
	ManagedBy string `json:"managedBy,omitempty"`
}

// TargetPreview is what would happen to one target if this Automation's
// condition held and it were in force, arbitrated against the claims that exist
// right now.
//
// It is answerable without writing anything because arbitration is a pure
// function of the claims that hold: the same fold that decides a target's value
// answers the counterfactual with one more claim in it. It is reported while an
// Automation is deliberately out of force — spec.dryRun, or spec.suspend, where
// it answers "what would resuming this do" — and is absent otherwise, because
// an Automation that is in force is already described by the fields above.
//
// It is also absent on a target ManagedBy names, and that is an answer rather
// than a gap: such a target would be declined rather than claimed, so there is
// no level to preview.
//
// Three things it cannot promise, all the same thing said three ways: it is
// computed from the peers, the observed state and the target as they are at
// this moment, and any of them can differ by the time the condition actually
// holds. It also says nothing about whether the write would succeed — RBAC, an
// admission webhook and a target that has since been deleted are all outside
// what a fold can know.
type TargetPreview struct {
	// Desired is the level this Automation would ask for. Absent when it would
	// ask for nothing on this target.
	// +optional
	Desired *int32 `json:"desired,omitempty"`

	// Effective is what arbitration would resolve to across every claim,
	// including this one.
	// +optional
	Effective *int32 `json:"effective,omitempty"`

	// Level renders Effective in the units of the action that would set it, for
	// the same reason TargetStatus.Level does.
	// +optional
	Level string `json:"level,omitempty"`

	// DeferredBy names the Automations whose more restrictive claim would
	// outvote this one, leaving the target exactly where it already is.
	// +optional
	DeferredBy []string `json:"deferredBy,omitempty"`

	// WouldDefer names the Automations this claim would outvote: the ones
	// getting what they want now that would stop getting it. A peer already
	// outvoted by a third automation is not listed, because it is deferred
	// either way and this claim is not what did it.
	// +optional
	WouldDefer []string `json:"wouldDefer,omitempty"`

	// OnExit says what this Automation would want for the target once its
	// condition stopped holding, in words — "3 replicas", "running", or "left
	// as found" under reversal None.
	//
	// Rendered rather than numeric because it is read rather than computed
	// against, and because under reversal Baseline it may describe a value
	// Reactor has not recorded yet: on a target nothing has ever claimed, the
	// baseline a claim would capture is simply what the target is at now.
	// +optional
	OnExit string `json:"onExit,omitempty"`
}

// AutomationStatus is the observed state of an Automation.
type AutomationStatus struct {
	// Conditions represent the current service state. "Ready" reports whether
	// the Automation is valid and being reconciled; "Applied" reports whether
	// what it wants is what its targets actually have.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Matching is true while a state trigger's condition currently matches.
	// +optional
	Matching bool `json:"matching,omitempty"`

	// ObservedState is the provider state relevant to this Automation at the
	// last reconcile, e.g. {"wan": "backup"}.
	// +optional
	ObservedState map[string]string `json:"observedState,omitempty"`

	// ObservedAt is when the provider state above was read from the provider,
	// which is not the same as when this Automation last reconciled.
	//
	// It is the qualifier on every other field here. A decision is only as
	// current as the observation it was taken against, and the two windows that
	// separate them are very different: a value that CHANGED reaches this
	// object within one poll interval times the samples its key must hold for,
	// while a provider that has stopped answering leaves this timestamp
	// standing still and every decision below being re-taken against it. Past
	// the age the install allows, Ready goes False with reason
	// ObservationStale — and Reactor still acts, because withdrawing state it
	// cannot confirm would release claims mid-incident.
	//
	// Absent until the provider has reported anything at all.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// LastTransition is the state change that last flipped Matching.
	// +optional
	LastTransition *StateTransition `json:"lastTransition,omitempty"`

	// LastExecution is the outcome of the most recent desired-state action run,
	// including onExit runs (recorded for auditability).
	// +optional
	LastExecution *ExecutionStatus `json:"lastExecution,omitempty"`

	// EdgeActions is what the edge actions did on the last transition, one
	// entry per action that fired, in spec order. It is replaced wholesale on
	// each transition rather than appended to: it answers "what happened when
	// this last changed", not "what has ever happened".
	// +optional
	EdgeActions []EdgeExecutionStatus `json:"edgeActions,omitempty"`

	// Targets reports the arbitrated outcome per target, explaining why an
	// Automation that wants something is not getting it.
	// +optional
	Targets []TargetStatus `json:"targets,omitempty"`

	// ReleaseAttempts counts how many times handing this Automation's targets
	// back has failed during deletion. Deletion gives up once it is exhausted
	// rather than leaving the resource stuck terminating.
	// +optional
	ReleaseAttempts int32 `json:"releaseAttempts,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.when.provider`
// +kubebuilder:printcolumn:name="Matching",type=boolean,JSONPath=`.status.matching`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Automation reacts to provider state or events with declarative actions.
type Automation struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec AutomationSpec `json:"spec"`

	// +optional
	Status AutomationStatus `json:"status,omitempty"`
}

// InForce reports whether this Automation currently claims its targets.
//
// Suspension, a dry run and deletion are the same answer to arbitration: all
// three mean the policy is not in force, so targets resolve as if the
// Automation were not there. Keeping them identical is what stops "pause this",
// "show me what this would do" and "remove this" from having different effects
// on the workloads being held down — and it is what makes a dry run safe to
// apply beside automations that are live.
func (a *Automation) InForce() bool {
	return a.DeletionTimestamp.IsZero() && !a.Spec.Suspend && !a.Spec.DryRun
}

// +kubebuilder:object:root=true

// AutomationList contains a list of Automation.
type AutomationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Automation `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Automation{}, &AutomationList{})
		return nil
	})
}
