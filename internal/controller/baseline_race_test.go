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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/robbeverhelst/unifi-reactor/internal/engine"
)

// The baseline records what a workload was before Reactor first touched it, and
// it is the only thing that knows how to put it back. Recording it wrong is
// therefore not a cosmetic bug: the outage ends and the workload comes back at
// the level Reactor itself set.
//
// Two Automations sharing one target reconcile concurrently — maxConcurrentReconciles
// is 4 — and each takes its own snapshot of the target before deciding anything.
// The one that gets there second still sees "no baseline recorded" in ITS
// snapshot, while the level it reads to record comes from a live read of the
// scale subresource that the first one has already changed. So the second write
// records the level the first one just applied.
//
// This is what CI caught on the arbitration spec: an Automation that had stopped
// matching wanted its target back at 1 — the value the other claim had set —
// rather than at the 3 it started from.
var _ = Describe("Recording a target's baseline", func() {
	const target = "baseline-race"
	ctx := context.Background()

	deploymentAt := func(replicas int32) *unstructured.Unstructured {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: target, Namespace: testNamespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: target}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelApp: target}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: target, Image: "example/" + target},
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, deployment) })

		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: target}, fetched)).To(Succeed())
		return fetched
	}

	// snapshot is what a second, concurrent reconcile is holding: the target as
	// it was before the first claim wrote anything to it.
	It("does not re-record it from a level Reactor itself has already applied", func() {
		handler, err := handlerFor(kindDeployment)
		Expect(err).NotTo(HaveOccurred())

		stale := deploymentAt(3)

		// The first claim, exactly as a reconcile would make it: baseline 3,
		// then the level it asked for.
		fresh := stale.DeepCopy()
		_, err = claimTarget(ctx, k8sClient, handler, fresh,
			engine.Resolution{Level: 1}, []engine.Intent{{Claimant: "apps/first", Level: 1}})
		Expect(err).NotTo(HaveOccurred())

		var claimed appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: target}, &claimed)).To(Succeed())
		Expect(claimed.Annotations).To(HaveKeyWithValue(annotationBaselineReplicas, "3"))
		Expect(claimed.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))

		// The second reconcile, still holding the snapshot it took before any
		// of that happened. It must not conclude that the baseline is unrecorded
		// and write the 1 it can now read off the live scale.
		_, err = claimTarget(ctx, k8sClient, handler, stale,
			engine.Resolution{Level: 1}, []engine.Intent{
				{Claimant: "apps/first", Level: 1},
				{Claimant: "apps/second", Level: 1},
			})
		if err != nil {
			// A conflict is a correct outcome: the write is refused, the
			// reconcile retries, and the retry sees the recorded baseline.
			Expect(err.Error()).To(ContainSubstring("conflict"),
				"the only acceptable failure here is the optimistic-lock conflict that prevents the clobber")
		}

		var after appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: target}, &after)).To(Succeed())
		Expect(after.Annotations).To(HaveKeyWithValue(annotationBaselineReplicas, "3"),
			"the baseline was overwritten with the level Reactor itself applied; "+
				"the workload would be restored to that instead of to what it was")
	})
})
