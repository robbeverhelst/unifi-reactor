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

// How long Reactor may go on acting on a console state that has already
// changed.
//
// There are two windows and only one of them is bounded by anything. A value
// that CHANGED reaches an Automation within one poll interval times the samples
// its key has to hold for, and both terms are the operator's own settings. A
// console that stops ANSWERING has no such window: the store keeps reporting
// what it last saw, and every reconcile re-decides against it for as long as
// that lasts.
//
// The answer this file pins is that the second window stays unbounded — going
// blind must not release a claim during the incident that took the console
// away — and stops being silent.
var _ = Describe("Deciding against an observation that has stopped arriving", func() {
	ctx := context.Background()

	const (
		staleTarget = "stale-target"
		baseline    = 3
		// bound is short only so the tests can state an age against it; every
		// observation below is either seconds old or an hour old.
		bound = time.Minute
	)

	var (
		store      *engine.StateStore
		reconciler *AutomationReconciler
	)

	newReconciler := func(options ...engine.StoreOption) {
		store = engine.NewStateStore(options...)
		reconciler = &AutomationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Store:  store,
		}
	}

	observeAt := func(at time.Time, state map[string]string) {
		store.Observe(events.Observation{Provider: providerUniFi, State: state, ObservedAt: at})
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

	createAutomation := func(name string) {
		replicas := int32(0)
		automation := &reactorv1alpha1.Automation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{{
					Type:     actionKubernetesScale,
					Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: staleTarget},
					Replicas: &replicas,
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

	reconcileOnce := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	automationOf := func(name string) reactorv1alpha1.Automation {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		return automation
	}

	replicasOf := func(name string) int32 {
		var deployment appsv1.Deployment
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
		return *deployment.Spec.Replicas
	}

	readyOf := func(name string) *metav1.Condition {
		automation := automationOf(name)
		for i := range automation.Status.Conditions {
			if automation.Status.Conditions[i].Type == conditionReady {
				return &automation.Status.Conditions[i]
			}
		}
		return nil
	}

	It("reports when the state it decided against was observed", func() {
		const name = "stale-timestamp"
		newReconciler()
		createDeployment(staleTarget, baseline)
		createAutomation(name)

		observedAt := time.Now().Add(-30 * time.Second).Truncate(time.Second)
		observeAt(observedAt, map[string]string{keyWAN: wanPrimary})
		reconcileOnce(name)

		Expect(automationOf(name).Status.ObservedAt).To(HaveValue(
			HaveField("Time", BeTemporally("==", observedAt))),
			"a decision is only as current as the observation it was taken against, and nothing said which")
	})

	It("says nothing about an old observation when the install set no bound", func() {
		const name = "stale-unbounded"
		newReconciler()
		createDeployment(staleTarget, baseline)
		createAutomation(name)

		observeAt(time.Now().Add(-time.Hour), map[string]string{keyWAN: wanPrimary})
		reconcileOnce(name)

		ready := readyOf(name)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("Reconciled"),
			"an install that set no bound was told its state is too old, which every upgrade would inherit")
	})

	It("says so, and keeps holding the target, once the observation is older than the bound", func() {
		const name = "stale-bounded"
		newReconciler(engine.WithStaleAfter(providerUniFi, bound))
		createDeployment(staleTarget, baseline)
		createAutomation(name)

		By("claiming the target against a fresh observation")
		observeAt(time.Now(), map[string]string{keyWAN: wanBackup})
		reconcileOnce(name)
		Expect(replicasOf(staleTarget)).To(BeEquivalentTo(0))

		By("then letting the console stop answering, which leaves that observation standing")
		observeAt(time.Now().Add(-time.Hour), map[string]string{keyWAN: wanBackup})
		reconcileOnce(name)

		ready := readyOf(name)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonObservationStale))
		Expect(ready.Message).To(ContainSubstring("still acting on the state it last reported"))

		Expect(replicasOf(staleTarget)).To(BeEquivalentTo(0),
			"going blind released a claim, which is the workload coming back up mid-outage")
		Expect(automationOf(name).Status.Matching).To(BeTrue(),
			"an observation that stopped arriving was treated as the condition ending")
	})

	It("clears the moment the console answers again", func() {
		const name = "stale-recovered"
		newReconciler(engine.WithStaleAfter(providerUniFi, bound))
		createDeployment(staleTarget, baseline)
		createAutomation(name)

		observeAt(time.Now().Add(-time.Hour), map[string]string{keyWAN: wanBackup})
		reconcileOnce(name)
		Expect(readyOf(name).Reason).To(Equal(reasonObservationStale))

		observeAt(time.Now(), map[string]string{keyWAN: wanBackup})
		reconcileOnce(name)

		ready := readyOf(name)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("Reconciled"))
	})
})
