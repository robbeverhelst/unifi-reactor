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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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
func ReleaseAllClaims(ctx context.Context, c client.Client) error {
	log := logf.FromContext(ctx)

	var list reactorv1alpha1.AutomationList
	if err := c.List(ctx, &list); err != nil {
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
