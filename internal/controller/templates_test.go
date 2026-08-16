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
	"fmt"
	"strings"
	"testing"
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
	// keyISP is the key these tests reference and deliberately do not match on.
	// It is the one from #89: observed, visible in status.observedState, and
	// absent from .State because the Automation never asked for it.
	keyISP = "isp"
	// carrierSlug is a value of that key, whose set nothing enumerates.
	carrierSlug = "carrier-a"
)

func notifyWith(message string) reactorv1alpha1.Action {
	return reactorv1alpha1.Action{
		Type: "notification.ntfy",
		Notification: &reactorv1alpha1.Notification{
			SecretRef: reactorv1alpha1.SecretReference{Name: "ntfy-credentials"},
			Message:   message,
		},
	}
}

func automationWithActions(actionList, onExit []reactorv1alpha1.Action) *reactorv1alpha1.Automation {
	return &reactorv1alpha1.Automation{
		ObjectMeta: metav1.ObjectMeta{Name: "notify-on-wan-failover", Namespace: testNamespace},
		Spec: reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Actions: actionList,
			OnExit:  onExit,
		},
	}
}

// TestTemplateFaultsNamesTheKeyAndTheFix is the acceptance criterion of #89: a
// message saying only that a template is invalid repeats the original problem
// somewhere new.
func TestTemplateFaultsNamesTheKeyAndTheFix(t *testing.T) {
	faults := templateFaults(automationWithActions(
		[]reactorv1alpha1.Action{notifyWith("WAN failed over to {{ .State.isp }}")}, nil))
	if len(faults) != 1 {
		t.Fatalf("templateFaults = %v, want one", faults)
	}
	for _, want := range []string{
		"spec.actions[0].notification.message",
		`state key "isp"`,
		"does not match on",
		"add isp to spec.when.state",
	} {
		if !strings.Contains(faults[0], want) {
			t.Errorf("the fault does not say %q: %s", want, faults[0])
		}
	}
}

func TestTemplateFaultsCoversEveryTemplateAnActionRenders(t *testing.T) {
	automation := automationWithActions(
		[]reactorv1alpha1.Action{
			{
				Type: actions.TypeHTTPRequest,
				Request: &reactorv1alpha1.HTTPRequest{
					URL:  "https://hooks.example.com/reactor",
					Body: `{"isp": {{ json .State.isp }}}`,
				},
			},
			{
				Type: actions.TypeHomeAssistant,
				HomeAssistant: &reactorv1alpha1.HomeAssistantService{
					Domain: "light", Service: "turn_on", Data: `{"note": "{{ .State.isp }}"}`,
				},
			},
			func() reactorv1alpha1.Action {
				action := notifyWith("fine: {{ .State.wan }}")
				action.Notification.Title = "{{ .State.isp }}"
				return action
			}(),
		},
		// onExit matters as much as actions: it renders against the same
		// narrowed context, on the recovery nobody is watching for.
		[]reactorv1alpha1.Action{notifyWith("back to {{ .State.isp }}")})

	found := templateFaults(automation)
	paths := make([]string, 0, len(found))
	for _, fault := range found {
		paths = append(paths, strings.Fields(fault)[0])
	}
	want := []string{
		"spec.actions[0].request.body",
		"spec.actions[1].homeAssistant.data",
		"spec.actions[2].notification.title",
		"spec.onExit[0].notification.message",
	}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("faults reported for %v, want %v", paths, want)
	}
}

// TestTemplateFaultsIgnoresWhatIsNeverRendered guards the other direction: a
// block left on an action of a type that does not read it is dead YAML, and
// reporting a working Automation as broken is worse than the trap itself.
func TestTemplateFaultsIgnoresWhatIsNeverRendered(t *testing.T) {
	scale := scaleAction(0)
	scale.Notification = &reactorv1alpha1.Notification{Message: "{{ .State.isp }}"}
	automation := automationWithActions([]reactorv1alpha1.Action{
		scale,
		{Type: actions.TypeQBittorrentPause, QBittorrent: &reactorv1alpha1.QBittorrent{
			URL: "https://qbittorrent.example.com", SecretRef: reactorv1alpha1.SecretReference{Name: "qbit"},
		}},
		notifyWith("uplink is {{ .State.wan }}, matched at {{ .Time }}"),
	}, nil)

	if faults := templateFaults(automation); len(faults) > 0 {
		t.Fatalf("templateFaults = %v, want nothing", faults)
	}
}

func scaleAction(replicas int32) reactorv1alpha1.Action {
	return reactorv1alpha1.Action{
		Type:     actionKubernetesScale,
		Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: targetQbit},
		Replicas: &replicas,
	}
}

