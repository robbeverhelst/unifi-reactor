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

const (
	testNamespace = "default"
	wanPrimary    = "primary"
	wanBackup     = "backup"
	keyWAN        = "wan"
	keyUPS        = "ups"
	upsOnline     = "online"
	upsOnBattery  = "on-battery"
)

var _ = Describe("Automation Controller", func() {
	ctx := context.Background()

	var (
		store      *engine.StateStore
		reconciler *AutomationReconciler
	)

	BeforeEach(func() {
		store = engine.NewStateStore()
		reconciler = &AutomationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Store:  store,
		}
	})

	scaleTo := func(target string, replicas int32) reactorv1alpha1.Action {
		return reactorv1alpha1.Action{
			Type:     actionKubernetesScale,
			Target:   &reactorv1alpha1.TargetRef{Kind: "Deployment", Name: target},
			Replicas: &replicas,
		}
	}

	createDeployment := func(name string, replicas int32) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: name, Image: "example/" + name},
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, deployment) })
	}

	createAutomation := func(name string, spec reactorv1alpha1.AutomationSpec) {
		automation := &reactorv1alpha1.Automation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, automation)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, automation) })
	}

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

	replicasOf := func(name string) int32 {
		var deployment appsv1.Deployment
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
		return *deployment.Spec.Replicas
	}

	annotationsOf := func(name string) map[string]string {
		var deployment appsv1.Deployment
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
		return deployment.Annotations
	}

	scaleBy := func(name string, replicas int32) {
		var deployment appsv1.Deployment
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
		deployment.Spec.Replicas = &replicas
		Expect(k8sClient.Update(ctx, &deployment)).To(Succeed())
	}

	statusOf := func(name string) reactorv1alpha1.AutomationStatus {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		return automation.Status
	}

	conditionOf := func(status reactorv1alpha1.AutomationStatus, conditionType string) *metav1.Condition {
		for i := range status.Conditions {
			if status.Conditions[i].Type == conditionType {
				return &status.Conditions[i]
			}
		}
		return nil
	}

	Context("with one automation owning a target", func() {
		It("scales on entering the state and restores the declared value on leaving", func() {
			const target = "single-declared"
			createDeployment(target, 1)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(target, 1)},
			})

			By("reporting pending while no provider state is observed")
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(1)))
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationBaselineReplicas))

			By("claiming the target on entering the matching state")
			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationBaselineReplicas, "1"))
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationClaimedBy, testNamespace+"/"+target))

			By("treating repeated identical observations as no-ops")
			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("releasing the target on leaving the matching state")
			observe(map[string]string{keyWAN: wanPrimary})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(1)))
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationBaselineReplicas))
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationClaimedBy))

			By("recording status")
			status := statusOf(target)
			Expect(status.Matching).To(BeFalse())
			Expect(status.ObservedState).To(HaveKeyWithValue(keyWAN, wanPrimary))
			Expect(status.LastExecution).NotTo(BeNil())
			Expect(status.LastExecution.OnExit).To(BeTrue())
			Expect(conditionOf(status, conditionApplied).Status).To(Equal(metav1.ConditionTrue))
		})

		It("holds its claim when the provider stops reporting the key", func() {
			const target = "single-hold"
			createDeployment(target, 1)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(target, 1)},
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("not releasing merely because the key went missing")
			observe(map[string]string{keyUPS: upsOnline})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(conditionOf(statusOf(target), conditionReady).Reason).To(Equal("StateKeyUnavailable"))

			By("resuming normally once the key is reported again")
			observe(map[string]string{keyWAN: wanPrimary})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(1)))
		})

		It("restores the pre-claim baseline when onExit is omitted", func() {
			const target = "single-baseline"
			createDeployment(target, 3)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationBaselineReplicas, "3"))

			By("never re-recording the baseline while claimed")
			reconcileOnce(target)
			reconcileOnce(target)
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationBaselineReplicas, "3"),
				"re-recording would capture the value Reactor itself set and strand the workload at 0")

			observe(map[string]string{keyWAN: wanPrimary})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(3)))
		})

		It("leaves the target alone under reversal None, and stops asserting once released", func() {
			const target = "single-none"
			createDeployment(target, 2)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions:  []reactorv1alpha1.Action{scaleTo(target, 0)},
				Reversal: reactorv1alpha1.ReversalNone,
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("leaving it where it was on release")
			observe(map[string]string{keyWAN: wanPrimary})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationBaselineReplicas))

			By("not re-asserting a value after release")
			scaleBy(target, 5)
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(5)))
		})
	})

	Context("with two automations sharing a target", func() {
		const (
			target = "shared"
			wanned = "pause-on-backup-wan"
			upsed  = "shed-on-battery"
		)

		BeforeEach(func() {
			createDeployment(target, 2)
			createAutomation(wanned, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(target, 2)},
			})
			createAutomation(upsed, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(target, 2)},
			})
		})

		// The bug this whole model exists to fix: one automation's reversal
		// used to scale the workload back up while the other still wanted it
		// down, and which one won depended on reconcile ordering.
		It("keeps the target down while either condition still holds", func() {
			By("both conditions holding")
			observe(map[string]string{keyWAN: wanBackup, keyUPS: upsOnBattery})
			reconcileOnce(wanned)
			reconcileOnce(upsed)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationClaimedBy,
				testNamespace+"/"+wanned+","+testNamespace+"/"+upsed))

			By("the WAN recovering while the power is still out")
			observe(map[string]string{keyWAN: wanPrimary, keyUPS: upsOnBattery})
			reconcileOnce(wanned)
			Expect(replicasOf(target)).To(Equal(int32(0)),
				"the UPS automation still claims this target")

			By("explaining in status why the reversal did not take effect")
			status := statusOf(wanned)
			Expect(status.Targets).To(HaveLen(1))
			Expect(*status.Targets[0].Desired).To(Equal(int32(2)))
			Expect(*status.Targets[0].Effective).To(Equal(int32(0)))
			Expect(status.Targets[0].DeferredBy).To(Equal([]string{testNamespace + "/" + upsed}))
			applied := conditionOf(status, conditionApplied)
			Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			Expect(applied.Reason).To(Equal("DeferredToOtherAutomation"))

			By("staying down no matter which automation reconciles")
			reconcileOnce(upsed)
			reconcileOnce(wanned)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("releasing only once neither condition holds")
			observe(map[string]string{keyWAN: wanPrimary, keyUPS: upsOnline})
			reconcileOnce(upsed)
			Expect(replicasOf(target)).To(Equal(int32(2)))
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationBaselineReplicas))
		})

		It("resolves to the most restrictive claim when the two disagree", func() {
			var automation reactorv1alpha1.Automation
			key := types.NamespacedName{Name: wanned, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
			one := int32(1)
			automation.Spec.Actions[0].Replicas = &one
			Expect(k8sClient.Update(ctx, &automation)).To(Succeed())

			observe(map[string]string{keyWAN: wanBackup, keyUPS: upsOnBattery})
			reconcileOnce(wanned)
			Expect(replicasOf(target)).To(Equal(int32(0)), "min of the competing claims wins")

			status := statusOf(wanned)
			Expect(*status.Targets[0].Desired).To(Equal(int32(1)))
			Expect(*status.Targets[0].Effective).To(Equal(int32(0)))
			Expect(status.Targets[0].DeferredBy).To(Equal([]string{testNamespace + "/" + upsed}))
		})

		It("claims the target when only one condition holds", func() {
			observe(map[string]string{keyWAN: wanBackup, keyUPS: upsOnline})
			reconcileOnce(upsed)
			Expect(replicasOf(target)).To(Equal(int32(0)),
				"the WAN automation's claim counts even when the UPS one is doing the reconciling")
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationClaimedBy, testNamespace+"/"+wanned))
		})
	})
})
