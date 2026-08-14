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

package adopt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	crdName   = "automations.reactor.robbeverhelst.com"
	release   = "reactor"
	namespace = "reactor-system"

	// otherRelease and otherNamespace stand for somebody else's release: an
	// umbrella chart, or a platform team's CRD bundle. Nothing here may take a
	// CRD from it.
	otherRelease   = "platform-crds"
	otherNamespace = "platform"
)

// chartCRD is the shape of what the chart mounts alongside the hook: the
// rendered template, leading document separator and all, carrying a schema the
// live CRD does not have yet.
const chartCRD = `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    helm.sh/resource-policy: keep
  name: automations.reactor.robbeverhelst.com
spec:
  group: reactor.robbeverhelst.com
  names:
    kind: Automation
    listKind: AutomationList
    plural: automations
    singular: automation
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                reversal: {type: string}
`

// scheme teaches the fake client the one kind this package touches. It is
// registered as unstructured deliberately: the hook reads and patches metadata
// and a spec it never interprets, so it needs no typed CRD API and no
// dependency on the apiextensions client.
func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(crdGroupVersionKind, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(crdGroupVersionKind.GroupVersion().WithKind(crdKind+"List"), &unstructured.UnstructuredList{})
	return s
}

// liveCRD is the CRD as the crds/ packaging left it: applied by Helm, recorded
// as part of no release, and serving the old schema.
func liveCRD(annotations map[string]string) *unstructured.Unstructured {
	crd := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"group": "reactor.robbeverhelst.com",
			"versions": []any{map[string]any{
				"name": "v1alpha1", "served": true, "storage": true,
			}},
		},
	}}
	crd.SetGroupVersionKind(crdGroupVersionKind)
	crd.SetName(crdName)
	if annotations != nil {
		crd.SetAnnotations(annotations)
	}
	return crd
}

func manifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crds.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the chart's CRD: %v", err)
	}
	return path
}

func options(path string) Options {
	return Options{Name: crdName, Release: release, Namespace: namespace, ManifestPath: path}
}

func readBack(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGroupVersionKind)
	if err := c.Get(context.Background(), client.ObjectKey{Name: crdName}, crd); err != nil {
		t.Fatalf("reading the CRD back: %v", err)
	}
	return crd
}

// TestAdoptsACRDOwnedByNobody is the upgrade every install from chart 0.3.0 or
// earlier goes through exactly once: Helm's three ownership keys are set, and
// the schema the chart ships goes live in the same patch — because on that
// upgrade the chart leaves the CRD out of the release, so nothing else would.
func TestAdoptsACRDOwnedByNobody(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(liveCRD(nil)).Build()

	if err := CRD(context.Background(), c, options(manifest(t, chartCRD))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	adopted := readBack(t, c)
	if got := adopted.GetLabels()[managedByLabel]; got != managedByHelm {
		t.Errorf("%s = %q, want %q, or Helm still refuses to update the CRD", managedByLabel, got, managedByHelm)
	}
	annotations := adopted.GetAnnotations()
	if annotations[releaseNameAnnotation] != release || annotations[releaseNamespaceAnnotation] != namespace {
		t.Errorf("release annotations = %v, want the release being installed", annotations)
	}
	if !strings.Contains(schemaOf(t, adopted), "reversal") {
		t.Error("the chart's schema was not put live, so the operator would expect fields the API server lacks")
	}
}

// schemaOf renders the CRD's versions back to something searchable, so a test
// can ask whether a field the chart ships is now part of the live schema
// without walking the whole openAPIV3Schema by hand.
func schemaOf(t *testing.T, crd *unstructured.Unstructured) string {
	t.Helper()
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || !found {
		t.Fatalf("the CRD has no versions: %v", err)
	}
	return strings.Join(nestedKeys(versions), " ")
}

// nestedKeys flattens every map key and string value it can reach, which is
// enough to answer "is this field in the schema" without a typed CRD.
func nestedKeys(value any) []string {
	var keys []string
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			keys = append(keys, key)
			keys = append(keys, nestedKeys(nested)...)
		}
	case []any:
		for _, nested := range typed {
			keys = append(keys, nestedKeys(nested)...)
		}
	case string:
		keys = append(keys, typed)
	}
	return keys
}

