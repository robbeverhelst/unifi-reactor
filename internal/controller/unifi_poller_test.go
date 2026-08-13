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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
)

func automation(name, provider, eventType string) *reactorv1alpha1.Automation {
	a := &reactorv1alpha1.Automation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	if eventType != "" {
		a.Spec.Trigger = &reactorv1alpha1.EventTrigger{Provider: provider, Event: eventType}
		return a
	}
	a.Spec.When = &reactorv1alpha1.StateTrigger{Provider: provider, State: map[string]string{"wan": wanBackupValue}}
	return a
}

// A state change must reach the reconciler immediately. Without this the only
// path back into Reconcile is the periodic requeue, which adds up to a full
// re-evaluation interval of latency to every reaction.
func TestPollerWakesOnlyItsOwnStateAutomations(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := reactorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding reactor scheme: %v", err)
	}

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		automation("unifi-wan", "unifi", ""),
		automation("unifi-ups", "unifi", ""),
		automation("nut-ups", "nut", ""),          // different provider
		automation("unifi-event", "unifi", "x.y"), // event trigger, not state
	).Build()

	wake := make(chan event.GenericEvent, 16)
	poller := &UniFiPoller{Reader: reader, Events: wake}
	poller.wake(context.Background())
	close(wake)

	var woken []string
	for e := range wake {
		woken = append(woken, e.Object.GetName())
	}
	if len(woken) != 2 {
		t.Fatalf("expected 2 automations woken, got %v", woken)
	}
	for _, name := range woken {
		if name != "unifi-wan" && name != "unifi-ups" {
			t.Errorf("unexpected automation woken: %s", name)
		}
	}
}

func TestPollerWakeIsANoOpWithoutWiring(t *testing.T) {
	// Must not panic when the channel or reader is absent.
	(&UniFiPoller{}).wake(context.Background())
}

func TestPollerWakeDropsRatherThanBlocking(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := reactorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding reactor scheme: %v", err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		automation("a", "unifi", ""),
		automation("b", "unifi", ""),
	).Build()

	full := make(chan event.GenericEvent, 1)
	poller := &UniFiPoller{Reader: reader, Events: full}

	done := make(chan struct{})
	go func() {
		poller.wake(context.Background())
		close(done)
	}()
	<-done // would hang if a saturated queue blocked observation
}
