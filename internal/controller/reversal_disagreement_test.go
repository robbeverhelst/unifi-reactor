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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

// Two automations sharing a workload and wanting it down for different reasons
// are both right, and the fold between them is the design working. Two
// automations declaring different levels for it once NOTHING claims it cannot
// both be right: a workload has one normal size, and these say it is 1 and 3.
//
// Reactor still takes min, which is defensible, documented and independent of
// reconcile order. What it must not do is take it silently — and it must say so
// while the disagreement exists rather than at the moment of release, because
// finding out at release is finding out after the workload has already come
// back at the wrong number.
var _ = Describe("Two automations disagreeing about a target's normal size", func() {
	ctx := context.Background()

	const (
		disputed = "disputed"
		baseline = 3
		shedA    = "shed-a"
		shedB    = "shed-b"
	)

	var (
		store      *engine.StateStore
		recorder   *fakeRecorder
		reconciler *AutomationReconciler
	)

	BeforeEach(func() {
		store = engine.NewStateStore()
		recorder = &fakeRecorder{}
		reconciler = &AutomationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Store:    store,
			Recorder: recorder,
		}
	})

	observe := func(state map[string]string) {
		store.Observe(events.Observation{
			Provider: providerUniFi, State: state, ObservedAt: time.Now(),
		})
	}

	createDeployment := func(name string, replicas int32) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelApp: name}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: name, Image: "example/" + name},
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, deployment) })
	}

	// shedder is one automation on the shared target: down to zero while the
	// WAN is on backup, and back to onExit — or to whatever reversal says —
	// once it is not.
	shedder := func(name string, onExit *int32, reversal reactorv1alpha1.ReversalPolicy) {
		zero := int32(0)
		spec := reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Actions: []reactorv1alpha1.Action{{
				Type:     actionKubernetesScale,
				Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: disputed},
				Replicas: &zero,
			}},
			Reversal: reversal,
		}
		if onExit != nil {
			spec.OnExit = []reactorv1alpha1.Action{{
				Type:     actionKubernetesScale,
				Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: disputed},
				Replicas: onExit,
			}}
		}
		automation := &reactorv1alpha1.Automation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, automation)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, automation)
			var lingering reactorv1alpha1.Automation
			key := types.NamespacedName{Name: name, Namespace: testNamespace}
			if err := k8sClient.Get(ctx, key, &lingering); err == nil {
				lingering.Finalizers = nil
				_ = k8sClient.Update(ctx, &lingering)
			}
		})
	}

	reconcileBoth := func() {
		for _, name := range []string{shedA, shedB} {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	disagreementOn := func(name string) []reactorv1alpha1.ReversalIntent {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		Expect(automation.Status.Targets).To(HaveLen(1))
		return automation.Status.Targets[0].ReversalDisagreement
	}

	appliedOn := func(name string) *metav1.Condition {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		for i := range automation.Status.Conditions {
			if automation.Status.Conditions[i].Type == conditionApplied {
				return &automation.Status.Conditions[i]
			}
		}
		return nil
	}

	replicasOf := func(name string) int32 {
		var deployment appsv1.Deployment
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
		return *deployment.Spec.Replicas
	}

	declared := func(replicas int32) *int32 { return &replicas }

	It("names both claimants and both levels while the workload is still down", func() {
		createDeployment(disputed, baseline)
		shedder(shedA, declared(1), "")
		shedder(shedB, declared(3), "")

		observe(map[string]string{keyWAN: wanBackup})
		reconcileBoth()

		Expect(replicasOf(disputed)).To(BeEquivalentTo(0), "the live arbitration is unaffected")

		// The acceptance criterion: reported now, with the outage still running
		// and every reversal still hypothetical.
		for _, name := range []string{shedA, shedB} {
			Expect(disagreementOn(name)).To(Equal([]reactorv1alpha1.ReversalIntent{
				{Claimant: testNamespace + "/" + shedA, Desired: 1, Level: "1 replicas"},
				{Claimant: testNamespace + "/" + shedB, Desired: 3, Level: "3 replicas"},
			}), "%s did not report the contradiction between its own spec and its peer's", name)
		}

		By("as a Warning, because nothing Reactor does resolves it")
		event, found := recorder.find(reasonReversalDisagreement)
		Expect(found).To(BeTrue(), "the disagreement was resolved silently")
		Expect(event.Type).To(Equal(corev1.EventTypeWarning))
		Expect(event.Note).To(ContainSubstring("1 replicas"))
		Expect(event.Note).To(ContainSubstring("3 replicas"))
		Expect(event.Note).To(ContainSubstring(testNamespace + "/" + shedB))

		By("and without making an automation whose claim is in effect look unhealthy")
		Expect(appliedOn(shedA)).To(HaveField("Status", metav1.ConditionTrue))
	})

	It("still resolves with min, and says so at the release too", func() {
		createDeployment(disputed, baseline)
		shedder(shedA, declared(1), "")
		shedder(shedB, declared(3), "")

		observe(map[string]string{keyWAN: wanBackup})
		reconcileBoth()
		observe(map[string]string{keyWAN: wanPrimary})
		reconcileBoth()

		Expect(replicasOf(disputed)).To(BeEquivalentTo(1),
			"the resolved value changed; reporting a disagreement must not resolve it")
		Expect(disagreementOn(shedA)).To(HaveLen(2))
	})

	It("says nothing when they agree", func() {
		createDeployment(disputed, baseline)
		shedder(shedA, declared(3), "")
		shedder(shedB, declared(3), "")

		observe(map[string]string{keyWAN: wanBackup})
		reconcileBoth()

		Expect(disagreementOn(shedA)).To(BeEmpty())
		Expect(recorder.reasons()).NotTo(ContainElement(reasonReversalDisagreement))
	})

	It("says nothing when one of them declines to have an opinion", func() {
		createDeployment(disputed, baseline)
		shedder(shedA, declared(1), "")
		// Reversal None contributes no level at all, so there is nothing for the
		// other one to contradict.
		shedder(shedB, nil, reactorv1alpha1.ReversalNone)

		observe(map[string]string{keyWAN: wanBackup})
		reconcileBoth()

		Expect(disagreementOn(shedA)).To(BeEmpty())
		Expect(recorder.reasons()).NotTo(ContainElement(reasonReversalDisagreement))
	})

	It("says nothing when both defer to the baseline, which they cannot disagree about", func() {
		createDeployment(disputed, baseline)
		shedder(shedA, nil, "")
		shedder(shedB, nil, "")

		observe(map[string]string{keyWAN: wanBackup})
		reconcileBoth()

		Expect(disagreementOn(shedA)).To(BeEmpty(),
			"two automations resolving to the same recorded baseline are not in disagreement")
	})

	It("says it once, not every fifteen seconds for as long as it stands", func() {
		createDeployment(disputed, baseline)
		shedder(shedA, declared(1), "")
		shedder(shedB, declared(3), "")

		observe(map[string]string{keyWAN: wanBackup})
		reconcileBoth()
		reconcileBoth()
		reconcileBoth()

		raised := 0
		for _, reason := range recorder.reasons() {
			if reason == reasonReversalDisagreement {
				raised++
			}
		}
		Expect(raised).To(Equal(2),
			"an unchanged contradiction should be announced once per automation, not once per reconcile")
	})
})
