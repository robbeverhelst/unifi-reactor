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

// Two ways of not acting, and they are not the same one.
//
// spec.dryRun takes ONE automation out of force so it can be applied beside
// policies that are live without perturbing them, and reports the arbitration
// it is staying out of. The --dry-run install flag leaves every automation in
// force, resolves everything exactly as it otherwise would, and declines to
// write — which is the report a first rollout into a cluster needs.
var _ = Describe("Not acting, and saying what would have happened", func() {
	ctx := context.Background()

	const (
		dryTarget = "preview-target"
		baseline  = 3
	)

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

	targetOf := func(automation reactorv1alpha1.Automation) reactorv1alpha1.TargetStatus {
		ExpectWithOffset(1, automation.Status.Targets).To(HaveLen(1))
		return automation.Status.Targets[0]
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

	createAutomation := func(name string, spec reactorv1alpha1.AutomationSpec) {
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

	shedTo := func(target string, replicas int32) reactorv1alpha1.Action {
		return reactorv1alpha1.Action{
			Type:     actionKubernetesScale,
			Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: target},
			Replicas: &replicas,
		}
	}

	onBattery := func(target string, replicas int32, dryRun bool) reactorv1alpha1.AutomationSpec {
		return reactorv1alpha1.AutomationSpec{
			When:    &reactorv1alpha1.StateTrigger{Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery}},
			Actions: []reactorv1alpha1.Action{shedTo(target, replicas)},
			DryRun:  dryRun,
		}
	}

	Context("an automation running as a dry run", func() {
		const name = "preview-shed"

		BeforeEach(func() {
			createDeployment(dryTarget, baseline)
			createAutomation(name, onBattery(dryTarget, 0, true))
		})

		It("writes nothing to the target, and no claim annotation either", func() {
			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(name)

			deployment := deploymentOf(dryTarget)
			Expect(*deployment.Spec.Replicas).To(BeEquivalentTo(baseline),
				"a dry run scaled the workload it was only supposed to describe")
			Expect(deployment.Annotations).NotTo(HaveKey(annotationBaselineReplicas),
				"a dry run recorded a baseline, which would claim a target it never took")
			Expect(deployment.Annotations).NotTo(HaveKey(annotationClaimedBy))
		})

		It("reports what it would hold the target at, and what it would hand back", func() {
			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(name)

			target := targetOf(automationOf(name))
			Expect(target.Preview).NotTo(BeNil())
			Expect(target.Preview.Desired).To(HaveValue(BeEquivalentTo(0)))
			Expect(target.Preview.Effective).To(HaveValue(BeEquivalentTo(0)))
			Expect(target.Preview.Level).To(Equal("0 replicas"))
			Expect(target.Preview.DeferredBy).To(BeEmpty())
			Expect(target.Preview.WouldDefer).To(BeEmpty())
			Expect(target.Preview.OnExit).To(Equal("3 replicas"),
				"with no baseline recorded yet, a reversal to Baseline restores what the target is at now")
			Expect(target.Effective).To(BeNil(), "nothing claims the target, so nothing is holding it anywhere")
		})

		It("answers what would happen even while the condition does not hold", func() {
			// The case the feature exists for: an automation for a power cut is
			// written on an afternoon when the power is fine.
			observe(map[string]string{keyUPS: upsOnline})
			reconcileOnce(name)

			automation := automationOf(name)
			Expect(automation.Status.Matching).To(BeFalse())
			Expect(targetOf(automation).Preview.Effective).To(HaveValue(BeEquivalentTo(0)))
		})

		It("is Ready and not Applied, and says which of the two ways of not acting this is", func() {
			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(name)

			automation := automationOf(name)
			Expect(conditionOf(automation, conditionReady)).To(HaveField("Status", metav1.ConditionTrue))
			applied := conditionOf(automation, conditionApplied)
			Expect(applied).NotTo(BeNil())
			Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			Expect(applied.Reason).To(Equal(reasonDryRun))
		})

		It("keeps its release finalizer, so turning the dry run off changes nothing else", func() {
			// Unlike an install-wide dry run, this operator can write: the
			// finalizer is what would hand a target back the moment this
			// automation stops previewing and starts claiming, and adding it
			// only then would leave a window where it holds a claim it would
			// not release. It has nothing to do meanwhile, which costs nothing.
			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(name)

			Expect(automationOf(name).Finalizers).To(ContainElement(finalizerReleaseClaims))
		})
	})

	Context("a dry run beside an automation that is live", func() {
		const (
			live    = "live-shed"
			preview = "preview-beside-live"
		)

		BeforeEach(func() {
			createDeployment(dryTarget, baseline)
			createAutomation(live, onBattery(dryTarget, 1, false))
			createAutomation(preview, onBattery(dryTarget, 0, true))
			observe(map[string]string{keyUPS: upsOnBattery})
		})

		It("cannot change what the live automation's target resolves to", func() {
			reconcileOnce(preview)
			reconcileOnce(live)

			Expect(*deploymentOf(dryTarget).Spec.Replicas).To(BeEquivalentTo(1),
				"the dry run's more restrictive claim was folded in, which it must never be")
			Expect(targetOf(automationOf(live)).DeferredBy).To(BeEmpty())
		})

		It("names the peer it would outvote, and the level that peer would lose", func() {
			reconcileOnce(live)
			reconcileOnce(preview)

			target := targetOf(automationOf(preview))
			Expect(target.Effective).To(HaveValue(BeEquivalentTo(1)),
				"what the target is actually being held at, by somebody else")
			Expect(target.Preview.Effective).To(HaveValue(BeEquivalentTo(0)),
				"and what it would be held at if this one counted")
			Expect(target.Preview.WouldDefer).To(ConsistOf(testNamespace + "/" + live))
			Expect(target.Preview.DeferredBy).To(BeEmpty())
		})

		It("names the peer that would outvote it when it is the less restrictive one", func() {
			// Same pair, the other way round: the dry run wants more than the
			// live automation is willing to allow.
			createAutomation("preview-outvoted", onBattery(dryTarget, 2, true))
			reconcileOnce(live)
			reconcileOnce("preview-outvoted")

			target := targetOf(automationOf("preview-outvoted"))
			Expect(target.Preview.Desired).To(HaveValue(BeEquivalentTo(2)))
			Expect(target.Preview.Effective).To(HaveValue(BeEquivalentTo(1)))
			Expect(target.Preview.DeferredBy).To(ConsistOf(testNamespace + "/" + live))
			Expect(target.Preview.WouldDefer).To(BeEmpty())
		})
	})

	Context("an install running as a dry run", func() {
		const name = "install-dry-run"

		BeforeEach(func() {
			reconciler.DryRun = true
			createDeployment(dryTarget, baseline)
			createAutomation(name, onBattery(dryTarget, 0, false))
			observe(map[string]string{keyUPS: upsOnBattery})
		})

		It("resolves the arbitration and stops one line short of writing it", func() {
			reconcileOnce(name)

			deployment := deploymentOf(dryTarget)
			Expect(*deployment.Spec.Replicas).To(BeEquivalentTo(baseline))
			Expect(deployment.Annotations).NotTo(HaveKey(annotationBaselineReplicas))

			automation := automationOf(name)
			target := targetOf(automation)
			Expect(target.Effective).To(HaveValue(BeEquivalentTo(0)),
				"the fold is the real one, so this is the value the target would have")
			Expect(target.Level).To(Equal("0 replicas"))
			Expect(target.Preview).To(BeNil(),
				"the automation is in force, so the outcome above is not a counterfactual")

			applied := conditionOf(automation, conditionApplied)
			Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			Expect(applied.Reason).To(Equal(reasonDryRun))
		})

		It("takes the release finalizer back off, so nothing is left holding a deletion", func() {
			reconcileOnce(name)
			Expect(automationOf(name).Finalizers).NotTo(ContainElement(finalizerReleaseClaims))
		})

		It("puts it back on the moment the install starts writing again", func() {
			reconcileOnce(name)
			reconciler.DryRun = false
			reconcileOnce(name)

			Expect(automationOf(name).Finalizers).To(ContainElement(finalizerReleaseClaims))
			Expect(*deploymentOf(dryTarget).Spec.Replicas).To(BeEquivalentTo(0))
		})
	})
})
