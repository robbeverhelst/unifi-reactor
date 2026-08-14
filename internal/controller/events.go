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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
)

// Event reasons. Together these are meant to make `kubectl describe automation`
// tell the whole story of a failover without anyone needing log access — which
// is cluster-admin, and which nobody has at 3am on a phone.
//
// Normal and Warning are used deliberately, not as a severity dial. A held
// state, a deferred claim and a reversal are all Warning-free: they are the
// design working. Warning is reserved for something an operator has to act on.
const (
	// reasonStateEntered and reasonStateExited bracket the interesting part of
	// every incident: the moment this Automation's condition started holding,
	// and the moment it stopped.
	reasonStateEntered = "StateEntered"
	reasonStateExited  = "StateExited"
	// reasonTargetHeld and reasonTargetReleased record a write that actually
	// happened. Reconciling a target that is already where it should be is the
	// common case and produces nothing.
	//
	// One reason covers every desired-state action rather than one per kind:
	// what happened is that arbitration moved a target to its resolved level,
	// and the note says in words which level that was. A per-kind reason would
	// make `kubectl get events --field-selector reason=...` an incomplete
	// question that silently stops matching each time a kind is added.
	reasonTargetHeld     = "TargetHeld"
	reasonTargetReleased = "TargetReleased"
	// reasonDeferred is Normal on purpose. Being outvoted by a more restrictive
	// claim is how two Automations sharing a workload are meant to behave, and
	// reporting it as a fault would train people to ignore it.
	reasonDeferred = "DeferredToOtherAutomation"
	// reasonStateKeyUnavailable is a Warning because holding state indefinitely
	// is not a resting place: the hardware publishing a key has gone, and
	// somebody has to decide whether it is coming back.
	reasonStateKeyUnavailable = "StateKeyUnavailable"
	// reasonObservationStale is the adjacent case, and a Warning for the same
	// reason. There the console answered and left a key out; here it has
	// stopped answering, so every key is as old as the last reply. Both hold
	// the last known state and go on acting on it, because losing sight of the
	// world is not evidence about the world — and both have to say so, or the
	// only difference between Reactor working and Reactor blind is a graph.
	//
	// It is raised only on an install that set unifi.maxObservationAge. Without
	// a bound there is no such thing as too old, and inventing one would make
	// an upgrade start reporting a fault that was always there.
	reasonObservationStale = "ObservationStale"
	// reasonRetryBudgetExhausted is raised once, when Reactor stops retrying a
	// target and starts waiting for the next state change instead.
	reasonRetryBudgetExhausted = "RetryBudgetExhausted"
	// reasonReleaseFailed is raised when deletion gives up handing targets back.
	reasonReleaseFailed = "ReleaseFailed"
	// reasonDryRun is Normal, and is the whole output of a mode whose output is
	// the point. It is raised on the transition a dry run would have acted on,
	// because that is the moment worth telling somebody about and the one thing
	// status cannot tell them: status is a poll, and nobody polls a resource at
	// the second it becomes interesting.
	reasonDryRun = "DryRun"
	// reasonTargetManagedByHPA is a Warning, and the one place that distinction
	// carries real weight. Being outvoted by a peer is the arbitration working
	// and is reported Normal; being unable to arbitrate at all is an automation
	// that cannot do its job, and no amount of waiting fixes it. Somebody has to
	// decide which controller owns the workload.
	reasonTargetManagedByHPA = "TargetManagedByHPA"
)

// The events.k8s.io "action" field: what the controller was doing, as a coarse
// verb. It is a separate axis from reason, and it is what makes an Event stream
// filterable by phase rather than only by outcome.
const (
	actionEvaluate  = "Evaluate"
	actionExecute   = "Execute"
	actionRelease   = "Release"
	actionEdge      = "EdgeAction"
	actionReconcile = "Reconcile"
)

// event raises one Event against an Automation, tolerating an unset recorder so
// that tests and any caller running without one behave identically to one that
// simply has nothing to say.
func (r *AutomationReconciler) event(
	automation *reactorv1alpha1.Automation,
	eventType, reason, action, note string,
	args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(automation, nil, eventType, reason, action, note, args...)
}

// eventOnNewReason raises an Event only when the named condition is not already
// reporting this reason.
//
// Reconcile runs at least every reevaluateInterval, so anything raised
// unconditionally from a steady state — a key that has been missing for an hour,
// a claim that has been deferred all week — would be one API write every fifteen
// seconds, per Automation, forever. The condition already persists exactly the
// edge that matters, so it is used as the memory rather than inventing a second
// one that a restart would lose.
//
// It must be called BEFORE the matching setCondition, while the condition still
// holds the previous reason. Calling it after is not a compile error and would
// silently emit nothing, which is why the two are never more than a line apart.
func (r *AutomationReconciler) eventOnNewReason(
	automation *reactorv1alpha1.Automation,
	conditionType, eventType, reason, action, note string,
	args ...any,
) {
	if existing := meta.FindStatusCondition(automation.Status.Conditions, conditionType); existing != nil &&
		existing.Reason == reason {
		return
	}
	r.event(automation, eventType, reason, action, note, args...)
}

