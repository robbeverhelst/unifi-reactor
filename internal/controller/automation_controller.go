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
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/providers/unifi"
)

const (
	// conditionReady reports whether the Automation is valid and reconciling.
	conditionReady = "Ready"
	// conditionApplied reports whether what this Automation wants for its
	// targets is what those targets actually have. The two are separate
	// because an Automation can be perfectly healthy and still not be the one
	// deciding a target's value.
	conditionApplied = "Applied"
	// providerUniFi is the provider name UniFi observations are stored under.
	providerUniFi = unifi.ProviderName
	// actionKubernetesScale is the only action type implemented in v0.1.
	actionKubernetesScale = "kubernetes.scale"
	// reevaluateInterval bounds how stale a matching decision can get relative
	// to the poller's StateStore when nothing else triggers a reconcile.
	reevaluateInterval = 15 * time.Second
	// releaseRetryInterval is the base delay between attempts to hand a
	// deleted Automation's targets back.
	releaseRetryInterval = 5 * time.Second
	// defaultActionTimeout bounds one attempt at an action when the Automation
	// does not set spec.actions[].timeoutSeconds.
	defaultActionTimeout = 30 * time.Second
	// maxActionAttempts is how many consecutive failures a target gets before
	// Reactor stops retrying it and waits for the state to change instead.
	maxActionAttempts = 5
	// retryBackoffBase and retryBackoffCap bound the exponential delay between
	// those attempts.
	retryBackoffBase = 2 * time.Second
	retryBackoffCap  = time.Minute
	// maxConcurrentReconciles keeps one Automation stuck on an unreachable
	// target from stalling every other Automation behind it.
	maxConcurrentReconciles = 4
)

// attemptsAfter is the attempt number of the failure about to be recorded. A
// run that last succeeded starts the count again, so a target that recovers
// and fails later gets the full budget rather than the remainder of an old one.
func attemptsAfter(last *reactorv1alpha1.ExecutionStatus) int32 {
	if last == nil || last.Status != "Failed" {
		return 1
	}
	return last.Attempts + 1
}

// retryBackoff is the delay before the next attempt: exponential from
// retryBackoffBase, capped so a long-running failure settles into a steady
// retry rather than drifting towards never trying again.
func retryBackoff(attempts int32) time.Duration {
	backoff := retryBackoffBase << (attempts - 1)
	if backoff > retryBackoffCap || backoff <= 0 {
		return retryBackoffCap
	}
	return backoff
}

// desiredStateActions are the action types that express a level — what a
// target should be — rather than an occurrence. Only these are arbitrated
// across the Automations sharing a target, and only these are reconciled
// continuously rather than run on a transition.
//
// Action types absent from this set are edge actions: they fire on their own
// Automation's transitions, own no target and take part in no arbitration.
// None exist yet. kubernetes.restart (#16) and the notification actions (#19)
// will be the first, and they attach to the transition branch in Reconcile
// rather than to arbitration.
var desiredStateActions = map[string]bool{
	actionKubernetesScale: true,
}

func isDesiredState(actionType string) bool {
	return desiredStateActions[actionType]
}

// AutomationReconciler reconciles a Automation object against the provider
// state observed by the pollers feeding the shared StateStore.
type AutomationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Store  *engine.StateStore

	// Wake carries Automations that a provider has observed a state change
	// for, so they reconcile immediately rather than on their next periodic
	// re-evaluation. Optional; without it reactions lag by up to one
	// reevaluateInterval.
	Wake <-chan event.GenericEvent

	// Recorder surfaces the cases an operator has to know about but would
	// otherwise only find in logs — notably giving up on releasing a deleted
	// Automation's targets.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// evaluation is one Automation's condition assessed against current state.
type evaluation struct {
	// matching is whether every key the Automation asks for holds its wanted
	// value. Only meaningful when known is true and missing is empty.
	matching bool
	// observed is the subset of provider state this Automation cares about.
	observed map[string]string
	// missing lists keys the provider has stopped reporting.
	missing []string
	// known is false when the provider has never reported anything.
	known bool
}

// evaluate assesses an Automation's condition against the current observation.
// It never consults status, so the same function serves both the Automation
// being reconciled and its peers.
func (r *AutomationReconciler) evaluate(automation *reactorv1alpha1.Automation) evaluation {
	assessment := evaluation{matching: true, observed: map[string]string{}}
	if automation.Spec.When == nil {
		return assessment
	}

	observation, ok := r.Store.Get(automation.Spec.When.Provider)
	if !ok {
		return assessment
	}
	assessment.known = true

	for key, want := range automation.Spec.When.State {
		got, present := observation.State[key]
		if !present {
			assessment.missing = append(assessment.missing, key)
			continue
		}
		assessment.observed[key] = got
		if got != want {
			assessment.matching = false
		}
	}
	slices.Sort(assessment.missing)
	return assessment
}

