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

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kindHorizontalPodAutoscaler is the one claimant Reactor cannot arbitrate with
// and can nevertheless see.
const kindHorizontalPodAutoscaler = "HorizontalPodAutoscaler"

// fieldScaleTargetRef is where an HPA names the object it drives.
const fieldScaleTargetRef = "scaleTargetRef"

// foreignManagerOf names a controller that already owns this target's level.
//
// Arbitration works because Reactor can see every claimant: the fold is over
// the Automations, and one that wants something different is resolved rather
// than fought. A HorizontalPodAutoscaler claims exactly the same field — it
// writes the scale subresource from metrics — and it is not an Automation, so
// there is nothing to fold it into. Writing anyway produces a fight neither
// side can win, and v1.0 made that fight louder rather than quieter: claims are
// re-asserted on every reconcile rather than once per transition, so what used
// to look like a one-off flap is now a sustained oscillation on the poll
// interval. Same bug, fifteen seconds apart, forever.
//
// So this answers one question — is somebody else already driving this field —
// and the answer is used to decline rather than to compete.
//
// It is not a general solution and cannot be. KEDA, a GitOps controller with
// drift correction, and a cron job running `kubectl scale` own spec.replicas
// just as hard, and none of them is discoverable through a stable API. An HPA
// is the common case and the one that can be detected, which is the whole of
// the claim being made here.
//
// It is off unless the install asked for it, because it costs a permission the
// operator does not otherwise need. With detection off, the behaviour is what
// it was: Reactor writes, and the fight is whatever the workload's owner
// notices.
func (r *AutomationReconciler) foreignManagerOf(
	ctx context.Context,
	handler targetHandler,
	key targetKey,
) (string, error) {
	if !r.DetectHPA || !handler.contested {
		return "", nil
	}

	// Unstructured, so this read is uncached like every other target read: a
	// typed List would start an informer and hold every HorizontalPodAutoscaler
	// in the cluster in memory to answer a question asked about one namespace.
	// It also means "list" is the entire grant — no get, no watch.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(autoscalingv2.SchemeGroupVersion.WithKind(kindHorizontalPodAutoscaler + "List"))
	if err := r.List(ctx, list, client.InNamespace(key.Namespace)); err != nil {
		// Failing closed on purpose. Detection is on because this install said
		// it cares about being outfought, so writing blind is the one answer
		// that is definitely wrong — and the message names the fix, because
		// nobody reading a status can see the operator's RBAC.
		return "", fmt.Errorf(
			"listing HorizontalPodAutoscalers in %s, which detection needs before it may claim a target "+
				"(install with safety.detectHPA=true to grant it, or turn detection off): %w",
			key.Namespace, err)
	}

	for i := range list.Items {
		if drives(&list.Items[i], handler.gvk, key.Name) {
			return kindHorizontalPodAutoscaler + "/" + key.Namespace + "/" + list.Items[i].GetName(), nil
		}
	}
	return "", nil
}

// drives reports whether an HPA scales this exact object.
//
// The group is compared and the version is not. scaleTargetRef carries a whole
// apiVersion, an HPA written years ago against apps/v1beta2 still drives the
// same Deployment today, and a version mismatch read as "not managed" would put
// Reactor straight back into the fight this exists to avoid. A ref in the core
// group carries no slash, which is the empty group and not a malformed one.
func drives(hpa *unstructured.Unstructured, gvk schema.GroupVersionKind, name string) bool {
	ref, found, err := unstructured.NestedStringMap(hpa.Object, fieldSpec, fieldScaleTargetRef)
	if err != nil || !found {
		return false
	}
	group, _, qualified := strings.Cut(ref["apiVersion"], "/")
	if !qualified {
		group = ""
	}
	return group == gvk.Group && ref["kind"] == gvk.Kind && ref["name"] == name
}