// reportTransition announces this Automation's condition starting or stopping
// holding, naming the state change that did it.
//
// This is the pair of Events an incident is read through, so the transition is
// spelled out in the note rather than left to be looked up: a key with an open
// value set — isp — is deliberately not a metric label, and this is where its
// values live instead.
//
// The transitioned guard lives here rather than at the call site so Reconcile
// gains no branch for reporting: it is already at the edge of what gocyclo
// allows, and observability has no business being what pushes it over.
func (r *AutomationReconciler) reportTransition(
	automation *reactorv1alpha1.Automation,
	transitioned, matching bool,
) {
	if !transitioned {
		return
	}

	reason, verb := reasonStateExited, "stopped holding"
	if matching {
		reason, verb = reasonStateEntered, "started holding"
	}

	transition := automation.Status.LastTransition
	if transition == nil || transition.Key == "" {
		r.event(automation, corev1.EventTypeNormal, reason, actionEvaluate,
			"the condition %s against observed %s state", verb, automation.Spec.When.Provider)
		return
	}
	r.event(automation, corev1.EventTypeNormal, reason, actionEvaluate,
		"%s moved from %q to %q, so the condition %s",
		transition.Key, transition.From, transition.To, verb)
}

// reportPreview announces what a dry run would have done, at the moment it
// would have done it. A suspended Automation announces nothing, for the same
// reason it fires no edge action: it is not acting, and it is not pretending to.
func (r *AutomationReconciler) reportPreview(
	automation *reactorv1alpha1.Automation,
	outcomes []targetOutcome,
) {
	if !automation.Spec.DryRun {
		return
	}
	r.event(automation, corev1.EventTypeNormal, reasonDryRun, actionExecute,
		"dry run: nothing was written. In force, this automation would %s", describePreviews(outcomes))
}

// describeManaged names the targets another controller is driving, and which
// one, because "somebody else owns this" is not actionable until you know who.
func describeManaged(outcomes []targetOutcome) string {
	var managed []string
	for _, outcome := range outcomes {
		if outcome.managedBy != "" {
			managed = append(managed, fmt.Sprintf("%s is driven by %s", outcome.ref, outcome.managedBy))
		}
	}
	return strings.Join(managed, "; ")
}

// describePreviews renders what an out-of-force Automation's targets would
// become, in the words its levels are reported in rather than as bare numbers —
// "0 replicas" and "suspended" survive being read at 3am, "0" does not.
func describePreviews(outcomes []targetOutcome) string {
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		preview := outcome.preview
		if preview == nil || preview.Effective == nil {
			continue
		}
		part := fmt.Sprintf("hold %s at %s", outcome.ref, preview.Level)
		switch {
		case len(preview.DeferredBy) > 0:
			part = fmt.Sprintf("leave %s at %s, outvoted by %s",
				outcome.ref, preview.Level, strings.Join(preview.DeferredBy, ", "))
		case len(preview.WouldDefer) > 0:
			part += ", outvoting " + strings.Join(preview.WouldDefer, ", ")
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "change nothing: it claims no target"
	}
	return strings.Join(parts, "; ")
}

// eventsForTargets announces the writes this reconcile actually made. Only
// changed outcomes produce one: a target already at the value it should be is
// the steady state, and reporting it every fifteen seconds would bury the two
// that matter.
func (r *AutomationReconciler) eventsForTargets(automation *reactorv1alpha1.Automation, outcomes []targetOutcome) {
	for _, outcome := range outcomes {
		if !outcome.changed {
			continue
		}
		if outcome.managedBy != "" {
			// Handing a claim back to a controller that took the target over,
			// which reads nothing like an ordinary release: the standing state
			// afterwards is that Reactor does not act here, and the reason it
			// is a Warning is that somebody has to know their automation has
			// stopped covering this workload.
			r.event(automation, corev1.EventTypeWarning, reasonTargetManagedByHPA, actionRelease,
				"%s is now driven by %s; handed it back at %s and stopped claiming it",
				outcome.ref, outcome.managedBy, outcome.level)
			continue
		}
		if outcome.effective == nil {
			r.event(automation, corev1.EventTypeNormal, reasonTargetReleased, actionRelease,
				"%s released; no automation claims it any more", outcome.ref)
			continue
		}
		r.event(automation, corev1.EventTypeNormal, reasonTargetHeld, actionExecute,
			"%s held at %s", outcome.ref, outcome.level)
	}
}
