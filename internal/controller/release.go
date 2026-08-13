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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
)

// finalizerReleaseClaims keeps a deleted Automation around just long enough to
// hand its targets back. Without it, deleting an Automation while its
// condition holds leaves whatever it scaled down stranded there, with nothing
// left running that knows the two facts are connected.
const finalizerReleaseClaims = "reactor.robbeverhelst.com/release-claims"

// maxReleaseAttempts bounds how hard deletion tries before giving up and
// letting the object go. A stranded workload is bad; a resource stuck
// terminating forever is worse, and the annotations Reactor leaves on the
// target still say what it was before.
const maxReleaseAttempts = 3

// ReleaseOptions bounds what a release sweep touches.
type ReleaseOptions struct {
	// Namespace scopes the sweep, and must be set to whatever the operator was
	// permitted to watch: listing at cluster scope is forbidden under
	// namespace-scoped RBAC, which would fail the hook and, with it, the
	// uninstall. Empty means every namespace.
	Namespace string

	// Manager names the operator's own Deployment, which is stopped before
	// anything is released. Zero skips that, for a sweep run by hand while
	// Reactor is already gone.
	Manager types.NamespacedName

	// ManagerStopTimeout bounds waiting for the operator to stop. Releasing
	// claims with it still running is worse than not waiting long enough, but
	// not by so much that an uninstall should hang on it.
	ManagerStopTimeout time.Duration
}

// defaultManagerStopTimeout is how long to wait for the operator to stop
// before releasing anyway.
const defaultManagerStopTimeout = 60 * time.Second

// stopManager scales the operator down and waits for it to stop running.
//
// Handing claims back while the controller is still watching does not work:
// Helm removes the operator's Deployment only after its pre-delete hooks have
// finished, so a controller still running simply re-claims every workload this
// sweep released — and re-adds the finalizer, which by then has nothing left
// to service it and turns a later `kubectl delete crd` into a hang.
//
// Failing to stop it is not fatal. A release that mostly works beats an
// uninstall that refuses to proceed, and the targets still carry the
// annotations that say what they were.
func stopManager(ctx context.Context, c client.Client, options ReleaseOptions) error {
	if options.Manager.Name == "" || options.Manager.Namespace == "" {
		return nil
	}
	log := logf.FromContext(ctx)

	var deployment appsv1.Deployment
	if err := c.Get(ctx, options.Manager, &deployment); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting the operator deployment %s: %w", options.Manager, err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		patch := client.MergeFrom(deployment.DeepCopy())
		stopped := int32(0)
		deployment.Spec.Replicas = &stopped
		if err := c.Patch(ctx, &deployment, patch); err != nil {
			return fmt.Errorf("stopping the operator deployment %s: %w", options.Manager, err)
		}
	}

	timeout := options.ManagerStopTimeout
	if timeout <= 0 {
		timeout = defaultManagerStopTimeout
	}
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			var current appsv1.Deployment
			if err := c.Get(ctx, options.Manager, &current); err != nil {
				return errors.IsNotFound(err), nil
			}
			return current.Status.Replicas == 0, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for the operator %s to stop: %w", options.Manager, err)
	}
	log.Info("stopped the operator before releasing its claims", "deployment", options.Manager.String())
	return nil
}

// ReleaseAllClaims hands every target Reactor holds back to whatever the
// Automations referencing it want once nothing claims it, and removes the
// finalizer from every Automation.
//
// This runs from the chart's pre-delete hook, because uninstalling Reactor
// does not delete the Automations: Helm never deletes CRDs installed from
// crds/, so the resources survive the uninstall, no finalizer ever fires, and
// every workload Reactor had scaled down would simply freeze there. Dropping
// the finalizers matters for the same reason — once the controller is gone
// nothing is left to service them, and a later `kubectl delete automation`
// (or `kubectl delete crd`) would hang forever.
//
// It is deliberately best-effort: one unreachable target must not be able to
// block an uninstall, so failures are logged and the next target is tried.
// Only being unable to enumerate the Automations at all is fatal.
func ReleaseAllClaims(ctx context.Context, c client.Client, options ReleaseOptions) error {
	log := logf.FromContext(ctx)

	if err := stopManager(ctx, c, options); err != nil {
		log.Error(err, "could not stop the operator first, releasing anyway")
	}

	var scope []client.ListOption
	if options.Namespace != "" {
		scope = append(scope, client.InNamespace(options.Namespace))
	}
	var list reactorv1alpha1.AutomationList
	if err := c.List(ctx, &list, scope...); err != nil {
		return fmt.Errorf("listing automations: %w", err)
	}

	// Grouped by target: a workload claimed by two Automations is released
	// once, to the value they agree on, rather than twice to whichever was
	// processed last.
	claimants := map[targetKey][]*reactorv1alpha1.Automation{}
	var order []targetKey
	for i := range list.Items {
		automation := &list.Items[i]
		if automation.Spec.When == nil {
			continue
		}
		for _, key := range targetsOf(automation) {
			if _, seen := claimants[key]; !seen {
				order = append(order, key)
			}
			claimants[key] = append(claimants[key], automation)
		}
	}

	failures := 0
	for _, key := range order {
		if err := releaseClaimedTarget(ctx, c, key, claimants[key]); err != nil {
			failures++
			log.Error(err, "releasing target failed, continuing", "target", key.String())
		}
	}

	for i := range list.Items {
		automation := &list.Items[i]
		if !controllerutil.RemoveFinalizer(automation, finalizerReleaseClaims) {
			continue
		}
		if err := c.Update(ctx, automation); err != nil {
			failures++
			log.Error(err, "removing finalizer failed, continuing",
				"automation", claimantOf(automation))
		}
	}

	log.Info("released claims", "targets", len(order), "failures", failures)
	return nil
}

// releaseClaimedTarget releases one target, treating every Automation that
// references it as no longer claiming — which is what Reactor going away
// means.
func releaseClaimedTarget(
	ctx context.Context,
	c client.Client,
	key targetKey,
	automations []*reactorv1alpha1.Automation,
) error {
	var deployment appsv1.Deployment
	name := types.NamespacedName{Namespace: key.Namespace, Name: key.Name}
	if err := c.Get(ctx, name, &deployment); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting target %s: %w", key, err)
	}

	baseline, recorded := baselineOf(&deployment)
	if !recorded {
		return nil // never claimed, so there is nothing to hand back
	}

	var reversals []engine.Intent
	for _, automation := range automations {
		if reversal, ok := reversalFor(automation, key, baseline); ok {
			reversals = append(reversals, reversal)
		}
	}
	var level *int32
	if release, ok := engine.Resolve(reversals); ok {
		value := int32(release.Level)
		level = &value
	}

	changed, err := releaseTarget(ctx, c, &deployment, level)
	if err != nil {
		return err
	}
	if changed {
		logf.FromContext(ctx).Info("released target", "target", key.String(), "replicas", level)
	}
	return nil
}
