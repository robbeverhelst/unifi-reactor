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

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
)

// An Automation written against the old schema — spec.trigger, no spec.when —
// outlives the field's removal in etcd. It must stay inert: it never claimed a
// target and it must not start now, and it must not be written to either,
// because spec.when is required and the API server would reject the update.
//
// A fake client is used deliberately: the CRD no longer admits an object like
// this, so envtest cannot create one.
func TestALeftOverEventTriggerAutomationStaysInert(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := reactorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding reactor scheme: %v", err)
	}

	replicas := int32(3)
	target := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "qbittorrent", Namespace: testNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	stale := legacyAutomation("notify-on-client-connect")
	stale.Spec.Actions = []reactorv1alpha1.Action{{
		Type:     actionKubernetesScale,
		Target:   &reactorv1alpha1.TargetRef{Kind: "Deployment", Name: "qbittorrent"},
		Replicas: new(int32),
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(stale, target).
		WithStatusSubresource(&reactorv1alpha1.Automation{}).
		Build()
	reconciler := &AutomationReconciler{Client: c, Scheme: scheme, Store: engine.NewStateStore()}

	ctx := context.Background()
	key := types.NamespacedName{Name: stale.Name, Namespace: stale.Namespace}
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconciling a left-over event trigger: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for an inert automation, got %+v", result)
	}

	var reconciled reactorv1alpha1.Automation
	if err := c.Get(ctx, key, &reconciled); err != nil {
		t.Fatalf("getting automation: %v", err)
	}
	if len(reconciled.Status.Conditions) != 0 {
		t.Errorf("status was written to an object the API server would reject: %+v", reconciled.Status.Conditions)
	}
	if len(reconciled.Finalizers) != 0 {
		t.Errorf("an automation that claims nothing must not carry a finalizer: %v", reconciled.Finalizers)
	}

	var untouched appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: target.Namespace}, &untouched); err != nil {
		t.Fatalf("getting target: %v", err)
	}
	if *untouched.Spec.Replicas != replicas {
		t.Errorf("target was scaled to %d by an automation that never ran", *untouched.Spec.Replicas)
	}
}