func TestBrokenTemplatesAreReportedOnceAsAWarning(t *testing.T) {
	recorder := &fakeRecorder{}
	r := &AutomationReconciler{Recorder: recorder}
	automation := automationWithActions(
		[]reactorv1alpha1.Action{notifyWith("{{ .State.isp }}")}, nil)
	faults := templateFaults(automation)

	for range 3 {
		if !r.reportBrokenTemplates(automation, faults) {
			t.Fatal("a broken template must be reported")
		}
	}

	event, ok := recorder.find(reasonTemplateWillNotRender)
	if !ok {
		t.Fatalf("no %s Event: %v", reasonTemplateWillNotRender, recorder.reasons())
	}
	if event.Type != corev1.EventTypeWarning {
		t.Errorf("a template that can never render is a %q; somebody has to change the spec", event.Type)
	}
	if len(recorder.reasons()) != 1 {
		t.Errorf("reconciling every %s must not raise an Event each time: %v",
			reevaluateInterval, recorder.reasons())
	}
	ready := automation.Status.Conditions[0]
	if ready.Type != conditionReady || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %s/%s, want Ready=False", ready.Type, ready.Status)
	}
	if !strings.Contains(ready.Message, keyISP) {
		t.Errorf("the condition message does not name the key: %s", ready.Message)
	}
}

// TestManyBrokenTemplatesAreReportedBriefly keeps a pathological spec from
// producing a condition message the API server refuses for length, which would
// turn a template somebody can fix into a status write that never lands.
func TestManyBrokenTemplatesAreReportedBriefly(t *testing.T) {
	list := make([]reactorv1alpha1.Action, 0, 20)
	for i := range 20 {
		list = append(list, notifyWith(fmt.Sprintf("{{ .State.key%d }}", i)))
	}
	r := &AutomationReconciler{}
	automation := automationWithActions(list, nil)
	faults := templateFaults(automation)
	if len(faults) != 20 {
		t.Fatalf("templateFaults reported %d, want one per action", len(faults))
	}

	r.reportBrokenTemplates(automation, faults)
	message := automation.Status.Conditions[0].Message
	if !strings.Contains(message, "and 15 more like it") {
		t.Errorf("the condition does not say how much it left out: %s", message)
	}
	if len(message) > 4096 {
		t.Errorf("the condition message is %d bytes; it has to stay readable and writable", len(message))
	}
}

var _ = Describe("An automation whose template can never render", func() {
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

	readyOf := func(name string) *metav1.Condition {
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		for i := range automation.Status.Conditions {
			if automation.Status.Conditions[i].Type == conditionReady {
				return &automation.Status.Conditions[i]
			}
		}
		return nil
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

	// The whole point of #89: this is reported on an object that has never
	// matched anything, on an install whose provider has not answered yet.
	It("says so before any state has been observed", func() {
		const name = "notify-unmatched-key"
		createAutomation(name, reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Actions: []reactorv1alpha1.Action{notifyWith("WAN failed over to {{ .State.isp }}")},
		})

		reconcileOnce(name)
		ready := readyOf(name)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonTemplateWillNotRender))
		Expect(ready.Message).To(ContainSubstring(keyISP))
		Expect(ready.Message).To(ContainSubstring("spec.when.state"))
	})

	It("keeps acting on its targets, because the message is the report and not the reaction", func() {
		const name = "notify-and-scale"
		createDeployment(name, 2)
		createAutomation(name, reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Actions: []reactorv1alpha1.Action{
				{
					Type:     actionKubernetesScale,
					Target:   &reactorv1alpha1.TargetRef{Kind: kindDeployment, Name: name},
					Replicas: ptrTo(int32(0)),
				},
				notifyWith("scaled down, carrier is {{ .State.isp }}"),
			},
		})

		observe(map[string]string{keyWAN: wanBackup, keyISP: carrierSlug})
		reconcileOnce(name)

		var deployment appsv1.Deployment
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: testNamespace}, &deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(0)),
			"a typo in a notification must not cost somebody the failover it was reporting")
		Expect(readyOf(name).Reason).To(Equal(reasonTemplateWillNotRender))
	})

	It("clears once the key is matched on, and stays quiet for a template that renders", func() {
		const name = "notify-fixed"
		createAutomation(name, reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Actions: []reactorv1alpha1.Action{notifyWith("carrier is {{ .State.isp }}")},
		})
		observe(map[string]string{keyWAN: wanPrimary, keyISP: carrierSlug})
		reconcileOnce(name)
		Expect(readyOf(name).Reason).To(Equal(reasonTemplateWillNotRender))

		By("matching on the key the message reads")
		var automation reactorv1alpha1.Automation
		key := types.NamespacedName{Name: name, Namespace: testNamespace}
		Expect(k8sClient.Get(ctx, key, &automation)).To(Succeed())
		automation.Spec.When.State[keyISP] = carrierSlug
		Expect(k8sClient.Update(ctx, &automation)).To(Succeed())

		reconcileOnce(name)
		Expect(readyOf(name).Reason).To(Equal("Reconciled"))
		Expect(readyOf(name).Status).To(Equal(metav1.ConditionTrue))
	})

	It("reports it on a suspended automation too", func() {
		const name = "notify-suspended"
		createAutomation(name, reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{
				Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup},
			},
			Suspend: true,
			Actions: []reactorv1alpha1.Action{notifyWith("failed over from {{ .From }}")},
			OnExit:  []reactorv1alpha1.Action{notifyWith("recovered, carrier {{ .State.isp }}")},
		})

		observe(map[string]string{keyWAN: wanBackup})
		reconcileOnce(name)
		Expect(readyOf(name).Reason).To(Equal(reasonTemplateWillNotRender))
		Expect(readyOf(name).Message).To(ContainSubstring("spec.onExit[0].notification.message"))
	})
})

func ptrTo[T any](value T) *T { return &value }
