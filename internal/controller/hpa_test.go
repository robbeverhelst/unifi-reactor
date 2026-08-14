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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

// A claimant arbitration cannot reach.
//
// Reactor writes spec.replicas; an HPA computes one from metrics and writes it
// back. The fold cannot resolve this because an HPA is not an Automation, so
// the only choices are to fight and oscillate, or to decline and say so.
var _ = Describe("A target a HorizontalPodAutoscaler already drives", func() {
	ctx := context.Background()

	const (
		hpaTarget = "autoscaled"
		hpaName   = "autoscaled-hpa"
		automated = "shed-the-autoscaled"
		baseline  = 3
	)

	var (
		store      *engine.StateStore
		reconciler *AutomationReconciler
	)

	BeforeEach(func() {
		store = engine.NewStateStore()
		reconciler = &AutomationReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Store:     store,
			DetectHPA: true,
		}
	})

	observe := func(state map[string]string) {
		store.Observe(events.Observation{
			Provider: providerUniFi, State: state, ObservedAt: time.Now(),
		})
	}

	reconcileOnce := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	deploymentOf := func(name string) appsv1.Deployment {
		var deployment appsv1.Deployment
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
		return deployment
	}

	automationOf := func(name string) reactorv1alpha1.Automation {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		return automation
	}

	conditionOf := func(automation reactorv1alpha1.Automation, kind string) *metav1.Condition {
		for i := range automation.Status.Conditions {
			if automation.Status.Conditions[i].Type == kind {
				return &automation.Status.Conditions[i]
			}
		}
		return nil
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

	// createHPA points a real HorizontalPodAutoscaler at an object. kind and
	// name are parameters because the interesting failures are the near misses:
	// an HPA driving something else in the same namespace must not count.
	createHPA := func(name, kind, target string) *autoscalingv2.HorizontalPodAutoscaler {
		minReplicas := int32(1)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1", Kind: kind, Name: target,
				},
				MinReplicas: &minReplicas,
				MaxReplicas: 5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, hpa) })
		return hpa
	}

	createAutomation := func(name, target string) {
		zero := int32(0)
		automation := &reactorv1alpha1.Automation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{{
					Type:     actionKubernetesScale,
					Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: target},
					Replicas: &zero,
				}},
			},
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

	Context("when Reactor has never claimed it", func() {
		BeforeEach(func() {
			createDeployment(hpaTarget, baseline)
			createHPA(hpaName, kindDeployment, hpaTarget)
			createAutomation(automated, hpaTarget)
			observe(map[string]string{keyUPS: upsOnBattery})
		})

		It("writes nothing at all, not even the annotations", func() {
			reconcileOnce(automated)

			deployment := deploymentOf(hpaTarget)
			Expect(*deployment.Spec.Replicas).To(BeEquivalentTo(baseline),
				"Reactor started a fight with the HPA it cannot win")
			Expect(deployment.Annotations).NotTo(HaveKey(annotationBaselineReplicas),
				"a baseline captured here would record a value the HPA is actively changing")
			Expect(deployment.Annotations).NotTo(HaveKey(annotationClaimedBy))
		})

		It("names the HPA responsible, and stays Ready while it does", func() {
			reconcileOnce(automated)

			automation := automationOf(automated)
			Expect(automation.Status.Targets).To(HaveLen(1))
			Expect(automation.Status.Targets[0].ManagedBy).
				To(Equal("HorizontalPodAutoscaler/" + testNamespace + "/" + hpaName))

			applied := conditionOf(automation, conditionApplied)
			Expect(applied).NotTo(BeNil())
			Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			Expect(applied.Reason).To(Equal(reasonTargetManagedByHPA))
			Expect(conditionOf(automation, conditionReady)).To(HaveField("Status", metav1.ConditionTrue),
				"the automation is correctly configured; it just cannot act on this target")
		})

		It("claims the target the moment the HPA is gone", func() {
			reconcileOnce(automated)
			Expect(k8sClient.Delete(ctx, &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: hpaName, Namespace: testNamespace},
			})).To(Succeed())

			reconcileOnce(automated)
			Expect(*deploymentOf(hpaTarget).Spec.Replicas).To(BeEquivalentTo(0))
			Expect(deploymentOf(hpaTarget).Annotations).
				To(HaveKeyWithValue(annotationBaselineReplicas, "3"))
			Expect(automationOf(automated).Status.Targets[0].ManagedBy).To(BeEmpty())
		})
	})

	Context("when an HPA appears over a target Reactor is already holding", func() {
		BeforeEach(func() {
			createDeployment(hpaTarget, baseline)
			createAutomation(automated, hpaTarget)
			observe(map[string]string{keyUPS: upsOnBattery})
		})

		It("hands the workload back to its baseline and lets go", func() {
			By("claiming it while nothing else drives it")
			reconcileOnce(automated)
			Expect(*deploymentOf(hpaTarget).Spec.Replicas).To(BeEquivalentTo(0))

			By("adding the autoscaler afterwards, as somebody would")
			createHPA(hpaName, kindDeployment, hpaTarget)
			reconcileOnce(automated)

			deployment := deploymentOf(hpaTarget)
			Expect(*deployment.Spec.Replicas).To(BeEquivalentTo(baseline),
				"left at 0 the HPA cannot recover it: an HPA does not scale a workload up from zero")
			Expect(deployment.Annotations).NotTo(HaveKey(annotationBaselineReplicas))
			Expect(deployment.Annotations).NotTo(HaveKey(annotationClaimedBy))
		})

		It("stays let go, rather than reclaiming on the next pass", func() {
			reconcileOnce(automated)
			createHPA(hpaName, kindDeployment, hpaTarget)
			reconcileOnce(automated)
			reconcileOnce(automated)

			Expect(*deploymentOf(hpaTarget).Spec.Replicas).To(BeEquivalentTo(baseline))
			Expect(automationOf(automated).Status.Targets[0].ManagedBy).NotTo(BeEmpty())
		})
	})

	Context("when the HPA drives something else", func() {
		BeforeEach(func() {
			createDeployment(hpaTarget, baseline)
			createAutomation(automated, hpaTarget)
			observe(map[string]string{keyUPS: upsOnBattery})
		})

		It("acts normally on a namespace-mate of an autoscaled workload", func() {
			createDeployment("some-other-workload", 1)
			createHPA(hpaName, kindDeployment, "some-other-workload")

			reconcileOnce(automated)
			Expect(*deploymentOf(hpaTarget).Spec.Replicas).To(BeEquivalentTo(0))
			Expect(automationOf(automated).Status.Targets[0].ManagedBy).To(BeEmpty())
		})

		It("acts normally when the kind differs, even at the same name", func() {
			createHPA(hpaName, kindStatefulSet, hpaTarget)

			reconcileOnce(automated)
			Expect(*deploymentOf(hpaTarget).Spec.Replicas).To(BeEquivalentTo(0))
		})
	})

	Context("previewed rather than claimed", func() {
		BeforeEach(func() {
			createDeployment(hpaTarget, baseline)
			createHPA(hpaName, kindDeployment, hpaTarget)
			createAutomation(automated, hpaTarget)
			observe(map[string]string{keyUPS: upsOnBattery})

			var automation reactorv1alpha1.Automation
			key := types.NamespacedName{Name: automated, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
			automation.Spec.DryRun = true
			Expect(k8sClient.Update(ctx, &automation)).To(Succeed())
		})

		It("previews no level, because declining is what it would do", func() {
			reconcileOnce(automated)

			entry := automationOf(automated).Status.Targets[0]
			Expect(entry.ManagedBy).NotTo(BeEmpty())
			Expect(entry.Preview).To(BeNil(),
				"a preview of a claim this automation would never make is worse than no preview")
		})
	})

	Context("with detection off", func() {
		BeforeEach(func() {
			reconciler.DetectHPA = false
			createDeployment(hpaTarget, baseline)
			createHPA(hpaName, kindDeployment, hpaTarget)
			createAutomation(automated, hpaTarget)
			observe(map[string]string{keyUPS: upsOnBattery})
		})

		It("behaves exactly as it did before, which is to write and be overwritten", func() {
			reconcileOnce(automated)

			Expect(*deploymentOf(hpaTarget).Spec.Replicas).To(BeEquivalentTo(0))
			Expect(automationOf(automated).Status.Targets[0].ManagedBy).To(BeEmpty())
			Expect(conditionOf(automationOf(automated), conditionApplied)).
				To(HaveField("Status", metav1.ConditionTrue))
		})
	})
})
