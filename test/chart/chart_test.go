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

// Package chart tests what the Helm chart renders.
//
// These are render-time tests: they prove the CRD is part of the release (so
// `helm upgrade` applies it) rather than a crds/ file Helm would install once
// and never touch again. Proving the upgrade end to end — install an old
// chart, upgrade, read the live schema back from the API server — needs a real
// cluster and belongs with the chart e2e work in #35.
package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	crdKind    = "kind: CustomResourceDefinition"
	crdName    = "automations.reactor.robbeverhelst.com"
	keepPolicy = "helm.sh/resource-policy: keep"
)

func chartDir() string { return filepath.Join("..", "..", "charts", "reactor") }

// render runs `helm template` the way a user's install or upgrade would, and
// returns every manifest the release contains.
func render(t *testing.T, values ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed; skipping chart render tests")
	}
	args := append([]string{"template", "reactor", chartDir()}, values...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestCRDIsPartOfTheRelease is the regression test for the packaging trap:
// a CRD under crds/ is installed once and silently never upgraded, so every
// later schema change would leave the operator expecting fields the API server
// does not have.
func TestCRDIsPartOfTheRelease(t *testing.T) {
	manifests := render(t)
	if !strings.Contains(manifests, crdKind) || !strings.Contains(manifests, crdName) {
		t.Fatalf("the %s CRD is not rendered by the chart, so helm upgrade would not update it", crdName)
	}
	if !strings.Contains(manifests, keepPolicy) {
		t.Fatalf("the CRD is missing %q, so helm uninstall would delete it and every Automation with it", keepPolicy)
	}
}

// TestNoCRDsDirectory guards the same trap from the other side: reintroducing
// charts/reactor/crds/ would hand Helm a CRD it installs once and forgets,
// and would collide with the templated one on install.
func TestNoCRDsDirectory(t *testing.T) {
	if _, err := os.Stat(filepath.Join(chartDir(), "crds")); !os.IsNotExist(err) {
		t.Fatalf("charts/reactor/crds/ exists again; Helm installs that directory once and never upgrades it")
	}
}

// TestCRDCanBeManagedOutsideTheRelease covers the documented manual path, for
// clusters where CRDs are applied by an admin or by GitOps.
func TestCRDCanBeManagedOutsideTheRelease(t *testing.T) {
	manifests := render(t, "--set", "crds.install=false")
	if strings.Contains(manifests, crdKind) {
		t.Fatal("crds.install=false still rendered a CustomResourceDefinition")
	}
	if !strings.Contains(manifests, "kind: Deployment") {
		t.Fatal("crds.install=false dropped the operator itself")
	}
}

// TestChartCRDMatchesGenerated fails when the chart's CRD was hand-edited or
// `make manifests` was not run: the chart copy is generated from
// config/crd/bases by hack/sync-chart-crds.sh.
func TestChartCRDMatchesGenerated(t *testing.T) {
	generated := readSpec(t, filepath.Join("..", "..", "config", "crd", "bases",
		"reactor.robbeverhelst.com_automations.yaml"))
	inChart := readSpec(t, filepath.Join(chartDir(), "templates", "crds.yaml"))
	if generated != inChart {
		t.Fatal("the chart's CRD differs from config/crd/bases; run `make manifests`")
	}
}

// readSpec returns the CRD's spec block — everything from the top-level `spec:`
// on — which is the part that has to stay identical between the generated CRD
// and the chart's templated copy. The metadata differs by design: the chart
// copy carries the Helm keep policy.
func readSpec(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	_, spec, found := strings.Cut(string(b), "\nspec:\n")
	if !found {
		t.Fatalf("no top-level spec block in %s", path)
	}
	// The chart copy ends with the Helm guard; the generated one does not.
	spec, _, _ = strings.Cut(spec, "\n{{- end }}")
	return strings.TrimSpace(spec)
}
