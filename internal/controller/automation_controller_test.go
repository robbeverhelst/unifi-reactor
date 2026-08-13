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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// stallingClient stands in for a target that has stopped answering: reads of
// the Automation itself still work, so the reconcile gets far enough to block
// on the target the way a wedged API call would.
type stallingClient struct {
	client.Client
}

func (s stallingClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, isTarget := obj.(*appsv1.Deployment); !isTarget {
		return s.Client.Get(ctx, key, obj, opts...)
	}
	<-ctx.Done()
	return ctx.Err()
}

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

	withTimeout := func(action reactorv1alpha1.Action, seconds int32) reactorv1alpha1.Action {
		action.TimeoutSeconds = &seconds
		return action
	}

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
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, automation)
			// The release finalizer would otherwise leave the resource
			// terminating for the rest of the suite, where it still shows up
			// in the List that gathers claimants.
			var lingering reactorv1alpha1.Automation
			key := types.NamespacedName{Name: name, Namespace: testNamespace}
			if err := k8sClient.Get(ctx, key, &lingering); err == nil {
				lingering.Finalizers = nil
				_ = k8sClient.Update(ctx, &lingering)
			}
		})
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

	Context("when an automation is suspended", func() {
		suspend := func(name string, suspended bool) {
			var automation reactorv1alpha1.Automation
			key := types.NamespacedName{Name: name, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
			automation.Spec.Suspend = suspended
			Expect(k8sClient.Update(ctx, &automation)).To(Succeed())
		}

		// Suspending is a reversible delete, not a freeze: it takes the policy
		// out of force, and a policy that is not in force cannot hold a
		// workload down. The alternative — keeping the claim but ignoring new
		// transitions — would leave the target pinned by an automation that has
		// stopped reacting, and would re-assert that value over any manual
		// scale, which is the opposite of an off switch.
		It("hands its targets back and stops asserting anything", func() {
			const target = "suspend-single"
			createDeployment(target, 2)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(target, 2)},
			})

			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("releasing the target while the condition still holds")
			suspend(target, true)
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(2)),
				"a suspended automation claims nothing, exactly as a deleted one does")
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationBaselineReplicas))
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationClaimedBy))

			By("still reporting what it observes, so it stays useful for debugging")
			status := statusOf(target)
			Expect(status.Matching).To(BeTrue())
			Expect(status.ObservedState).To(HaveKeyWithValue(keyUPS, upsOnBattery))
			Expect(conditionOf(status, conditionReady).Status).To(Equal(metav1.ConditionTrue))
			Expect(conditionOf(status, conditionReady).Reason).To(Equal(reasonSuspended))
			applied := conditionOf(status, conditionApplied)
			Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			Expect(applied.Reason).To(Equal(reasonSuspended))

			By("leaving the target alone afterwards, including a manual scale")
			scaleBy(target, 5)
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(5)))

			By("re-claiming from current state on resume, without replaying anything")
			suspend(target, false)
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationBaselineReplicas, "5"))
			Expect(conditionOf(statusOf(target), conditionApplied).Reason).To(Equal("InEffect"))
		})

		// Pausing is most useful in the middle of an incident, which is exactly
		// when the console may be unreachable. A claim that outlives the
		// observation would make the off switch fail when it is needed.
		It("releases even when no provider state can be observed", func() {
			const target = "suspend-blind"
			createDeployment(target, 3)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
			})

			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("suspending it after a restart that has observed nothing yet")
			suspend(target, true)
			reconciler.Store = engine.NewStateStore()
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(3)), "the recorded baseline is restored")
			Expect(statusOf(target).Matching).To(BeTrue(), "the last known matching is held, not guessed")
		})

		It("does not release a target another automation still claims", func() {
			const (
				target = "suspend-shared"
				paused = "suspend-paused"
				peer   = "suspend-peer"
			)
			createDeployment(target, 2)
			for _, name := range []string{paused, peer} {
				createAutomation(name, reactorv1alpha1.AutomationSpec{
					When: &reactorv1alpha1.StateTrigger{
						Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
					},
					Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
					OnExit:  []reactorv1alpha1.Action{scaleTo(target, 2)},
				})
			}

			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(paused)
			reconcileOnce(peer)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("suspending one of the two")
			suspend(paused, true)
			reconcileOnce(paused)
			Expect(replicasOf(target)).To(Equal(int32(0)),
				"the automation still in force keeps its claim")
			Expect(annotationsOf(target)).To(HaveKeyWithValue(annotationClaimedBy,
				testNamespace+"/"+peer), "a suspended automation is not listed as a claimant")

			By("deleting the suspended one, which is holding nothing")
			var automation reactorv1alpha1.Automation
			key := types.NamespacedName{Name: paused, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &automation)).To(Succeed())
			reconcileOnce(paused)
			Expect(k8sClient.Get(ctx, key, &automation)).To(MatchError(ContainSubstring("not found")),
				"the finalizer must not block deleting an automation that claims nothing")
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("releasing once the survivor's condition ends")
			observe(map[string]string{keyUPS: upsOnline})
			reconcileOnce(peer)
			Expect(replicasOf(target)).To(Equal(int32(2)))
		})
	})

	Context("when a target will not answer", func() {
		reconcileFor := func(name string) (reconcile.Result, error) {
			return reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
			})
		}

		It("times out a hanging target and surfaces it in status", func() {
			const target = "hanging"
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{withTimeout(scaleTo(target, 0), 1)},
			})
			reconciler.Client = stallingClient{Client: k8sClient}

			observe(map[string]string{keyWAN: wanBackup})
			started := time.Now()
			result, err := reconcileFor(target)

			Expect(err).NotTo(HaveOccurred(), "a timeout is a recorded failure, not a reconcile error")
			Expect(time.Since(started)).To(BeNumerically("<", 10*time.Second),
				"the action must be bounded by its own timeout")
			Expect(result.RequeueAfter).To(Equal(retryBackoff(1)))

			status := statusOf(target)
			Expect(status.LastExecution.Status).To(Equal("Failed"))
			Expect(status.LastExecution.Attempts).To(Equal(int32(1)))
			Expect(status.LastExecution.Reason).To(ContainSubstring("context deadline exceeded"))
			Expect(conditionOf(status, conditionReady).Reason).To(Equal("ActionFailed"))
		})

		It("backs off exponentially and then gives up with a reason", func() {
			const target = "missing-target"
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
			})

			observe(map[string]string{keyWAN: wanBackup})
			for attempt := int32(1); attempt < maxActionAttempts; attempt++ {
				result, err := reconcileFor(target)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(retryBackoff(attempt)),
					"attempt %d should back off exponentially", attempt)
				Expect(statusOf(target).LastExecution.Attempts).To(Equal(attempt))
			}

			By("giving up once the budget is spent")
			result, err := reconcileFor(target)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(reevaluateInterval),
				"a spent budget falls back to ordinary re-evaluation, not a growing backoff")

			status := statusOf(target)
			applied := conditionOf(status, conditionApplied)
			Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			Expect(applied.Reason).To(Equal("RetryBudgetExhausted"))
			Expect(status.LastExecution.Attempts).To(Equal(int32(maxActionAttempts)))
		})

		It("starts the budget again once the target recovers", func() {
			const target = "recovering"
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
			})

			observe(map[string]string{keyWAN: wanBackup})
			_, err := reconcileFor(target)
			Expect(err).NotTo(HaveOccurred())
			Expect(statusOf(target).LastExecution.Attempts).To(Equal(int32(1)))

			createDeployment(target, 2)
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))
			Expect(statusOf(target).LastExecution.Status).To(Equal("Success"))
			Expect(statusOf(target).LastExecution.Attempts).To(BeZero())
		})
	})

	Context("when Reactor stops watching", func() {
		// The realistic version of #39: the UPS is on battery, a workload is
		// scaled down, and the automation — or the whole operator — goes away.
		It("hands the target back when a matched automation is deleted", func() {
			const target = "deleted-while-matched"
			createDeployment(target, 2)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(target, 2)},
			})

			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(target)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("deleting it while the power is still out")
			var automation reactorv1alpha1.Automation
			key := types.NamespacedName{Name: target, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &automation)).To(Succeed())
			reconcileOnce(target)

			Expect(replicasOf(target)).To(Equal(int32(2)),
				"removing the automation removes the policy, so the workload comes back")
			Expect(annotationsOf(target)).NotTo(HaveKey(annotationBaselineReplicas))
			Expect(k8sClient.Get(ctx, key, &automation)).To(MatchError(ContainSubstring("not found")))
		})

		It("restores every claimed target and drops finalizers on uninstall", func() {
			const (
				declared = "uninstall-declared"
				baseline = "uninstall-baseline"
			)
			createDeployment(declared, 2)
			createDeployment(baseline, 4)
			createAutomation(declared, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(declared, 0)},
				OnExit:  []reactorv1alpha1.Action{scaleTo(declared, 2)},
			})
			createAutomation(baseline, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(baseline, 1)},
			})

			observe(map[string]string{keyUPS: upsOnBattery})
			reconcileOnce(declared)
			reconcileOnce(baseline)
			Expect(replicasOf(declared)).To(Equal(int32(0)))
			Expect(replicasOf(baseline)).To(Equal(int32(1)))

			By("running the pre-delete release with the power still out")
			Expect(ReleaseAllClaims(ctx, k8sClient)).To(Succeed())

			Expect(replicasOf(declared)).To(Equal(int32(2)), "declared onExit value")
			Expect(replicasOf(baseline)).To(Equal(int32(4)), "recorded baseline")
			Expect(annotationsOf(declared)).NotTo(HaveKey(annotationBaselineReplicas))
			Expect(annotationsOf(baseline)).NotTo(HaveKey(annotationClaimedBy))

			By("leaving nothing that would block a later delete")
			for _, name := range []string{declared, baseline} {
				var automation reactorv1alpha1.Automation
				key := types.NamespacedName{Name: name, Namespace: testNamespace}
				Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
				Expect(automation.Finalizers).NotTo(ContainElement(finalizerReleaseClaims),
					"nothing is left running to service a finalizer after uninstall")
			}
		})

		It("is a no-op for targets it never claimed", func() {
			const target = "never-claimed"
			createDeployment(target, 3)
			createAutomation(target, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyUPS: upsOnBattery},
				},
				Actions: []reactorv1alpha1.Action{scaleTo(target, 0)},
			})

			Expect(ReleaseAllClaims(ctx, k8sClient)).To(Succeed())
			Expect(replicasOf(target)).To(Equal(int32(3)))
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

		It("keeps the target claimed when one of the two is deleted", func() {
			observe(map[string]string{keyWAN: wanBackup, keyUPS: upsOnBattery})
			reconcileOnce(wanned)
			reconcileOnce(upsed)
			Expect(replicasOf(target)).To(Equal(int32(0)))

			By("deleting the WAN automation while the power is still out")
			var automation reactorv1alpha1.Automation
			key := types.NamespacedName{Name: wanned, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
			Expect(automation.Finalizers).To(ContainElement(finalizerReleaseClaims))
			Expect(k8sClient.Delete(ctx, &automation)).To(Succeed())
			reconcileOnce(wanned)

			Expect(replicasOf(target)).To(Equal(int32(0)),
				"the UPS automation still claims this target")
			Expect(k8sClient.Get(ctx, key, &automation)).To(MatchError(ContainSubstring("not found")),
				"the finalizer must be released once the target has been handed back")

			By("releasing once the survivor's condition also ends")
			observe(map[string]string{keyWAN: wanPrimary, keyUPS: upsOnline})
			reconcileOnce(upsed)
			Expect(replicasOf(target)).To(Equal(int32(2)))
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
