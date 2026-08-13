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
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

const (
	testWebhookURL = "https://ntfy.example.com/reactor"
	secretName     = "ntfy-credentials"
)

// recordingDoer stands in for the outbound transport. No test here opens a
// socket: what is being tested is when an edge action fires and what it is
// asked to send, and the transport itself is covered in internal/actions.
type recordingDoer struct {
	mu       sync.Mutex
	disabled bool
	fail     error
	sent     []actions.Request
}

func (d *recordingDoer) Enabled() bool { return !d.disabled }

func (d *recordingDoer) Do(_ context.Context, req actions.Request) (actions.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, req)
	if d.fail != nil {
		return actions.Result{Origin: "https://ntfy.example.com:443", Attempts: 3}, d.fail
	}
	return actions.Result{Origin: "https://ntfy.example.com:443", Status: 200, Attempts: 1}, nil
}

func (d *recordingDoer) requests() []actions.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]actions.Request(nil), d.sent...)
}

var _ = Describe("Edge actions", func() {
	ctx := context.Background()

	var (
		store      *engine.StateStore
		outbound   *recordingDoer
		reconciler *AutomationReconciler
	)

	BeforeEach(func() {
		store = engine.NewStateStore()
		outbound = &recordingDoer{}
		reconciler = &AutomationReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Store:        store,
			Outbound:     outbound,
			SecretReader: k8sClient,
		}
	})

	notify := func(message string) reactorv1alpha1.Action {
		return reactorv1alpha1.Action{
			Type: actions.TypeNtfy,
			Notification: &reactorv1alpha1.Notification{
				SecretRef: reactorv1alpha1.SecretReference{Name: secretName},
				Title:     "Reactor: {{ .Name }}",
				Message:   message,
			},
		}
	}

	scale := func(target string, replicas int32) reactorv1alpha1.Action {
		return reactorv1alpha1.Action{
			Type:     actionKubernetesScale,
			Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: target},
			Replicas: &replicas,
		}
	}

	createSecret := func(name string, data map[string][]byte) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Data:       data,
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })
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

	createAutomation := func(name string, spec reactorv1alpha1.AutomationSpec) *reactorv1alpha1.Automation {
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
		return automation
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

	statusOf := func(name string) reactorv1alpha1.AutomationStatus {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		return automation.Status
	}

	readyOf := func(name string) *metav1.Condition {
		status := statusOf(name)
		for i := range status.Conditions {
			if status.Conditions[i].Type == conditionReady {
				return &status.Conditions[i]
			}
		}
		return nil
	}

	notifyOnly := func(name, message string) {
		createAutomation(name, reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Actions: []reactorv1alpha1.Action{notify(message)},
		})
	}

	Context("firing on a transition", func() {
		It("sends once on entering the state, with the message rendered from the transition", func() {
			const name = "edge-enter"
			createSecret(secretName, map[string][]byte{
				actions.SecretKeyURL:           []byte(testWebhookURL),
				actions.SecretKeyAuthorization: []byte("Bearer tk_example"),
			})
			notifyOnly(name, "{{ .Key }} moved from {{ .From }} to {{ .To }}")

			By("staying quiet while the condition does not hold")
			observe(map[string]string{keyWAN: wanPrimary})
			reconcileOnce(name)
			Expect(outbound.requests()).To(BeEmpty())

			By("sending on the edge into the state")
			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)
			sent := outbound.requests()
			Expect(sent).To(HaveLen(1))
			Expect(sent[0].URL).To(Equal(testWebhookURL))
			Expect(sent[0].Header.Get("Authorization")).To(Equal("Bearer tk_example"))
			Expect(sent[0].Header.Get("X-Title")).To(Equal("Reactor: " + name))
			Expect(string(sent[0].Body)).To(Equal("wan moved from primary to backup"))
			Expect(sent[0].Retryable).To(BeTrue())

			By("not sending again while nothing transitions")
			reconcileOnce(name)
			reconcileOnce(name)
			Expect(outbound.requests()).To(HaveLen(1))

			status := statusOf(name)
			Expect(status.EdgeActions).To(HaveLen(1))
			Expect(status.EdgeActions[0].Status).To(Equal(executionSuccess))
			Expect(status.EdgeActions[0].Type).To(Equal(actions.TypeNtfy))
			Expect(status.EdgeActions[0].Destination).To(Equal("https://ntfy.example.com:443"))
			Expect(status.EdgeActions[0].OnExit).To(BeFalse())
		})

		It("fires an edge action from onExit on this automation's own edge", func() {
			const name = "edge-exit"
			createSecret(secretName, map[string][]byte{actions.SecretKeyURL: []byte(testWebhookURL)})
			createAutomation(name, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{notify("failed over")},
				OnExit:  []reactorv1alpha1.Action{notify("recovered")},
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)
			observe(map[string]string{keyWAN: wanPrimary})
			reconcileOnce(name)

			sent := outbound.requests()
			Expect(sent).To(HaveLen(2))
			Expect(string(sent[0].Body)).To(Equal("failed over"))
			Expect(string(sent[1].Body)).To(Equal("recovered"))

			status := statusOf(name)
			Expect(status.EdgeActions).To(HaveLen(1))
			Expect(status.EdgeActions[0].OnExit).To(BeTrue())
		})

		It("sends nothing while the automation is suspended", func() {
			const name = "edge-suspended"
			createSecret(secretName, map[string][]byte{actions.SecretKeyURL: []byte(testWebhookURL)})
			createAutomation(name, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{notify("failed over")},
				Suspend: true,
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			// Suspension is a reversible delete, and a deleted automation does
			// not announce transitions it is no longer acting on.
			Expect(outbound.requests()).To(BeEmpty())
			Expect(statusOf(name).Matching).To(BeTrue())
		})
	})

	Context("when an edge action fails", func() {
		It("records the failure without failing the automation whose target was scaled", func() {
			const name = "edge-failure"
			createSecret(secretName, map[string][]byte{actions.SecretKeyURL: []byte(testWebhookURL)})
			createDeployment(name, 1)
			createAutomation(name, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{scale(name, 0), notify("scaled down")},
			})
			outbound.fail = errors.New("https://ntfy.example.com:443: responded 502 Bad Gateway")

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			By("having scaled the workload anyway")
			var deployment appsv1.Deployment
			key := types.NamespacedName{Name: name, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(0)))

			By("reporting the notification as failed, and the automation as healthy")
			status := statusOf(name)
			Expect(status.EdgeActions).To(HaveLen(1))
			Expect(status.EdgeActions[0].Status).To(Equal(executionFailed))
			Expect(status.EdgeActions[0].Reason).To(ContainSubstring("502"))
			Expect(status.LastExecution.Status).To(Equal(executionSuccess))
			Expect(readyOf(name).Status).To(Equal(metav1.ConditionTrue))

			By("not retrying it on a later reconcile, which has no transition to report")
			reconcileOnce(name)
			Expect(outbound.requests()).To(HaveLen(1))
		})

		It("refuses the action when no destination is allowed on this install", func() {
			const name = "edge-disabled"
			createSecret(secretName, map[string][]byte{actions.SecretKeyURL: []byte(testWebhookURL)})
			notifyOnly(name, "hello")
			outbound.disabled = true

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			Expect(outbound.requests()).To(BeEmpty())
			status := statusOf(name)
			Expect(status.EdgeActions).To(HaveLen(1))
			Expect(status.EdgeActions[0].Status).To(Equal(executionFailed))
			Expect(status.EdgeActions[0].Reason).To(ContainSubstring("no destination is allowed"))
		})

		It("reports a missing credential secret rather than sending without one", func() {
			const name = "edge-no-secret"
			notifyOnly(name, "hello")

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			Expect(outbound.requests()).To(BeEmpty())
			status := statusOf(name)
			Expect(status.EdgeActions).To(HaveLen(1))
			Expect(status.EdgeActions[0].Status).To(Equal(executionFailed))
			Expect(status.EdgeActions[0].Reason).To(ContainSubstring(secretName))
		})

		It("reports a secret with no url key rather than guessing a destination", func() {
			const name = "edge-no-url"
			createSecret(secretName, map[string][]byte{
				actions.SecretKeyAuthorization: []byte("Bearer tk_example"),
			})
			notifyOnly(name, "hello")

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			Expect(outbound.requests()).To(BeEmpty())
			Expect(statusOf(name).EdgeActions[0].Reason).To(ContainSubstring(actions.SecretKeyURL))
		})
	})

	Context("ordering against the desired-state actions", func() {
		It("does not announce a transition whose target could not be written", func() {
			const name = "edge-after-apply"
			createSecret(secretName, map[string][]byte{actions.SecretKeyURL: []byte(testWebhookURL)})
			createDeployment(name, 1)
			one := int32(1)
			action := scale(name, 0)
			action.TimeoutSeconds = &one
			createAutomation(name, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{action, notify("scaled down")},
			})

			// A target that has stopped answering fails the desired-state
			// action, which leaves status.matching where it was — so there is
			// no committed transition, and nothing to announce.
			reconciler.Client = stallingClient{Client: k8sClient}
			observe(map[string]string{keyWAN: wanBackup})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(outbound.requests()).To(BeEmpty())
			Expect(statusOf(name).Matching).To(BeFalse())

			By("announcing it once the target does get written")
			reconciler.Client = k8sClient
			reconcileOnce(name)
			Expect(outbound.requests()).To(HaveLen(1))
			Expect(statusOf(name).Matching).To(BeTrue())
		})
	})

	Context("schema validation", func() {
		// The action type enum is a CRD schema change, and the CEL rules beside
		// it are what stop a type and its configuration block drifting apart.
		rejects := func(name string, action reactorv1alpha1.Action) {
			automation := &reactorv1alpha1.Automation{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec: reactorv1alpha1.AutomationSpec{
					When: &reactorv1alpha1.StateTrigger{
						Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
					},
					Actions: []reactorv1alpha1.Action{action},
				},
			}
			Expect(k8sClient.Create(ctx, automation)).NotTo(Succeed())
		}

		It("rejects an action whose type and configuration block disagree", func() {
			rejects("no-notification", reactorv1alpha1.Action{Type: actions.TypeNtfy})
			rejects("no-request", reactorv1alpha1.Action{Type: actions.TypeHTTPRequest})
			rejects("notification-on-scale", reactorv1alpha1.Action{
				Type: actionKubernetesScale,
				Notification: &reactorv1alpha1.Notification{
					SecretRef: reactorv1alpha1.SecretReference{Name: secretName}, Message: "x",
				},
			})
		})

		It("rejects a target or replicas on an action that owns no target", func() {
			rejects("targeted-notification", reactorv1alpha1.Action{
				Type:   actions.TypeNtfy,
				Target: &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: targetQbit},
				Notification: &reactorv1alpha1.Notification{
					SecretRef: reactorv1alpha1.SecretReference{Name: secretName}, Message: "x",
				},
			})
		})
	})

	Context("http.request", func() {
		It("retries a PUT but attempts a POST exactly once", func() {
			const name = "edge-http"
			createAutomation(name, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{
					{
						Type: actions.TypeHTTPRequest,
						Request: &reactorv1alpha1.HTTPRequest{
							Method: "PUT",
							URL:    "https://example.com/state",
							Body:   `{"wan": {{ json .To }}}`,
						},
					},
					{
						Type: actions.TypeHTTPRequest,
						Request: &reactorv1alpha1.HTTPRequest{
							Method: "POST",
							URL:    "https://example.com/orders",
						},
					},
				},
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			sent := outbound.requests()
			Expect(sent).To(HaveLen(2))
			Expect(sent[0].Retryable).To(BeTrue(), "PUT is idempotent per RFC 9110")
			Expect(string(sent[0].Body)).To(Equal(`{"wan": "backup"}`))
			Expect(sent[1].Retryable).To(BeFalse(), "an unknown POST is attempted at most once")
		})

		It("refuses an inline Authorization header", func() {
			const name = "edge-inline-auth"
			createAutomation(name, reactorv1alpha1.AutomationSpec{
				When: &reactorv1alpha1.StateTrigger{
					Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
				},
				Actions: []reactorv1alpha1.Action{{
					Type: actions.TypeHTTPRequest,
					Request: &reactorv1alpha1.HTTPRequest{
						URL:     "https://example.com/hook",
						Headers: []reactorv1alpha1.HTTPHeader{{Name: "authorization", Value: "Bearer inline"}},
					},
				}},
			})

			observe(map[string]string{keyWAN: wanBackup})
			reconcileOnce(name)

			Expect(outbound.requests()).To(BeEmpty())
			Expect(statusOf(name).EdgeActions[0].Reason).To(ContainSubstring("belongs in the referenced secret"))
		})
	})
})