// TestRefusesACRDOwnedByAnotherRelease is the whole reason this is conservative.
// Adopting is taking ownership of something, and a CRD another release installed
// is that release's — taking it would leave the other release unable to update
// its own object.
func TestRefusesACRDOwnedByAnotherRelease(t *testing.T) {
	owned := liveCRD(map[string]string{
		releaseNameAnnotation:      otherRelease,
		releaseNamespaceAnnotation: otherNamespace,
	})
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(owned).Build()

	err := CRD(context.Background(), c, options(manifest(t, chartCRD)))
	if err == nil {
		t.Fatal("a CRD owned by another release was adopted; the upgrade must fail instead")
	}
	// Naming what it found is the point: an upgrade that stops has to say
	// which release it stopped for.
	for _, expected := range []string{otherRelease, otherNamespace, crdName} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the error does not name %q: %v", expected, err)
		}
	}
	if annotations := readBack(t, c).GetAnnotations(); annotations[releaseNameAnnotation] != otherRelease {
		t.Error("the CRD was modified despite belonging to another release")
	}
}

// TestLeavesAnAlreadyAdoptedCRDAlone keeps the hook safe to run on every
// upgrade: the second one has nothing to do, and doing nothing must not mean
// overwriting the schema Helm itself now maintains.
func TestLeavesAnAlreadyAdoptedCRDAlone(t *testing.T) {
	owned := liveCRD(map[string]string{
		releaseNameAnnotation:      release,
		releaseNamespaceAnnotation: namespace,
	})
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(owned).Build()

	if err := CRD(context.Background(), c, options(manifest(t, chartCRD))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(schemaOf(t, readBack(t, c)), "reversal") {
		t.Error("a CRD this release already owns was patched; Helm applies the schema on that path")
	}
}

// TestSucceedsWithNoCRDToAdopt is the fresh-install path. There is nothing to
// take over, and the release installs its own CRD moments later.
func TestSucceedsWithNoCRDToAdopt(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()

	if err := CRD(context.Background(), c, options(manifest(t, chartCRD))); err != nil {
		t.Fatalf("an install with no CRD to adopt failed: %v", err)
	}
}

// TestAdoptsOwnershipWithoutAManifest covers the narrower use the hook does not
// take: ownership alone, leaving the live schema exactly as it was.
func TestAdoptsOwnershipWithoutAManifest(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(liveCRD(nil)).Build()

	if err := CRD(context.Background(), c, Options{Name: crdName, Release: release, Namespace: namespace}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	adopted := readBack(t, c)
	if adopted.GetAnnotations()[releaseNameAnnotation] != release {
		t.Error("the CRD was not adopted")
	}
	if strings.Contains(schemaOf(t, adopted), "reversal") {
		t.Error("the schema changed without a manifest to change it to")
	}
}

// TestRejectsAManifestForSomethingElse guards the input that would be worst to
// act on: patching one CRD's schema from another object's spec.
func TestRejectsAManifestForSomethingElse(t *testing.T) {
	for name, content := range map[string]string{
		"another CRD": strings.Replace(chartCRD, crdName, "widgets.example.com", 1),
		"another kind": strings.Replace(chartCRD, "kind: CustomResourceDefinition",
			"kind: ConfigMap", 1),
		"two documents":  chartCRD + chartCRD,
		"nothing at all": "# just a comment\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(liveCRD(nil)).Build()

			if err := CRD(context.Background(), c, options(manifest(t, content))); err == nil {
				t.Fatal("expected the hook to refuse the manifest rather than patch from it")
			}
			if readBack(t, c).GetAnnotations()[releaseNameAnnotation] != "" {
				t.Error("the CRD was adopted despite the manifest being refused")
			}
		})
	}
}

// TestRequiresTheReleaseItAdoptsInto stops the hook running with a release
// identity the chart failed to render: adopting into an empty release name
// writes ownership metadata that matches nothing, which Helm reads as a
// conflict it cannot resolve.
func TestRequiresTheReleaseItAdoptsInto(t *testing.T) {
	for name, opts := range map[string]Options{
		"no CRD":       {Release: release, Namespace: namespace},
		"no release":   {Name: crdName, Namespace: namespace},
		"no namespace": {Name: crdName, Release: release},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(liveCRD(nil)).Build()
			if err := CRD(context.Background(), c, opts); err == nil {
				t.Fatal("expected an error rather than an adoption into an incomplete release")
			}
		})
	}
}