// Reconcile evaluates one Automation against current provider state and
// reconciles the desired state of every target it references.
//
// A target's value is arbitrated across every Automation referencing it rather
// than written by whichever one last saw a transition, so the outcome depends
// only on which conditions currently hold — not on the order they were
// observed in. Targets are therefore reconciled on every pass, not just on
// this Automation's own transitions: a peer entering or leaving its state
// changes what a shared target should be without anything about this one
// changing.
func (r *AutomationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var automation reactorv1alpha1.Automation
	if err := r.Get(ctx, req.NamespacedName, &automation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !automation.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &automation)
	}

	if automation.Spec.When == nil {
		// Event triggers are scheduled for v0.2; the schema exists but the
		// engine does not process them yet.
		r.setCondition(&automation, conditionReady, metav1.ConditionFalse, "EventTriggersNotImplemented",
			"spec.trigger automations are not processed yet (v0.2)")
		return ctrl.Result{}, r.Status().Update(ctx, &automation)
	}

	// Registered before the first claim is ever made, so there is no window in
	// which Reactor holds a target it would not hand back on deletion.
	if len(targetsOf(&automation)) > 0 && controllerutil.AddFinalizer(&automation, finalizerReleaseClaims) {
		if err := r.Update(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
	}

	assessment := r.evaluate(&automation)
	if !assessment.known {
		r.setCondition(&automation, conditionReady, metav1.ConditionFalse, "ProviderStateUnavailable",
			fmt.Sprintf("no state observed yet for provider %q", automation.Spec.When.Provider))
		if err := r.Status().Update(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	// Losing sight of a state key is not evidence that the condition stopped
	// holding: the hardware reporting it may simply have dropped off the
	// controller. Treating it as "no longer matching" would drop this
	// Automation's claim — e.g. releasing workloads in the middle of a power
	// failure. Hold the current matching state instead and say so in status.
	if len(assessment.missing) > 0 {
		automation.Status.ObservedState = assessment.observed
		r.setCondition(&automation, conditionReady, metav1.ConditionFalse, "StateKeyUnavailable",
			fmt.Sprintf("provider %q is not reporting %s; holding last known state",
				automation.Spec.When.Provider, strings.Join(assessment.missing, ", ")))
		if err := r.Status().Update(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	matching := assessment.matching
	wasMatching := automation.Status.Matching
	if matching != wasMatching {
		log.Info("state transition", "automation", automation.Name, "matching", matching)
	}

	outcomes, applyErr := r.reconcileTargets(ctx, &automation, matching)

	if applyErr == nil {
		if matching != wasMatching {
			automation.Status.LastTransition = r.transitionFor(&automation, assessment.observed)
		}
		automation.Status.Matching = matching
	}
	if changed := slices.ContainsFunc(outcomes, func(o targetOutcome) bool { return o.changed }); changed {
		automation.Status.LastExecution = &reactorv1alpha1.ExecutionStatus{
			Time: metav1.Now(), OnExit: !matching, Status: "Success",
		}
	}
	automation.Status.ObservedState = assessment.observed
	automation.Status.Targets = targetStatuses(outcomes)

	if applyErr != nil {
		attempts := attemptsAfter(automation.Status.LastExecution)
		automation.Status.LastExecution = &reactorv1alpha1.ExecutionStatus{
			Time: metav1.Now(), OnExit: !matching, Status: "Failed",
			Reason: applyErr.Error(), Attempts: attempts,
		}
		r.setCondition(&automation, conditionReady, metav1.ConditionFalse, "ActionFailed", applyErr.Error())

		// Retry is bounded here rather than by returning the error and
		// inheriting controller-runtime's requeue, so that giving up is a
		// decision with a visible reason instead of an unbounded backoff
		// nobody can see the end of.
		if attempts >= maxActionAttempts {
			r.setCondition(&automation, conditionApplied, metav1.ConditionFalse, "RetryBudgetExhausted",
				fmt.Sprintf("giving up after %d attempts, will try again on the next state change: %v",
					attempts, applyErr))
			log.Error(applyErr, "giving up on target after repeated failures",
				"automation", automation.Name, "attempts", attempts)
			if err := r.Status().Update(ctx, &automation); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
		}

		r.setCondition(&automation, conditionApplied, metav1.ConditionFalse, "ActionFailed", applyErr.Error())
		if err := r.Status().Update(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: retryBackoff(attempts)}, nil
	}

	r.setCondition(&automation, conditionReady, metav1.ConditionTrue, "Reconciled",
		"automation evaluated against observed state")
	r.setAppliedCondition(&automation, outcomes)
	if err := r.Status().Update(ctx, &automation); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
}

// finalize hands a deleted Automation's targets back before letting it go.
//
// Deletion needs no special arbitration case: an Automation being deleted is
// simply one that no longer claims anything, so reconciling its targets with
// matching=false releases them if nothing else claims them, and leaves them
// alone if something does. The reversal it declared still applies, which is
// what makes deleting an Automation mid-outage restore the workload rather
// than strand it.
func (r *AutomationReconciler) finalize(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(automation, finalizerReleaseClaims) {
		return ctrl.Result{}, nil
	}

	_, err := r.reconcileTargets(ctx, automation, false)
	if err != nil {
		attempts := automation.Status.ReleaseAttempts + 1
		if attempts < maxReleaseAttempts {
			automation.Status.ReleaseAttempts = attempts
			r.setCondition(automation, conditionApplied, metav1.ConditionFalse, "ReleaseFailed", err.Error())
			if statusErr := r.Status().Update(ctx, automation); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: time.Duration(attempts) * releaseRetryInterval}, nil
		}
		// Out of attempts. Let the object go anyway: a workload left where it
		// was is recoverable — the baseline annotation still records what it
		// was before — but a resource stuck terminating forever is not.
		log.Error(err, "giving up releasing targets, removing finalizer anyway",
			"automation", claimantOf(automation), "attempts", attempts)
		if r.Recorder != nil {
			r.Recorder.Eventf(automation, nil, corev1.EventTypeWarning, "ReleaseFailed", "Release",
				"could not hand targets back after %d attempts, deleting anyway: %v", attempts, err)
		}
	}

	controllerutil.RemoveFinalizer(automation, finalizerReleaseClaims)
	return ctrl.Result{}, r.Update(ctx, automation)
}

// reconcileTargets arbitrates and writes every target this Automation
// references, stopping at the first failure so the error surfaces rather than
// being masked by a later success.
func (r *AutomationReconciler) reconcileTargets(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	matching bool,
) ([]targetOutcome, error) {
	var outcomes []targetOutcome
	for _, key := range targetsOf(automation) {
		outcome, err := r.reconcileTarget(ctx, key, automation, matching)
		outcomes = append(outcomes, outcome)
		if err != nil {
			return outcomes, err
		}
	}
	return outcomes, nil
}

func targetStatuses(outcomes []targetOutcome) []reactorv1alpha1.TargetStatus {
	if len(outcomes) == 0 {
		return nil
	}
	statuses := make([]reactorv1alpha1.TargetStatus, 0, len(outcomes))
	for _, outcome := range outcomes {
		statuses = append(statuses, reactorv1alpha1.TargetStatus{
			Ref:        outcome.ref,
			Desired:    outcome.desired,
			Effective:  outcome.effective,
			DeferredBy: outcome.deferredBy,
		})
	}
	return statuses
}

// setAppliedCondition reports whether this Automation's intent is the one in
// effect. Being outvoted is a normal, healthy outcome — it is how two
// Automations sharing a workload are meant to behave — so it is reported as
// Applied=False with an explanation rather than as an error.
func (r *AutomationReconciler) setAppliedCondition(
	automation *reactorv1alpha1.Automation,
	outcomes []targetOutcome,
) {
	var deferred []string
	for _, outcome := range outcomes {
		if len(outcome.deferredBy) > 0 {
			deferred = append(deferred, fmt.Sprintf("%s held by %s",
				outcome.ref, strings.Join(outcome.deferredBy, ", ")))
		}
	}
	if len(deferred) > 0 {
		r.setCondition(automation, conditionApplied, metav1.ConditionFalse, "DeferredToOtherAutomation",
			strings.Join(deferred, "; "))
		return
	}
	r.setCondition(automation, conditionApplied, metav1.ConditionTrue, "InEffect",
		"target state matches what this automation wants")
}

// transitionFor records the first state key whose value differs from the
// previously observed one — with single-key triggers (the MVP case) this is
// exactly the transition that flipped matching.
func (r *AutomationReconciler) transitionFor(
	automation *reactorv1alpha1.Automation,
	current map[string]string,
) *reactorv1alpha1.StateTransition {
	for key := range automation.Spec.When.State {
		prev := automation.Status.ObservedState[key]
		if prev != current[key] {
			return &reactorv1alpha1.StateTransition{
				Key: key, From: prev, To: current[key], Time: metav1.Now(),
			}
		}
	}
	return automation.Status.LastTransition
}

func (r *AutomationReconciler) setCondition(
	automation *reactorv1alpha1.Automation,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta := metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: automation.Generation,
	}
	for i, c := range automation.Status.Conditions {
		if c.Type == conditionType {
			if c.Status != status || c.Reason != reason || c.Message != message {
				meta.LastTransitionTime = metav1.Now()
				automation.Status.Conditions[i] = meta
			}
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	automation.Status.Conditions = append(automation.Status.Conditions, meta)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AutomationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&reactorv1alpha1.Automation{}).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Named("automation")
	if r.Wake != nil {
		builder = builder.WatchesRawSource(source.Channel(r.Wake, &handler.EnqueueRequestForObject{}))
	}
	return builder.Complete(r)
}
