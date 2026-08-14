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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
)

// annotationRestartedAt is the pod-template annotation `kubectl rollout
// restart` writes. Reactor uses the same one deliberately: a workload restarted
// by Reactor and one restarted by hand should be indistinguishable afterwards,
// and anything already reading this annotation — a dashboard, an audit, the
// next person running `kubectl rollout history` — keeps working.
const annotationRestartedAt = "kubectl.kubernetes.io/restartedAt"

// templateAnnotations is where a rollout-triggering annotation lives. Changing
// the pod template is what makes the workload controller roll its pods; nothing
// about the annotation's value matters beyond being different from last time.
var templateAnnotations = []string{fieldSpec, "template", "metadata", "annotations"}

// isKubernetesAction reports whether an action acts on this cluster rather than
// leaving it.
func isKubernetesAction(actionType string) bool {
	return strings.HasPrefix(actionType, actionPrefixKubernetes)
}

// runClusterAction performs one edge action against the cluster.
//
// It is at-most-once, and unconditionally so — there is no per-type retry
// decision here the way there is for an HTTP method, because there is no
// kubernetes edge action for which repeating is harmless. Every execution of
// kubernetes.restart rolls the workload, so retrying after an ambiguous failure
// is a second outage rather than a correction. The failures that actually occur
// here are a conflict or a Forbidden, and neither is fixed by trying again
// inside the same reconcile.
//
// The transition it fires on has already been committed to status by the time
// this runs, so a later reconcile sees no edge and does not fire it again
// either. A crash in the window between the two loses the restart, which is the
// right way round: a workload that did not get restarted is visible and fixable,
// a workload restarted twice during an incident is neither.
func (r *AutomationReconciler) runClusterAction(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
) (actions.Result, error) {
	key, ok := targetKeyFor(automation, action)
	if !ok {
		return actions.Result{}, fmt.Errorf("%s needs a target", action.Type)
	}
	result := actions.Result{Origin: key.String(), Attempts: 1}

	handler, err := handlerFor(key.Kind)
	if err != nil {
		return result, err
	}

	timeout := defaultActionTimeout
	if action.TimeoutSeconds != nil {
		timeout = time.Duration(*action.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := newTarget(handler)
	name := types.NamespacedName{Namespace: key.Namespace, Name: key.Name}
	if err := r.Get(ctx, name, target); err != nil {
		if outOfScope(err) {
			return result, fmt.Errorf("target %s not reachable with current RBAC (%s): %w",
				key, permissionHint(key.Kind), err)
		}
		return result, fmt.Errorf("getting target %s: %w", key, err)
	}

	if action.Type != actionKubernetesRestart {
		return result, fmt.Errorf("no executor for action %q", action.Type)
	}
	return result, restartTarget(ctx, r.Client, target)
}

// restartTarget rolls a workload's pods by stamping its pod template, which is
// exactly what `kubectl rollout restart` does.
//
// It patches the template rather than deleting pods: the workload controller
// then rolls them under whatever update strategy and disruption budget the
// workload already declares, so a restart during an incident respects the same
// availability rules a deploy does. Deleting pods would bypass both.
//
// The stamp has second granularity, which means two restarts inside the same
// second produce no change and therefore no second rollout. That is a
// coincidence rather than a design, and it is not the flapping protection —
// debounce is. See the notes on kubernetes.restart in the README.
func restartTarget(ctx context.Context, c client.Client, target *unstructured.Unstructured) error {
	patch := client.MergeFrom(target.DeepCopy())

	annotations, _, err := unstructured.NestedStringMap(target.Object, templateAnnotations...)
	if err != nil {
		return fmt.Errorf("reading the pod template of %s: %w", describeObject(target), err)
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[annotationRestartedAt] = metav1.Now().UTC().Format(time.RFC3339)
	if err := unstructured.SetNestedStringMap(target.Object, annotations, templateAnnotations...); err != nil {
		return fmt.Errorf("stamping the pod template of %s: %w", describeObject(target), err)
	}

	if err := c.Patch(ctx, target, patch); err != nil {
		return fmt.Errorf("restarting %s: %w", describeObject(target), err)
	}
	return nil
}
