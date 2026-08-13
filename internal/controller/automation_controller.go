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
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/providers/unifi"
)

const (
	conditionReady = "Ready"
	// providerUniFi is the provider name UniFi observations are stored under.
	providerUniFi = unifi.ProviderName
	// actionKubernetesScale is the only action type implemented in v0.1.
	actionKubernetesScale = "kubernetes.scale"
	// reevaluateInterval bounds how stale a matching decision can get relative
	// to the poller's StateStore when nothing else triggers a reconcile.
	reevaluateInterval = 15 * time.Second
)

// AutomationReconciler reconciles a Automation object against the provider
// state observed by the pollers feeding the shared StateStore.
type AutomationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Store  *engine.StateStore
}

// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reactor.robbeverhelst.com,resources=automations/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch

// Reconcile evaluates one Automation against current provider state. Actions
// run only on transitions: entering the matching state runs spec.actions,
// leaving it runs spec.onExit. Repeated identical observations are no-ops.
func (r *AutomationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var automation reactorv1alpha1.Automation
	if err := r.Get(ctx, req.NamespacedName, &automation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if automation.Spec.When == nil {
		// Event triggers are scheduled for v0.2; the schema exists but the
		// engine does not process them yet.
		r.setReady(&automation, metav1.ConditionFalse, "EventTriggersNotImplemented",
			"spec.trigger automations are not processed yet (v0.2)")
		return ctrl.Result{}, r.Status().Update(ctx, &automation)
	}

	observation, ok := r.Store.Get(automation.Spec.When.Provider)
	if !ok {
		r.setReady(&automation, metav1.ConditionFalse, "ProviderStateUnavailable",
			fmt.Sprintf("no state observed yet for provider %q", automation.Spec.When.Provider))
		if err := r.Status().Update(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	matching := true
	observed := map[string]string{}
	var missing []string
	for key, want := range automation.Spec.When.State {
		got, present := observation.State[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		observed[key] = got
		if got != want {
			matching = false
		}
	}

	// Losing sight of a state key is not evidence that the condition stopped
	// holding: the hardware reporting it may simply have dropped off the
	// controller. Treating it as "no longer matching" would run onExit — e.g.
	// scaling workloads back up in the middle of a power failure. Hold the
	// current matching state instead and say so in status.
	if len(missing) > 0 {
		sort.Strings(missing)
		automation.Status.ObservedState = observed
		r.setReady(&automation, metav1.ConditionFalse, "StateKeyUnavailable",
			fmt.Sprintf("provider %q is not reporting %s; holding last known state",
				automation.Spec.When.Provider, strings.Join(missing, ", ")))
		if err := r.Status().Update(ctx, &automation); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
	}

	wasMatching := automation.Status.Matching
	if matching != wasMatching {
		actions := automation.Spec.Actions
		onExit := !matching
		if onExit {
			actions = automation.Spec.OnExit
		}
		log.Info("state transition", "automation", automation.Name, "matching", matching, "onExit", onExit)

		execution := &reactorv1alpha1.ExecutionStatus{Time: metav1.Now(), OnExit: onExit, Status: "Success"}
		for _, action := range actions {
			if err := r.execute(ctx, &automation, action); err != nil {
				execution.Status = "Failed"
				execution.Reason = err.Error()
				automation.Status.LastExecution = execution
				r.setReady(&automation, metav1.ConditionTrue, "ActionFailed", err.Error())
				if statusErr := r.Status().Update(ctx, &automation); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{}, err // controller-runtime backoff retries
			}
		}
		if len(actions) > 0 {
			automation.Status.LastExecution = execution
		}
		automation.Status.Matching = matching
		automation.Status.LastTransition = r.transitionFor(&automation, observation.State)
	}

	automation.Status.ObservedState = observed
	r.setReady(&automation, metav1.ConditionTrue, "Reconciled", "automation evaluated against observed state")
	if err := r.Status().Update(ctx, &automation); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: reevaluateInterval}, nil
}

// transitionFor records the first state key whose value differs from the
// previously observed one — with single-key triggers (the MVP case) this is
// exactly the transition that flipped matching.
func (r *AutomationReconciler) transitionFor(automation *reactorv1alpha1.Automation, current map[string]string) *reactorv1alpha1.StateTransition {
	for key := range automation.Spec.When.State {
		prev := automation.Status.ObservedState[key]
		if prev != current[key] {
			return &reactorv1alpha1.StateTransition{Key: key, From: prev, To: current[key], Time: metav1.Now()}
		}
	}
	return automation.Status.LastTransition
}

func (r *AutomationReconciler) execute(ctx context.Context, automation *reactorv1alpha1.Automation, action reactorv1alpha1.Action) error {
	switch action.Type {
	case actionKubernetesScale:
		return r.scale(ctx, automation, action)
	default:
		return fmt.Errorf("unsupported action type %q", action.Type)
	}
}

func (r *AutomationReconciler) scale(ctx context.Context, automation *reactorv1alpha1.Automation, action reactorv1alpha1.Action) error {
	if action.Target == nil || action.Replicas == nil {
		return fmt.Errorf("kubernetes.scale requires target and replicas")
	}
	namespace := action.Target.Namespace
	if namespace == "" {
		namespace = automation.Namespace
	}

	var deployment appsv1.Deployment
	key := types.NamespacedName{Namespace: namespace, Name: action.Target.Name}
	if err := r.Get(ctx, key, &deployment); err != nil {
		if errors.IsForbidden(err) {
			return fmt.Errorf("target %s/%s not reachable with current RBAC (cross-namespace targets need cluster-wide permissions): %w", namespace, action.Target.Name, err)
		}
		return fmt.Errorf("getting target %s/%s: %w", namespace, action.Target.Name, err)
	}
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == *action.Replicas {
		return nil // desired state already holds; retries stay idempotent
	}
	patch := client.MergeFrom(deployment.DeepCopy())
	deployment.Spec.Replicas = action.Replicas
	if err := r.Patch(ctx, &deployment, patch); err != nil {
		return fmt.Errorf("scaling %s/%s to %d: %w", namespace, action.Target.Name, *action.Replicas, err)
	}
	logf.FromContext(ctx).Info("executed action", "type", action.Type,
		"target", fmt.Sprintf("deployment/%s/%s", namespace, action.Target.Name), "replicas", *action.Replicas)
	return nil
}

func (r *AutomationReconciler) setReady(automation *reactorv1alpha1.Automation, status metav1.ConditionStatus, reason, message string) {
	meta := metav1.Condition{
		Type: conditionReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: automation.Generation,
	}
	for i, c := range automation.Status.Conditions {
		if c.Type == conditionReady {
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&reactorv1alpha1.Automation{}).
		Named("automation").
		Complete(r)
}
