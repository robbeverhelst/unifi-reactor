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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
)

// The WLAN fixture. It is never on any console: every test here stops at the
// seam, and what happens past it is covered in internal/providers/unifi against
// a stub that speaks the console's shape.
const testWLANName = "test-guest"

// recordingConsole stands in for a provider's console writer. What is being
// tested here is that the right actions reach it, with the right timeout, and
// that its failures are reported the way an edge action's are — not what any
// console does with them.
type recordingConsole struct {
	mu       sync.Mutex
	fail     error
	applied  []reactorv1alpha1.Action
	timeouts []time.Duration
}

func (c *recordingConsole) Apply(
	_ context.Context,
	action reactorv1alpha1.Action,
	timeout time.Duration,
) (actions.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = append(c.applied, action)
	c.timeouts = append(c.timeouts, timeout)
	origin := "unifi/wlan/" + action.WLAN.Name
	if c.fail != nil {
		return actions.Result{Origin: origin, Attempts: 1}, c.fail
	}
	return actions.Result{Origin: origin, Attempts: 1}, nil
}

func (c *recordingConsole) seen() []reactorv1alpha1.Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reactorv1alpha1.Action(nil), c.applied...)
}

func (c *recordingConsole) budgets() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.timeouts...)
}

var _ = Describe("Console actions", func() {
	ctx := context.Background()

	var (
		store      *engine.StateStore
		console    *recordingConsole
		outbound   *recordingDoer
		recorder   *fakeRecorder
		reconciler *AutomationReconciler
	)

	BeforeEach(func() {
		store = engine.NewStateStore()
		console = &recordingConsole{}
		outbound = &recordingDoer{}
		recorder = &fakeRecorder{}
		reconciler = &AutomationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Store:    store,
			Recorder: recorder,
			Outbound: outbound,
			Console:  console,
		}
	})

	create := func(name string, spec reactorv1alpha1.AutomationSpec) *reactorv1alpha1.Automation {
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

	// observe drives the store, then reconciles once so the Automation sees the
	// transition its edge actions fire on.
	observe := func(name string, state map[string]string) {
		store.Observe(events.Observation{
			Provider: providerUniFi, State: state, ObservedAt: time.Now(),
		})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	onBackup := func(list ...reactorv1alpha1.Action) reactorv1alpha1.AutomationSpec {
		return reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi,
				State:    map[string]string{keyWAN: wanBackup},
			},
			Actions: list,
		}
	}

	wlan := func(actionType string) reactorv1alpha1.Action {
		return reactorv1alpha1.Action{
			Type: actionType,
			WLAN: &reactorv1alpha1.WLAN{Name: testWLANName},
		}
	}

	wlanAutomation := func(name, actionType string) *reactorv1alpha1.Automation {
		return create(name, onBackup(wlan(actionType)))
	}

	statusOf := func(name string) reactorv1alpha1.AutomationStatus {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		return automation.Status
	}

	It("routes a unifi.wlan action to the console rather than the outbound client", func() {
		wlanAutomation("wlan-disable", actions.TypeUniFiWLANDisable)

		observe("wlan-disable", map[string]string{keyWAN: wanPrimary})
		observe("wlan-disable", map[string]string{keyWAN: wanBackup})

		applied := console.seen()
		Expect(applied).To(HaveLen(1))
		Expect(applied[0].Type).To(Equal(actions.TypeUniFiWLANDisable))
		Expect(applied[0].WLAN.Name).To(Equal(testWLANName))
		// The outbound client is what enforces the destination allowlist for
		// requests an Automation addressed. A console write is not one of those,
		// and must not be able to borrow that path.
		Expect(outbound.requests()).To(BeEmpty())
	})

	It("gives a console write the same budget a kubernetes action gets, not the outbound one", func() {
		wlanAutomation("wlan-budget", actions.TypeUniFiWLANDisable)

		observe("wlan-budget", map[string]string{keyWAN: wanPrimary})
		observe("wlan-budget", map[string]string{keyWAN: wanBackup})

		// A login, a check and a write is three round trips, so the shorter
		// outbound default would be the wrong budget to hand it.
		Expect(console.budgets()).To(ConsistOf(defaultActionTimeout))
		Expect(defaultActionTimeout).To(BeNumerically(">", actions.DefaultTimeout))
	})

	It("honours spec.actions[].timeoutSeconds", func() {
		automation := wlanAutomation("wlan-timeout", actions.TypeUniFiWLANDisable)
		seconds := int32(5)
		automation.Spec.Actions[0].TimeoutSeconds = &seconds
		Expect(k8sClient.Update(ctx, automation)).To(Succeed())

		observe("wlan-timeout", map[string]string{keyWAN: wanPrimary})
		observe("wlan-timeout", map[string]string{keyWAN: wanBackup})

		Expect(console.budgets()).To(ConsistOf(5 * time.Second))
	})

	It("records a console action in status and raises an Event naming the object", func() {
		wlanAutomation("wlan-status", actions.TypeUniFiWLANDisable)

		observe("wlan-status", map[string]string{keyWAN: wanPrimary})
		observe("wlan-status", map[string]string{keyWAN: wanBackup})

		status := statusOf("wlan-status")
		Expect(status.EdgeActions).To(HaveLen(1))
		Expect(status.EdgeActions[0].Status).To(Equal(executionSuccess))
		Expect(status.EdgeActions[0].Destination).To(Equal("unifi/wlan/" + testWLANName))

		// "applied to" rather than "delivered to": this changed a named thing on
		// the console, it did not announce anything to an address.
		sent, found := recorder.find(reasonEdgeActionSent)
		Expect(found).To(BeTrue())
		Expect(sent.Note).To(ContainSubstring("applied to unifi/wlan/" + testWLANName))
	})

	It("reports a console failure as a warning without failing the Automation", func() {
		console.fail = errors.New(`wlan "test-guest" is not allowed by this install`)
		wlanAutomation("wlan-refused", actions.TypeUniFiWLANDisable)

		observe("wlan-refused", map[string]string{keyWAN: wanPrimary})
		observe("wlan-refused", map[string]string{keyWAN: wanBackup})

		status := statusOf("wlan-refused")
		Expect(status.EdgeActions).To(HaveLen(1))
		Expect(status.EdgeActions[0].Status).To(Equal(executionFailed))
		Expect(status.EdgeActions[0].Reason).To(ContainSubstring("not allowed by this install"))
		// A WLAN that did not change is not an Automation that failed to do its
		// job: nothing here claims a target, so nothing is reported as unapplied.
		Expect(status.Targets).To(BeEmpty())
		_, found := recorder.find(reasonEdgeActionFailed)
		Expect(found).To(BeTrue())
	})

	It("refuses a console action when no console is configured", func() {
		reconciler.Console = nil
		wlanAutomation("wlan-no-console", actions.TypeUniFiWLANDisable)

		observe("wlan-no-console", map[string]string{keyWAN: wanPrimary})
		observe("wlan-no-console", map[string]string{keyWAN: wanBackup})

		status := statusOf("wlan-no-console")
		Expect(status.EdgeActions).To(HaveLen(1))
		Expect(status.EdgeActions[0].Status).To(Equal(executionFailed))
		Expect(status.EdgeActions[0].Reason).To(ContainSubstring("no console is configured"))
	})

	It("claims no target, because there is nowhere to record what the WLAN was", func() {
		wlanAutomation("wlan-unarbitrated", actions.TypeUniFiWLANDisable)

		observe("wlan-unarbitrated", map[string]string{keyWAN: wanPrimary})
		observe("wlan-unarbitrated", map[string]string{keyWAN: wanBackup})

		// This is the #64 rule pinned rather than merely documented: a WLAN is
		// not a Kubernetes object, so it has no baseline, so it is not claimed
		// and not arbitrated.
		Expect(statusOf("wlan-unarbitrated").Targets).To(BeEmpty())
	})

	It("writes nothing to the console under spec.dryRun", func() {
		spec := onBackup(wlan(actions.TypeUniFiWLANDisable))
		spec.DryRun = true
		create("wlan-dry-run", spec)

		observe("wlan-dry-run", map[string]string{keyWAN: wanPrimary})
		observe("wlan-dry-run", map[string]string{keyWAN: wanBackup})

		// A dry run that switched somebody's WiFi off would be the promise
		// broken by the half nobody was watching. The Automation is out of
		// force, so the transition reports a preview and sends nothing.
		Expect(console.seen()).To(BeEmpty())
	})

	It("writes nothing to the console when the whole install is a dry run", func() {
		reconciler.DryRun = true
		wlanAutomation("wlan-install-dry-run", actions.TypeUniFiWLANDisable)

		observe("wlan-install-dry-run", map[string]string{keyWAN: wanPrimary})
		observe("wlan-install-dry-run", map[string]string{keyWAN: wanBackup})

		Expect(console.seen()).To(BeEmpty())
		status := statusOf("wlan-install-dry-run")
		Expect(status.EdgeActions).To(HaveLen(1))
		Expect(status.EdgeActions[0].Status).To(Equal(executionSkipped))
	})

	Describe("admission", func() {
		rejects := func(name string, list ...reactorv1alpha1.Action) {
			Expect(k8sClient.Create(ctx, &reactorv1alpha1.Automation{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec:       onBackup(list...),
			})).NotTo(Succeed())
		}

		It("rejects a unifi.wlan action with no wlan block", func() {
			rejects("wlan-no-block", reactorv1alpha1.Action{Type: actions.TypeUniFiWLANDisable})
		})

		It("rejects a wlan block on an action that is not a unifi.wlan one", func() {
			rejects("wlan-wrong-type", reactorv1alpha1.Action{
				Type:   actionKubernetesRestart,
				Target: &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: "sonarr"},
				WLAN:   &reactorv1alpha1.WLAN{Name: testWLANName},
			})
		})

		It("rejects an empty SSID", func() {
			rejects("wlan-empty-name", reactorv1alpha1.Action{
				Type: actions.TypeUniFiWLANDisable,
				WLAN: &reactorv1alpha1.WLAN{Name: ""},
			})
		})

		It("accepts disable on the way in and enable on the way out", func() {
			spec := onBackup(wlan(actions.TypeUniFiWLANDisable))
			spec.OnExit = []reactorv1alpha1.Action{wlan(actions.TypeUniFiWLANEnable)}
			create("wlan-pair", spec)
		})
	})

	It("fires the onExit action on the way out", func() {
		spec := onBackup(wlan(actions.TypeUniFiWLANDisable))
		spec.OnExit = []reactorv1alpha1.Action{wlan(actions.TypeUniFiWLANEnable)}
		create("wlan-exit", spec)

		observe("wlan-exit", map[string]string{keyWAN: wanPrimary})
		observe("wlan-exit", map[string]string{keyWAN: wanBackup})
		observe("wlan-exit", map[string]string{keyWAN: wanPrimary})

		applied := console.seen()
		Expect(applied).To(HaveLen(2))
		Expect(applied[0].Type).To(Equal(actions.TypeUniFiWLANDisable))
		Expect(applied[1].Type).To(Equal(actions.TypeUniFiWLANEnable))

		// status.edgeActions answers "what happened when this last changed", so
		// the exit run replaces the entry rather than being appended to it.
		status := statusOf("wlan-exit")
		Expect(status.EdgeActions).To(HaveLen(1))
		Expect(status.EdgeActions[0].OnExit).To(BeTrue())
	})
})
