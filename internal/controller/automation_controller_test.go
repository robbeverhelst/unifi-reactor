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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

var _ = Describe("Automation Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
			targetName        = "qbittorrent"
			wanPrimary        = "primary"
			wanBackup         = "backup"
			keyWAN            = "wan"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		automation := &reactorv1alpha1.Automation{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Automation")
			err := k8sClient.Get(ctx, typeNamespacedName, automation)
			if err != nil && errors.IsNotFound(err) {
				zero, one := int32(0), int32(1)
				resource := &reactorv1alpha1.Automation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: reactorv1alpha1.AutomationSpec{
						When: &reactorv1alpha1.StateTrigger{
							Provider: providerUniFi,
							State:    map[string]string{keyWAN: wanBackup},
						},
						Actions: []reactorv1alpha1.Action{{
							Type:     actionKubernetesScale,
							Target:   &reactorv1alpha1.TargetRef{Kind: "Deployment", Name: targetName},
							Replicas: &zero,
						}},
						OnExit: []reactorv1alpha1.Action{{
							Type:     actionKubernetesScale,
							Target:   &reactorv1alpha1.TargetRef{Kind: "Deployment", Name: targetName},
							Replicas: &one,
						}},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &reactorv1alpha1.Automation{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Automation")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should scale the target on state transitions and reverse via onExit", func() {
			By("creating the target deployment at 1 replica")
			one := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: resourceNamespace},
				Spec: appsv1.DeploymentSpec{
					Replicas: &one,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": targetName}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": targetName}},
						Spec: corev1.PodSpec{Containers: []corev1.Container{
							{Name: targetName, Image: "example/qbittorrent"},
						}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, deployment) })

			store := engine.NewStateStore()
			controllerReconciler := &AutomationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Store:  store,
			}
			reconcileOnce := func() {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
			}
			replicasNow := func() int32 {
				var d appsv1.Deployment
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetName, Namespace: resourceNamespace}, &d)).To(Succeed())
				return *d.Spec.Replicas
			}

			By("reporting pending while no provider state is observed")
			reconcileOnce()
			Expect(replicasNow()).To(Equal(int32(1)))

			By("entering the matching state (wan=backup) scales down")
			store.Observe(events.Observation{Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup}, ObservedAt: time.Now()})
			reconcileOnce()
			Expect(replicasNow()).To(Equal(int32(0)))

			By("repeated identical observations are no-ops")
			store.Observe(events.Observation{Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup}, ObservedAt: time.Now()})
			reconcileOnce()
			Expect(replicasNow()).To(Equal(int32(0)))

			By("leaving the matching state runs onExit and scales back up")
			store.Observe(events.Observation{Provider: providerUniFi, State: map[string]string{keyWAN: wanPrimary}, ObservedAt: time.Now()})
			reconcileOnce()
			Expect(replicasNow()).To(Equal(int32(1)))

			By("recording status")
			var reconciled reactorv1alpha1.Automation
			Expect(k8sClient.Get(ctx, typeNamespacedName, &reconciled)).To(Succeed())
			Expect(reconciled.Status.Matching).To(BeFalse())
			Expect(reconciled.Status.ObservedState).To(HaveKeyWithValue(keyWAN, wanPrimary))
			Expect(reconciled.Status.LastExecution).NotTo(BeNil())
			Expect(reconciled.Status.LastExecution.OnExit).To(BeTrue())
		})
	})
})
