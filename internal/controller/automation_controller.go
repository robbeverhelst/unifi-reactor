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
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
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
	// reasonTriggerRemoved is reported for an Automation left over from when
	// the API had a second, never-implemented trigger kind.
	reasonTriggerRemoved = "EventTriggerRemoved"
	// reasonSuspended is reported while spec.suspend takes an Automation out
	// of force.
	reasonSuspended = "Suspended"
	// reasonActionFailed is reported when a desired-state action could not be
	// applied to its target.
	reasonActionFailed = "ActionFailed"
	// actionKubernetesScale holds a workload at a replica count.
	actionKubernetesScale = "kubernetes.scale"
	// actionCronJobSuspend holds a CronJob at suspended or running. Suspending
	// stops new Jobs being created and deliberately leaves a Job already
	// running alone: declining to start more work is a different and far safer
	// act than killing work in flight.
	actionCronJobSuspend = "kubernetes.cronjob.suspend"
	// actionKubernetesCordon holds a Node at schedulable or unschedulable. It is
	// desired-state for the same reason suspend is: schedulable is a level, it
	// is reversible, and applying it twice is applying it once.
	//
	// Its sibling in #18, kubernetes.drain, is deliberately not here and is not
	// implemented anywhere. See docs/spec.md for the reasoning; the short form
	// is that an eviction cannot be un-evicted, so a drain has no level to
	// arbitrate, no reversal to declare, and no way to be a pure function of
	// which conditions currently hold.
	actionKubernetesCordon = "kubernetes.cordon"
	// actionKubernetesRestart rolls a workload's pods, the way `kubectl rollout
	// restart` does. It is an edge action: a restart is an occurrence, not a
	// level — there is no value a target can be held at that means "restarted",
	// and nothing to arbitrate with a peer over.
	actionKubernetesRestart = "kubernetes.restart"
	// actionPrefixKubernetes marks the actions that act on the cluster rather
	// than leave it, which is what decides whether an edge action goes through
	// the outbound client and its destination allowlist.
	actionPrefixKubernetes = "kubernetes."
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
	//
	// This is the desired-state half of the retry policy, and it is generous
	// because it costs nothing to be: a desired-state action is idempotent by
	// construction, so the reconcile loop is already the retry and this only
	// decides how eagerly it runs.
	//
	// The edge half is the opposite shape and lives in edge_actions.go. An edge
	// action fires on an occurrence that has already passed, so it is never
	// retried across reconciles — a later reconcile has no new transition, and
	// re-sending there would be a duplicate rather than a retry. Whether it may
	// be repeated at all within its one reconcile is decided per type:
	//
	//   - notification.*    retried. Every transport shipped is a publish, so a
	//                       duplicate is noise on a phone and a miss is the
	//                       failure the feature exists to prevent.
	//   - http.request      retried only when the method is idempotent by RFC
	//                       9110, or the author declares it so. See retryable().
	//   - homeassistant.service
	//                       AT-MOST-ONCE unless the author sets idempotent.
	//                       light.turn_on is safe to repeat; script.turn_on,
	//                       notify.* and button.press are not, and the action
	//                       cannot tell which one it was handed. The author can,
	//                       which is why the declaration is theirs to make.
	//   - qbittorrent.*     retried. This is the one edge action that can argue
	//                       idempotence rather than assert it: pausing a paused
	//                       torrent is a no-op and so is resuming a running one.
	//                       A retry re-runs the whole exchange, login included,
	//                       and a rejected credential is not transient so it
	//                       stops at the first attempt.
	//   - kubernetes.restart AT-MOST-ONCE, unconditionally. Every execution rolls
	//                       the workload, so a retry after an ambiguous failure
	//                       is a second outage rather than a correction — and
	//                       the failures that matter here (a conflict, a
	//                       Forbidden) are not the kind a retry fixes.
	//   - unifi.wlan.*      AT-MOST-ONCE, unconditionally. Turning a WLAN on or
	//                       off is idempotent in the world, so this is not about
	//                       the effect: the write is a read-modify-write against
	//                       an undocumented endpoint with no version to compare
	//                       against, so a retry after an ambiguous failure
	//                       re-reads a document the failed attempt may already
	//                       have half-changed. The conservative reading of an
	//                       ambiguous console write is that it happened. The
	//                       action reads before it writes and does nothing when
	//                       the WLAN is already where it should be, so the next
	//                       transition corrects a miss rather than a retry.
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
	if last == nil || last.Status != executionFailed {
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
// http.request and notification.* are the first of those; they are executed by
// runEdgeActions, off the matching != wasMatching branch in Reconcile, and
// their retry policy is the one recorded on maxActionAttempts below.
//
// unifi.wlan.* is the case that makes the rule below worth stating precisely. A
// WLAN being enabled is a level, and it is still an edge action, because the
// meet is not what is missing: what is missing is anywhere to record the value
// the WLAN held before Reactor changed it that would outlive this Automation
// and be readable by an uninstall. See the WLAN type in api/v1alpha1.
//
// The rule for a new action type: if you cannot define a meet with an identity
// element for it, it is an edge action and belongs out of this map.
var desiredStateActions = map[string]bool{
	actionKubernetesScale:  true,
	actionCronJobSuspend:   true,
	actionKubernetesCordon: true,
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
	// Automation's targets, and every edge action that did or did not fire.
	Recorder events.EventRecorder

	// Outbound sends the edge actions that leave the cluster. Optional; nil
	// means every such action is refused with a reason rather than attempted.
	Outbound actions.Doer

	// DryRun makes this whole install evaluate and report without writing.
	//
	// Every Automation is arbitrated exactly as it otherwise would be, and the
	// resolved value is then not applied: status.targets[] reports what each
	// target would be held at, Applied is False with reason DryRun, and no edge
	// action is sent. That is deliberately not the same thing as
	// spec.dryRun on one Automation, which takes that Automation out of force
	// so it cannot perturb the live ones around it. Here nothing is live, so
	// there is nothing to perturb and the honest report is the real fold.
	//
	// It is a promise made in code. The chart pairs it with withholding the
	// verbs that could write to a target, so that on an install meant never to
	// touch a workload the API server is the one keeping the promise.
	DryRun bool

	// DetectHPA makes Reactor check, before claiming a scalable target, whether
	// a HorizontalPodAutoscaler already drives it — and decline the target
	// rather than fight over it.
	//
	// Off by default, because it costs a permission the operator does not
	// otherwise need and because turning it on changes what an install already
	// in that fight does. With it off the behaviour is what it was: Reactor
	// writes, the HPA writes back, and the workload oscillates on the poll
	// interval.
	DetectHPA bool

	// Console performs the edge actions that write to a provider's own console.
	// Optional; nil means the provider is not configured on this install and
	// every such action is refused with a reason rather than attempted.
	Console ConsoleWriter

	// SecretReader reads action credentials. It must be an uncached reader —
	// the manager's APIReader — because a cached Get on a Secret starts an
	// informer that holds every Secret in the cluster in memory. Falls back to
	// Client when unset, which is only ever the case in tests.
	SecretReader client.Reader
}

// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations/finalizers,verbs=update
// Targets are read as unstructured objects through the uncached client, so a
// target kind needs get to read it and patch to write it — no list, no watch,
// and no informer holding every object of that kind in memory.
// A replica count is read and written through the scale subresource, so a
// scalable kind needs its parent for the annotations and its /scale for the
// level. That split is what keeps one executor serving every scalable kind.
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;patch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale,verbs=get;update
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;patch
//
// A HorizontalPodAutoscaler writes the same scale subresource Reactor does, and
// is a claimant arbitration cannot reach, so a target one drives is declined
// rather than fought over. list and nothing else: the HPAs of one namespace are
// read uncached as unstructured objects, the way targets are, so no informer
// holds every HPA in the cluster in memory and no get or watch is needed. This
// is a read of an autoscaling policy and never a write to one — Reactor
// declines to act, and deliberately does not suspend the HPA on your behalf.
//
// Unlike nodes this IS marked, so the manifest bundle carries it: it is
// read-only and reaches no workload. The chart still renders it only under
// safety.detectHPA, because there the value gating the behaviour exists.
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=list
//
// Nodes are deliberately NOT marked here, even though kubernetes.cordon targets
// them. These markers generate config/rbac/role.yaml, which is granted
// unconditionally by `make deploy` and by the manifest bundle — and node access
// is the one grant in this operator that reaches outside the workloads it was
// installed to manage, so it must be something an install opts into rather than
// something every install carries. The chart renders it under
// rbac.allowNodeActions; the bundle does not offer it at all, and an Automation
// that needs it says so in its status.
// mgr.GetEventRecorder returns the events.k8s.io/v1 recorder, not the deprecated
// core/v1 one. They share storage but are separate API groups for authorization,
// and a rule naming only "" fails every emission with a Forbidden the broadcaster
// logs and nothing else surfaces — the Automation just has no Events.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

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
	// observedAt is when the state this assessment was made against was
	// observed, so a reaction can be measured from the observation that caused
	// it rather than from the reconcile that noticed.
	observedAt time.Time
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
	assessment.observedAt = observation.ObservedAt

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
		if err = client.IgnoreNotFound(err); err == nil {
			// Gone for good: drop its series rather than leave a deleted policy
			// reporting a condition it no longer has.
			metrics.ForgetAutomation(req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}

	if !automation.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &automation)
	}

	if automation.Spec.When == nil {
		// Written before spec.trigger was removed from v1alpha1: the schema no
		// longer offers a second trigger kind, but objects that used one
		// survive in etcd. They never did anything — the engine never
		// processed event triggers — so this is a report, not a migration.
		//
		// Deliberately no status write: spec.when is now required, so the API
		// server rejects any update to an object that has none, status
		// subresource included. Saying so once per reconcile beats an
		// error-backoff loop nobody can act on.
		log.Info("automation has no spec.when and will never act; spec.trigger was removed from v1alpha1, delete it",
			"automation", claimantOf(&automation))
		r.event(&automation, corev1.EventTypeWarning, reasonTriggerRemoved, actionReconcile,
			"spec.trigger was removed from v1alpha1 and was never implemented; this automation does nothing, delete it")
		return ctrl.Result{}, nil
	}

	// Registered before the first claim is ever made, so there is no window in
	// which Reactor holds a target it would not hand back on deletion.
	//
	// A dry-run install is the one case that must not carry one, and taking it
	// back off is not tidiness. The finalizer exists to release a claim; a dry
	// run makes none, so it would have nothing to do and no way to do it —
	// releasing is a write. Left in place it becomes the trap #39 documents:
	// an Automation nothing will ever finalize, and a `kubectl delete` that
	// hangs forever once the operator is gone.
	if err := r.reconcileFinalizer(ctx, &automation); err != nil {
		return ctrl.Result{}, err
	}

	// A suspended Automation is out of force, so its condition no longer
	// decides anything and the two guards below — which exist to stop it
	// acting on state it cannot see — have nothing to protect. Skipping them
	// is what makes pausing work during the incident you are pausing for: the
	// console being unreachable must not be able to keep a claim alive.
	inForce := automation.InForce()

	assessment := r.evaluate(&automation)
	if inForce && !assessment.known {
		r.setCondition(&automation, conditionReady, metav1.ConditionFalse, "ProviderStateUnavailable",
			fmt.Sprintf("no state observed yet for provider %q", automation.Spec.When.Provider))
		if err := r.updateStatus(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	// Losing sight of a state key is not evidence that the condition stopped
	// holding: the hardware reporting it may simply have dropped off the
	// controller. Treating it as "no longer matching" would drop this
	// Automation's claim — e.g. releasing workloads in the middle of a power
	// failure. Hold the current matching state instead and say so in status.
	if inForce && len(assessment.missing) > 0 {
		automation.Status.ObservedState = assessment.observed
		r.eventOnNewReason(&automation, conditionReady, corev1.EventTypeWarning,
			reasonStateKeyUnavailable, actionEvaluate,
			"provider %q stopped reporting %s; holding the last known state rather than treating lost sight of it as the condition ending",
			automation.Spec.When.Provider, strings.Join(assessment.missing, ", "))
		r.setCondition(&automation, conditionReady, metav1.ConditionFalse, reasonStateKeyUnavailable,
			fmt.Sprintf("provider %q is not reporting %s; holding last known state",
				automation.Spec.When.Provider, strings.Join(assessment.missing, ", ")))
		if err := r.updateStatus(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	// A suspended Automation keeps reporting what it observes — that is most of
	// what makes it useful to leave in place while debugging — but it holds its
	// last known matching whenever the state it needs is unavailable, the same
	// rule as above.
	readable := assessment.known && len(assessment.missing) == 0
	matching := automation.Status.Matching
	if readable {
		matching = assessment.matching
	}
	wasMatching := automation.Status.Matching
	if matching != wasMatching {
		log.Info("state transition", "automation", automation.Name, "matching", matching, "inForce", inForce)
	}

	// claiming, not matching: an Automation held out of force — suspended, or
	// previewing — can have its condition hold without asking for anything.
	// Out of force is also where a preview belongs: it is the counterfactual an
	// Automation that is not acting owes an explanation of.
	claiming := matching && inForce
	outcomes, applyErr := r.reconcileTargets(ctx, &automation,
		stance{matching: matching, claiming: claiming, preview: !inForce})

	if applyErr == nil {
		if matching != wasMatching {
			automation.Status.LastTransition = r.transitionFor(&automation, assessment.observed)
		}
		automation.Status.Matching = matching
	}
	if changed := slices.ContainsFunc(outcomes, func(o targetOutcome) bool { return o.changed }); changed {
		automation.Status.LastExecution = &reactorv1alpha1.ExecutionStatus{
			Time: metav1.Now(), OnExit: !claiming, Status: executionSuccess,
		}
		metrics.ReactionCompleted(automation.Spec.When.Provider, assessment.observedAt)
	}
	// Raised before the failure branch below, because reconcileTargets stops at
	// the first failure and the targets it wrote before that one were still
	// written.
	r.eventsForTargets(&automation, outcomes)
	if readable {
		automation.Status.ObservedState = assessment.observed
	}
	automation.Status.Targets = targetStatuses(outcomes)

	if applyErr != nil {
		return r.recordApplyFailure(ctx, &automation, applyErr, claiming)
	}
	// After the failure branch: a transition whose targets could not be written
	// is not committed, so announcing it here would report a failover that has
	// not happened yet. It is announced on the retry that succeeds — the same
	// ordering rule the edge actions below follow.
	r.reportTransition(&automation, matching != wasMatching, matching)

	if inForce {
		r.setCondition(&automation, conditionReady, metav1.ConditionTrue, "Reconciled",
			"automation evaluated against observed state")
		r.setAppliedCondition(&automation, outcomes)
	} else {
		r.setOutOfForceConditions(&automation)
	}
	if err := r.updateStatus(ctx, &automation); err != nil {
		return ctrl.Result{}, err
	}

	// The edge, and the only place edge actions run. It is after the status
	// write on purpose: the write above is what makes this transition
	// observed, so anything fired before it would be sent again by the retry
	// that follows a conflicting or failed write.
	//
	// A suspended Automation reaches here with inForce false and sends nothing.
	// Suspension is a reversible delete, and a deleted Automation does not
	// announce transitions it is no longer acting on.
	if matching != wasMatching {
		if !inForce {
			// The moment a dry run almost acted, which is the half of "what
			// would this do?" that status cannot answer: status is a poll, and
			// nobody polls a resource at the moment it becomes interesting.
			r.reportPreview(&automation, outcomes)
			return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
		}
		if results := r.runEdgeActions(ctx, &automation, matching); len(results) > 0 {
			automation.Status.EdgeActions = results
			if err := r.updateStatus(ctx, &automation); err != nil {
				// Logged rather than returned: the actions have already been
				// sent, and requeueing would re-send them on the next pass.
				log.Error(err, "recording edge action results failed",
					"automation", claimantOf(&automation))
			}
		}
	}
	return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
}

// recordApplyFailure reports a target that could not be written and decides
// when to try again.
//
// status.matching is deliberately left where it was, which is what keeps the
// transition uncommitted: the next reconcile still sees an edge, so the
// desired-state action is retried and any edge action attached to the same
// transition fires once the target actually changed — not while it is still
// where it was.
func (r *AutomationReconciler) recordApplyFailure(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	applyErr error,
	claiming bool,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	attempts := attemptsAfter(automation.Status.LastExecution)
	automation.Status.LastExecution = &reactorv1alpha1.ExecutionStatus{
		Time: metav1.Now(), OnExit: !claiming, Status: executionFailed,
		Reason: applyErr.Error(), Attempts: attempts,
	}
	// Bounded by the retry budget rather than raised every pass: past it,
	// Reactor has stopped trying and has nothing new to report until the state
	// changes, so a fifteen-second heartbeat would be noise on top of silence.
	if attempts <= maxActionAttempts {
		r.eventOnNewReason(automation, conditionReady, corev1.EventTypeWarning,
			reasonActionFailed, actionExecute,
			"attempt %d of %d failed: %v", attempts, maxActionAttempts, applyErr)
	}
	r.setCondition(automation, conditionReady, metav1.ConditionFalse, reasonActionFailed, applyErr.Error())

	// Retry is bounded here rather than by returning the error and inheriting
	// controller-runtime's requeue, so that giving up is a decision with a
	// visible reason instead of an unbounded backoff nobody can see the end of.
	if attempts >= maxActionAttempts {
		r.eventOnNewReason(automation, conditionApplied, corev1.EventTypeWarning,
			reasonRetryBudgetExhausted, actionExecute,
			"gave up after %d attempts; waiting for the next state change rather than retrying forever: %v",
			attempts, applyErr)
		r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonRetryBudgetExhausted,
			fmt.Sprintf("giving up after %d attempts, will try again on the next state change: %v",
				attempts, applyErr))
		log.Error(applyErr, "giving up on target after repeated failures",
			"automation", automation.Name, "attempts", attempts)
		if err := r.updateStatus(ctx, automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonActionFailed, applyErr.Error())
	if err := r.updateStatus(ctx, automation); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: retryBackoff(attempts)}, nil
}

// reconcileFinalizer keeps the release finalizer present exactly while there is
// something for it to release: a target this Automation could be holding, on an
// install that writes at all.
func (r *AutomationReconciler) reconcileFinalizer(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
) error {
	wanted := len(targetsOf(automation)) > 0 && !r.DryRun

	changed := false
	if wanted {
		changed = controllerutil.AddFinalizer(automation, finalizerReleaseClaims)
	} else {
		changed = controllerutil.RemoveFinalizer(automation, finalizerReleaseClaims)
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, automation)
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

	// No preview: an Automation being deleted has no future to describe, and
	// the status it would be written into is about to stop existing.
	_, err := r.reconcileTargets(ctx, automation, stance{})
	if err != nil {
		attempts := automation.Status.ReleaseAttempts + 1
		if attempts < maxReleaseAttempts {
			automation.Status.ReleaseAttempts = attempts
			r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonReleaseFailed, err.Error())
			if statusErr := r.updateStatus(ctx, automation); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: time.Duration(attempts) * releaseRetryInterval}, nil
		}
		// Out of attempts. Let the object go anyway: a workload left where it
		// was is recoverable — the baseline annotation still records what it
		// was before — but a resource stuck terminating forever is not.
		log.Error(err, "giving up releasing targets, removing finalizer anyway",
			"automation", claimantOf(automation), "attempts", attempts)
		r.event(automation, corev1.EventTypeWarning, reasonReleaseFailed, actionRelease,
			"could not hand targets back after %d attempts, deleting anyway: %v", attempts, err)
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
	s stance,
) ([]targetOutcome, error) {
	var outcomes []targetOutcome
	for _, key := range targetsOf(automation) {
		started := time.Now()
		outcome, err := r.reconcileTarget(ctx, key, automation, s)
		metrics.ActionExecuted(actionTypeFor(automation, key, s.claiming), metrics.KindDesiredState,
			!s.claiming, err, time.Since(started))
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
			Level:      outcome.level,
			DeferredBy: outcome.deferredBy,
			Preview:    outcome.preview,
			ManagedBy:  outcome.managedBy,
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
	if len(outcomes) == 0 {
		// Nothing to be in effect: an Automation whose actions are all edge
		// actions owns no target. Reporting Applied=True here would be
		// vacuously true and would read as a claim it is not making.
		r.setCondition(automation, conditionApplied, metav1.ConditionTrue, "NoTargets",
			"this automation only has edge actions, so it holds no target")
		return
	}

	if r.DryRun {
		// The arbitration was resolved and the write was not made, so what this
		// Automation wants is emphatically not what its targets have — and
		// saying Applied=True because the fold agreed with it would be the one
		// report a dry run must never produce.
		r.eventOnNewReason(automation, conditionApplied, corev1.EventTypeNormal,
			reasonDryRun, actionExecute,
			"this install runs as a dry run: every target was arbitrated and nothing was written")
		r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonDryRun,
			"this install runs as a dry run: status.targets[].effective is what each target would be "+
				"held at, and nothing was written")
		return
	}

	if managed := describeManaged(outcomes); managed != "" {
		// Warning, unlike being outvoted by a peer. A deferred claim is the
		// arbitration working; this is an automation that cannot do its job at
		// all, and somebody has to decide which controller should own the
		// workload — Reactor is not going to decide it for them.
		r.eventOnNewReason(automation, conditionApplied, corev1.EventTypeWarning,
			reasonTargetManagedByHPA, actionExecute,
			"not claiming %s: arbitration cannot resolve a claimant it cannot see, and writing anyway "+
				"would oscillate rather than win", managed)
		r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonTargetManagedByHPA, managed)
		return
	}

	var deferred []string
	for _, outcome := range outcomes {
		if len(outcome.deferredBy) > 0 {
			deferred = append(deferred, fmt.Sprintf("%s held by %s",
				outcome.ref, strings.Join(outcome.deferredBy, ", ")))
		}
	}
	if len(deferred) > 0 {
		r.eventOnNewReason(automation, conditionApplied, corev1.EventTypeNormal,
			reasonDeferred, actionExecute,
			"a more restrictive claim is in effect: %s", strings.Join(deferred, "; "))
		r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonDeferred,
			strings.Join(deferred, "; "))
		return
	}
	r.setCondition(automation, conditionApplied, metav1.ConditionTrue, "InEffect",
		"target state matches what this automation wants")
}

// setOutOfForceConditions reports an Automation that is healthy and
// deliberately not acting.
//
// Ready, because being out of force is a state an operator chose and not a
// fault; not Applied, because what it wants is deliberately not what its
// targets are being held at. Reporting either the other way round would make
// somebody's own pause look like something to fix.
//
// Suspending and previewing are the same answer to arbitration — neither claims
// anything — so they differ only in what they say about it. The reason is what
// tells `kubectl describe` which of the two is the case, and it is the only
// place that distinction is visible on the object.
func (r *AutomationReconciler) setOutOfForceConditions(automation *reactorv1alpha1.Automation) {
	if automation.Spec.DryRun {
		r.setCondition(automation, conditionReady, metav1.ConditionTrue, reasonDryRun,
			"spec.dryRun is true: state is observed and evaluated, and nothing is written")
		r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonDryRun,
			"a dry run claims no target; status.targets[].preview reports what this would do in force")
		return
	}
	r.setCondition(automation, conditionReady, metav1.ConditionTrue, reasonSuspended,
		"spec.suspend is true: state is still observed, and no target is claimed")
	r.setCondition(automation, conditionApplied, metav1.ConditionFalse, reasonSuspended,
		"suspended, so this automation's targets are arbitrated as if it did not exist")
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
