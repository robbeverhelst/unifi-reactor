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

// EventTrigger fires on a genuine point-in-time event, e.g. "client.connected".
type EventTrigger struct {
	// Provider is the event provider, e.g. "unifi".
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// Event is the normalized event type, e.g. "client.connected".
	// +kubebuilder:validation:MinLength=1
	Event string `json:"event"`

	// Match optionally narrows events by exact payload field values.
	// +optional
	Match map[string]string `json:"match,omitempty"`
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

// Action is a single normalized action. Type selects the action provider;
// the provider-specific fields are flat and validated per type.
type Action struct {
	// Type of the action, e.g. "kubernetes.scale".
	// +kubebuilder:validation:Enum=kubernetes.scale
	Type string `json:"type"`

	// Target of a kubernetes.* action.
	// +optional
	Target *TargetRef `json:"target,omitempty"`

	// Replicas is the desired replica count for kubernetes.scale.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// AutomationSpec defines the desired automation: exactly one trigger kind
// (state-shaped `when` or event-shaped `trigger`) and the actions to run.
// +kubebuilder:validation:XValidation:rule="has(self.when) != has(self.trigger)",message="exactly one of spec.when or spec.trigger must be set"
// +kubebuilder:validation:XValidation:rule="has(self.when) || !has(self.onExit)",message="spec.onExit is only valid with a state trigger (spec.when)"
type AutomationSpec struct {
	// When is a state trigger: active while the provider state matches.
	// +optional
	When *StateTrigger `json:"when,omitempty"`

	// Trigger is an event trigger for genuine point-in-time events.
	// +optional
	Trigger *EventTrigger `json:"trigger,omitempty"`

	// Actions run when the trigger fires (state entered, or event matched).
	// +kubebuilder:validation:MinItems=1
	Actions []Action `json:"actions"`

	// OnExit actions run when a previously matching state condition stops
	// matching. Reversal is always explicit, never inferred.
	// +optional
	OnExit []Action `json:"onExit,omitempty"`
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
}

// AutomationStatus is the observed state of an Automation.
type AutomationStatus struct {
	// Conditions represent the current service state. "Ready" reports whether
	// the Automation is valid and being reconciled.
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

	// LastExecution is the outcome of the most recent action run, including
	// onExit runs (recorded for auditability).
	// +optional
	LastExecution *ExecutionStatus `json:"lastExecution,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.when.provider`
// +kubebuilder:printcolumn:name="Matching",type=boolean,JSONPath=`.status.matching`
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
