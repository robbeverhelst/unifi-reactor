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
type TargetRef struct {
	// Kind of the target resource. Only "Deployment" is supported in v1alpha1.
	// +kubebuilder:validation:Enum=Deployment
	Kind string `json:"kind"`

	// Name of the target resource.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the target resource. Defaults to the Automation's own
	// namespace. Cross-namespace targets require the controller to run with
	// cluster-wide RBAC; otherwise the Automation reports Ready=False.
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
// Types divide into two kinds. A desired-state action (kubernetes.scale)
// declares a level and is arbitrated continuously across every Automation
// sharing its target. An edge action (http.request, notification.*) expresses
// an occurrence: it fires on this Automation's own transitions, owns no target
// and arbitrates with nothing.
// +kubebuilder:validation:XValidation:rule="(self.type == 'http.request') == has(self.request)",message="spec.actions: request is required by http.request and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type.startsWith('notification.') == has(self.notification)",message="spec.actions: notification is required by the notification.* types and rejected on every other type"
// +kubebuilder:validation:XValidation:rule="self.type == 'kubernetes.scale' || !has(self.target)",message="spec.actions: target belongs to kubernetes.* actions; the outbound actions have no target"
// +kubebuilder:validation:XValidation:rule="self.type == 'kubernetes.scale' || !has(self.replicas)",message="spec.actions: replicas belongs to kubernetes.scale"
type Action struct {
	// Type of the action, e.g. "kubernetes.scale".
	// +kubebuilder:validation:Enum=kubernetes.scale;http.request;notification.ntfy;notification.discord;notification.slack
	Type string `json:"type"`

	// Target of a kubernetes.* action.
	// +optional
	Target *TargetRef `json:"target,omitempty"`

	// Replicas is the desired replica count for kubernetes.scale.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Request describes the outbound call an http.request action makes.
	// +optional
	Request *HTTPRequest `json:"request,omitempty"`

	// Notification is the message a notification.* action sends.
	// +optional
	Notification *Notification `json:"notification,omitempty"`

	// TimeoutSeconds bounds a single attempt at this action, so an
	// unreachable target or endpoint cannot occupy a reconcile indefinitely.
	// Defaults to 30 for kubernetes.scale and to 10 for the outbound actions,
	// which may retry within the same reconcile. Exceeding it is recorded as a
	// failed execution, not held open.
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
	// Ref is the target this entry describes, as "Kind/namespace/name".
	Ref string `json:"ref"`

	// Desired is the value this Automation alone wants right now, whether it
	// is currently matching or reversing. Absent when it wants nothing —
	// reversal None, or a reversal value it has no way to know.
	// +optional
	Desired *int32 `json:"desired,omitempty"`

	// Effective is the value arbitration resolved to across every Automation
	// claiming this target. Absent while nothing claims it.
	// +optional
	Effective *int32 `json:"effective,omitempty"`

	// DeferredBy names the Automations whose more restrictive claim is holding
	// the target away from Desired, as "namespace/name". Empty when this
	// Automation's intent is the one in effect.
	// +optional
	DeferredBy []string `json:"deferredBy,omitempty"`
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
// Suspension and deletion are the same answer to arbitration: both mean the
// policy is not in force, so targets resolve as if the Automation were not
// there. Keeping them identical is what stops "pause this" and "remove this"
// from having different effects on the workloads being held down.
func (a *Automation) InForce() bool {
	return a.DeletionTimestamp.IsZero() && !a.Spec.Suspend
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
