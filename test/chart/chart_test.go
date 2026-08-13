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
	crdKind        = "kind: CustomResourceDefinition"
	crdName        = "automations.reactor.robbeverhelst.com"
	keepPolicy     = "helm.sh/resource-policy: keep"
	deploymentKind = "kind: Deployment"
	// A UniFi URL has to be set for the provider — and so its credentials —
	// to be part of the rendered Deployment at all.
	unifiURL = "unifi.url=https://192.0.2.1"
)

func chartDir() string { return filepath.Join("..", "..", "charts", "reactor") }

// render runs `helm template` the way a user's install or upgrade would, with
// each value given as it would be on the command line, and returns every
// manifest the release contains.
func render(t *testing.T, values ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed; skipping chart render tests")
	}
	args := make([]string, 0, len(values)*2+3)
	args = append(args, "template", "reactor", chartDir())
	for _, value := range values {
		args = append(args, "--set", value)
	}
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
	manifests := render(t, "crds.install=false")
	if strings.Contains(manifests, crdKind) {
		t.Fatal("crds.install=false still rendered a CustomResourceDefinition")
	}
	if !strings.Contains(manifests, deploymentKind) {
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

// TestLogLevelIsSettable covers the knob that makes the V(1) observation lines
// reachable: before, args were hardcoded and debug meant hand-editing a
// Deployment that Helm would overwrite on the next upgrade.
func TestLogLevelIsSettable(t *testing.T) {
	if got := render(t, "log.level=debug", "log.format=json"); !strings.Contains(got, "--zap-log-level=debug") ||
		!strings.Contains(got, "--zap-encoder=json") {
		t.Fatal("log.level / log.format did not reach the manager's arguments")
	}
	if got := render(t); !strings.Contains(got, "--zap-log-level=info") {
		t.Fatal("the default log level is not passed at all, leaving it at the binary's own default")
	}
}

// TestCredentialsAreMountedForRotation is the rotation contract at the chart
// level: the key arrives as a file the kubelet keeps up to date, not as an
// environment variable frozen at pod start.
func TestCredentialsAreMountedForRotation(t *testing.T) {
	manifests := render(t, unifiURL)
	if !strings.Contains(manifests, "name: UNIFI_API_KEY_FILE") {
		t.Fatal("the operator is not told to read its API key from a file")
	}
	if strings.Contains(manifests, "secretKeyRef") {
		t.Fatal("the API key is still injected from a Secret at startup, which rotation cannot reach")
	}
	// A subPath mount is never updated when the Secret changes, which would
	// silently reinstate the old behaviour.
	if strings.Contains(manifests, "subPath:") {
		t.Fatal("the credentials volume uses subPath, so a rotated Secret would never reach the container")
	}
	if !strings.Contains(manifests, "secretName: \"unifi-reactor-credentials\"") {
		t.Fatal("the credentials Secret is not mounted")
	}
}

// TestOperationalExtrasAreOptOut keeps upgrades boring: an existing install
// that does not ask for them must render exactly what it rendered before.
func TestOperationalExtrasAreOptOut(t *testing.T) {
	defaults := render(t, unifiURL)
	for _, kind := range []string{"kind: PodDisruptionBudget", "kind: NetworkPolicy"} {
		if strings.Contains(defaults, kind) {
			t.Errorf("%q is rendered by default; enabling it must be the user's choice", kind)
		}
	}

	pdb := render(t, unifiURL, "podDisruptionBudget.enabled=true", "podDisruptionBudget.minAvailable=2")
	if !strings.Contains(pdb, "kind: PodDisruptionBudget") || !strings.Contains(pdb, "minAvailable: 2") {
		t.Error("podDisruptionBudget.enabled did not produce a budget")
	}
	netpol := render(t, unifiURL, "networkPolicy.enabled=true")
	if !strings.Contains(netpol, "kind: NetworkPolicy") {
		t.Error("networkPolicy.enabled did not produce a policy")
	}
}

// TestWebhookFastPathIsOptOut is the upgrade guarantee for the fast path: an
// existing install that does not ask for it gains no listening port, no
// Service, and no second credential.
func TestWebhookFastPathIsOptOut(t *testing.T) {
	defaults := render(t, unifiURL)
	for _, absent := range []string{"UNIFI_WEBHOOK_ENABLED", "containerPort: 9090", "-webhook"} {
		if strings.Contains(defaults, absent) {
			t.Errorf("a default install renders %q; the webhook must be opt-in", absent)
		}
	}

	enabled := render(t, unifiURL, "unifi.webhook.enabled=true")
	for _, present := range []string{"UNIFI_WEBHOOK_ENABLED", "UNIFI_WEBHOOK_TOKEN", "containerPort: 9090"} {
		if !strings.Contains(enabled, present) {
			t.Errorf("unifi.webhook.enabled did not render %q", present)
		}
	}
	// A ClusterIP cannot be reached by a console outside the cluster, which is
	// the point: exposing the receiver stays the operator's explicit decision.
	if !strings.Contains(enabled, "type: ClusterIP") {
		t.Error("the webhook Service is not a ClusterIP by default")
	}
	// Self-registration writes to the user's gateway, so it needs its own yes.
	if strings.Contains(enabled, "UNIFI_WEBHOOK_REGISTER") {
		t.Error("enabling the receiver also enabled self-registration")
	}
}

// TestWebhookMisconfigurationsFailAtInstall covers the combinations that
// cannot work, where rendering something plausible would mean debugging a
// silent no-op later instead.
func TestWebhookMisconfigurationsFailAtInstall(t *testing.T) {
	for name, values := range map[string][]string{
		"receiver without a console":    {"unifi.webhook.enabled=true"},
		"registration without receiver": {unifiURL, "unifi.webhook.registration.enabled=true"},
		"registration without a public URL": {unifiURL, "unifi.webhook.enabled=true",
			"unifi.webhook.registration.enabled=true"},
	} {
		t.Run(name, func(t *testing.T) {
			if renderFails(t, values...) == "" {
				t.Error("expected the chart to refuse this combination")
			}
		})
	}
}

// renderFails returns the error output of a render expected to fail, or "" if
// it unexpectedly succeeded.
func renderFails(t *testing.T, values ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed; skipping chart render tests")
	}
	args := make([]string, 0, len(values)*2+3)
	args = append(args, "template", "reactor", chartDir())
	for _, value := range values {
		args = append(args, "--set", value)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil {
		return ""
	}
	return string(out)
}
