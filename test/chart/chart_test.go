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
// and never touch again. The upgrade itself — install the old packaging,
// adopt the CRD, upgrade, and read the live schema back from the API server —
// is proven end to end by test/e2e/lifecycle, which needs a real cluster.
package chart

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// envAllowedDestinations and secretsRule are how the outbound-action
	// allowlist and the permission it implies appear in the rendered release.
	envAllowedDestinations = "REACTOR_ACTION_ALLOWED_DESTINATIONS"
	secretsRule            = `resources: ["secrets"]`
	// tokenReviews is how the metrics endpoint's authn/authz filter appears in
	// the rendered RBAC.
	tokenReviews = "tokenreviews"
	// nodesRule is how kubernetes.cordon's opt-in permission appears.
	nodesRule = `resources: ["nodes"]`
	// clusterWide and namespaced are the two RBAC modes. Anything the operator
	// is granted has to be checked in both, because they render as different
	// object kinds from the same rule list.
	clusterWide    = "rbac.clusterWide=true"
	namespacedRBAC = "rbac.clusterWide=false"
)

// rbacModes is both RBAC modes, for the rules that must travel with either.
var rbacModes = []string{clusterWide, namespacedRBAC}

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

// celReservedWords are the identifiers CEL reserves. Kubernetes exposes a
// schema property whose name collides with one only under an escaped name
// (__namespace__ for namespace), and a rule that spells it plainly does not
// fail validation — it fails to COMPILE, which the API server reports by
// rejecting the entire CustomResourceDefinition.
//
// See https://kubernetes.io/docs/reference/using-api/cel/#escaping.
var celReservedWords = []string{
	"as", "break", "const", "continue", "else", "false", "for", "function",
	"if", "import", "in", "let", "loop", "namespace", "null", "package",
	"return", "true", "var", "void", "while",
}

// stringLiteral matches a CEL single-quoted literal, which is stripped before
// scanning so that a reserved word inside a message or a compared value —
// 'notification.' has no reserved word, but a future rule's might — cannot be
// mistaken for a field select.
var stringLiteral = regexp.MustCompile(`'[^']*'`)

// TestCELRulesEscapeReservedWords guards a failure mode that no other test in
// this repository can reach.
//
// A CEL rule is compiled by the API server and nowhere else. Get one wrong and
// the CRD is rejected whole — every field, not just the offending rule — so an
// install or upgrade fails outright and takes the operator down with it. Go
// vet, the unit suites and `helm template` all pass regardless, because none of
// them compiles CEL. This test cannot compile it either, but it can check the
// one mistake that is easy to make and expensive to discover: selecting a
// property whose name is a CEL reserved word without escaping it.
//
// Found the hard way. `has(self.namespace)` on TargetRef was correct-looking,
// passed everything local, and was rejected by the API server the moment the
// e2e suite tried to install the chart.
func TestCELRulesEscapeReservedWords(t *testing.T) {
	crd, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases",
		"reactor.robbeverhelst.com_automations.yaml"))
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}

	rules := regexp.MustCompile(`(?m)^\s*(?:- )?rule: (.*)$`).FindAllStringSubmatch(string(crd), -1)
	if len(rules) == 0 {
		t.Fatal("no x-kubernetes-validations rules found; this test is checking nothing")
	}
	for _, match := range rules {
		rule := stringLiteral.ReplaceAllString(match[1], "''")
		for _, word := range celReservedWords {
			unescaped := regexp.MustCompile(`\.` + word + `\b`)
			if unescaped.MatchString(rule) {
				t.Errorf("rule selects the CEL reserved word %q unescaped, so the API server "+
					"will refuse the whole CRD; write __%s__ instead:\n  %s", word, word, match[1])
			}
		}
	}
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

// TestNamespacedRBACScopesTheWatch covers the pairing that makes
// rbac.clusterWide=false usable at all. A namespaced Role grants no
// cluster-wide list, so an operator left watching every namespace is refused
// at every list and reconciles nothing — while its health probes, which only
// ping, keep reporting it ready.
func TestNamespacedRBACScopesTheWatch(t *testing.T) {
	scoped := render(t, unifiURL, namespacedRBAC)
	if !strings.Contains(scoped, "name: WATCH_NAMESPACE") {
		t.Fatal("namespace-scoped RBAC did not tell the operator which namespace it may watch")
	}
	if strings.Contains(scoped, "kind: ClusterRole") {
		t.Fatal("rbac.clusterWide=false still rendered cluster-scoped RBAC")
	}
	if wide := render(t, unifiURL); strings.Contains(wide, "name: WATCH_NAMESPACE") {
		t.Fatal("a cluster-wide install was restricted to one namespace")
	}
}

// TestSecretAccessFollowsTheGrantedScope pins the corner where two independent
// switches meet. Whether the Secret rule is granted at all is covered by the
// outbound-action tests; this is about *which* role carries it, because
// allowing a destination must not quietly widen a namespaced install into
// cluster-wide read access to every Secret in the cluster.
func TestSecretAccessFollowsTheGrantedScope(t *testing.T) {
	const destination = "actions.allowedDestinations={https://ntfy.example.com}"

	namespaced := render(t, unifiURL, namespacedRBAC, destination)
	if strings.Contains(namespaced, "kind: ClusterRole") {
		t.Fatal("allowing a destination widened a namespaced install to cluster-scoped RBAC")
	}
	scoped, found := managerRules(namespaced, "kind: Role")
	if !found {
		t.Fatal("no manager Role rendered for a namespaced install")
	}
	if !strings.Contains(scoped, secretsRule) {
		t.Error("a namespaced install allowing a destination cannot read the Secret it authenticates with")
	}

	// The same switch, off, in the mode the outbound-action tests do not cover.
	if off := render(t, unifiURL, namespacedRBAC); strings.Contains(off, secretsRule) {
		t.Error("a namespaced install that allows no destination was still granted read access to Secrets")
	}

	// And cluster-wide, the rule belongs to the manager's own ClusterRole
	// rather than to some other document that happens to mention Secrets.
	wide, found := managerRules(render(t, unifiURL, destination), "kind: ClusterRole")
	if !found {
		t.Fatal("no manager ClusterRole rendered for a cluster-wide install")
	}
	if !strings.Contains(wide, secretsRule) {
		t.Error("the Secret read was granted somewhere other than the manager's ClusterRole")
	}
}

// managerRules returns the rule block of the manager's Role or ClusterRole,
// so an assertion about what it permits cannot be satisfied by a different
// document in the release that happens to mention the same resource.
func managerRules(manifests, kind string) (string, bool) {
	for document := range strings.SplitSeq(manifests, "\n---") {
		// "rules:" is what tells the role apart from the binding that
		// references it, which names the same kind and the same object.
		if strings.Contains(document, kind) && strings.Contains(document, "-manager\n") &&
			strings.Contains(document, "\nrules:") {
			return document, true
		}
	}
	return "", false
}

// TestReleaseHookStopsTheOperator is the ordering the uninstall depends on.
// Helm removes the release's own resources only after its pre-delete hooks
// finish, so the hook has to stop the operator itself; one left running
// re-claims everything the hook released and re-adds the finalizer that by
// then has nothing to service it.
func TestReleaseHookStopsTheOperator(t *testing.T) {
	manifests := render(t, unifiURL)
	for _, wiring := range []string{"name: MANAGER_DEPLOYMENT", "name: MANAGER_NAMESPACE"} {
		if !strings.Contains(manifests, wiring) {
			t.Errorf("the pre-delete hook is not told %q, so it cannot stop the operator first", wiring)
		}
	}
	if disabled := render(t, unifiURL, "uninstall.releaseClaims=false"); strings.Contains(disabled, "kind: Job") {
		t.Error("uninstall.releaseClaims=false still rendered the release hook")
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

// TestOutboundActionsAreOffByDefault is the render-time half of the outbound
// story: with no destination allowed, the operator is not even granted read
// access to the Secrets those actions would authenticate with.
func TestOutboundActionsAreOffByDefault(t *testing.T) {
	manifests := render(t, unifiURL)
	if strings.Contains(manifests, envAllowedDestinations) {
		t.Fatal("an allowlist was rendered without one being configured")
	}
	if strings.Contains(manifests, secretsRule) {
		t.Fatal("read access to Secrets is granted by default; it should follow actions.allowedDestinations")
	}
}

// TestAllowedDestinationsGrantSecretReads pairs the two: configuring where
// outbound actions may go is what turns on the permission they need.
func TestAllowedDestinationsGrantSecretReads(t *testing.T) {
	manifests := render(t, unifiURL,
		"actions.allowedDestinations={https://ntfy.example.com,https://discord.com}")
	if !strings.Contains(manifests, `value: "https://ntfy.example.com,https://discord.com"`) {
		t.Fatal("the allowlist was not passed to the operator")
	}
	if !strings.Contains(manifests, secretsRule) {
		t.Fatal("outbound actions need get on Secrets, which was not granted")
	}
}

// TestOutboundActionsWorkWithoutAConsole proves the outbound actions are
// provider-agnostic at the packaging level too: an install with no UniFi
// console still gets its allowlist.
func TestOutboundActionsWorkWithoutAConsole(t *testing.T) {
	manifests := render(t, "actions.allowedDestinations={https://ntfy.example.com}")
	if !strings.Contains(manifests, envAllowedDestinations) {
		t.Fatal("the allowlist is only rendered when a UniFi console is configured")
	}
}

// TestMetricsAreOptIn is the upgrade guarantee for the metrics endpoint: an
// existing install that does not ask for it gains no listening port, no
// Service, and no new permission. The manifest bundle turns it on; the chart
// makes it a choice, because opening a port on a running operator is not
// something an upgrade should do on its own.
func TestMetricsAreOptIn(t *testing.T) {
	defaults := render(t, unifiURL)
	for _, absent := range []string{
		"--metrics-bind-address",
		"containerPort: 8443",
		"-metrics",
		tokenReviews,
		"kind: ServiceMonitor",
		"kind: PrometheusRule",
		"kind: GrafanaDashboard",
	} {
		if strings.Contains(defaults, absent) {
			t.Errorf("a default install renders %q; metrics must be opt-in", absent)
		}
	}

	enabled := render(t, unifiURL, "metrics.enabled=true")
	for _, present := range []string{
		"--metrics-bind-address=:8443",
		"containerPort: 8443",
		"kind: Service",
		tokenReviews,
		"subjectaccessreviews",
	} {
		if !strings.Contains(enabled, present) {
			t.Errorf("metrics.enabled did not render %q", present)
		}
	}
	// HTTPS behind the API server's authn/authz filter is the default posture,
	// matching what install.yaml has always done.
	if strings.Contains(enabled, "--metrics-secure=false") {
		t.Error("the metrics endpoint is served insecurely by default")
	}
	// Reading /metrics is granted by a ClusterRole with no binding: the chart
	// cannot know which ServiceAccount Prometheus scrapes with, and guessing
	// would produce a permission that looks granted and is not.
	if !strings.Contains(enabled, "nonResourceURLs: [\"/metrics\"]") {
		t.Error("no metrics-reader ClusterRole was rendered")
	}
	// One mention is the ClusterRole's own metadata.name; a second would be a
	// roleRef, meaning the chart guessed who scrapes.
	if got := strings.Count(enabled, "name: reactor-metrics-reader"); got != 1 {
		t.Errorf("metrics-reader is named %d times; binding it is the operator's choice, not the chart's", got)
	}
}

// TestMonitoringExtrasNeedTheEndpoint stops a release rendering a
// ServiceMonitor, alert rules or a dashboard that all query series nothing is
// publishing — which fails as silence rather than as an error.
func TestMonitoringExtrasNeedTheEndpoint(t *testing.T) {
	for name, value := range map[string]string{
		"service monitor": "metrics.serviceMonitor.enabled=true",
		"alert rules":     "metrics.rules.enabled=true",
		"dashboard":       "metrics.dashboard.enabled=true",
	} {
		t.Run(name, func(t *testing.T) {
			if renderFails(t, unifiURL, value) == "" {
				t.Error("expected the chart to refuse this without metrics.enabled")
			}
		})
	}
}

// TestAlertRulesCoverTheSilentFailure checks the one rule the whole feature
// exists for. Reactor fails open: when it stops observing, every Automation
// quietly stops reacting and nothing else in the cluster notices.
func TestAlertRulesCoverTheSilentFailure(t *testing.T) {
	manifests := render(t, unifiURL, "metrics.enabled=true", "metrics.rules.enabled=true")
	for _, alert := range []string{
		"ReactorObservationStale",
		"ReactorObservationAbsent",
		"ReactorObservationFailing",
		"ReactorActionFailing",
		"ReactorEdgeActionFailing",
		"ReactorAutomationNotReady",
		"ReactorReactionSlow",
		"ReactorUPSOnBattery",
		"ReactorWANOnBackup",
	} {
		if !strings.Contains(manifests, alert) {
			t.Errorf("alert %s is missing", alert)
		}
	}
	if !strings.Contains(manifests, "time() - reactor_last_observation_timestamp_seconds > 90") {
		t.Error("the staleness threshold did not reach the rule")
	}
	// Prometheus templating must survive Helm rendering rather than being
	// evaluated by it — an annotation reading "$labels.provider" as an empty
	// string is the classic way this ships broken.
	if !strings.Contains(manifests, "{{ $labels.provider }}") {
		t.Error("Prometheus label templating was rendered away by Helm")
	}
	// A failed notification is not a failed Automation, and no alert may imply
	// it is: the two are separated by the kind label.
	if !strings.Contains(manifests, `kind="edge"`) || !strings.Contains(manifests, `kind="desired_state"`) {
		t.Error("the action alerts do not distinguish edge actions from desired-state ones")
	}
}

// TestDashboardCarriesNothingSiteSpecific is the leak guard. A dashboard JSON
// is exactly where a datasource UID, an org-specific folder, or somebody's
// Grafana address ends up without anyone noticing.
func TestDashboardCarriesNothingSiteSpecific(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir(), "dashboards", "reactor.json"))
	if err != nil {
		t.Fatalf("reading the dashboard: %v", err)
	}

	var dashboard map[string]any
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("the dashboard is not valid JSON: %v", err)
	}
	// Every datasource reference must go through the dashboard's own variable,
	// so importing it into any Grafana works without editing it first.
	for _, uid := range regexp.MustCompile(`"uid"\s*:\s*"([^"]*)"`).FindAllStringSubmatch(string(raw), -1) {
		if uid[1] != "${datasource}" && uid[1] != "unifi-reactor-overview" {
			t.Errorf("the dashboard pins a datasource uid %q", uid[1])
		}
	}
	for _, leak := range []string{"192.168.", "10.0.", "grafana.com/orgs", "http://localhost", ".local"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the dashboard contains %q, which is somebody's infrastructure", leak)
		}
	}

	// And it has to survive being carried through a Helm template into a CR.
	manifests := render(t, unifiURL, "metrics.enabled=true", "metrics.dashboard.enabled=true")
	if !strings.Contains(manifests, "kind: GrafanaDashboard") {
		t.Fatal("metrics.dashboard.enabled did not render a GrafanaDashboard")
	}
	// Grafana legend formats are {{key}}, which Helm would happily evaluate to
	// nothing if the JSON were ever passed through tpl.
	if !strings.Contains(manifests, "{{provider}}") {
		t.Error("the dashboard's legend templating was rendered away by Helm")
	}
}

// TestEventsAreGrantedOnTheRightAPIGroup guards a rule that fails silently when
// it is wrong. The manager records through events.k8s.io/v1; a rule naming only
// the core group is refused on every emission, logged by the broadcaster and
// surfaced nowhere, so the Automation simply has no Events.
func TestEventsAreGrantedOnTheRightAPIGroup(t *testing.T) {
	// Both RBAC modes: the manager's rules render as a ClusterRole in one and a
	// Role in the other, and the events rule travels with them either way.
	for _, mode := range rbacModes {
		t.Run(mode, func(t *testing.T) {
			manifests := render(t, unifiURL, mode)
			if !strings.Contains(manifests, `apiGroups: ["events.k8s.io"]`) {
				t.Fatal("the manager is not granted events on events.k8s.io, so every Event it raises is refused")
			}
			// Leader election still uses the deprecated core/v1 recorder, and its
			// own Role grants that in the release namespace. Exactly one rule each.
			if got := strings.Count(manifests, `resources: ["events"]`); got != 2 {
				t.Errorf("expected one events rule for the manager and one for leader election, found %d", got)
			}
		})
	}
}

// TestTargetKindsAreGrantedInBothRBACModes checks the rule that turns a
// supported action into one that works.
//
// Every desired-state action targets a kind, and a kind the operator has not
// been granted fails at the write rather than at admission — during the outage
// the automation existed for. The verbs are asserted too, because targets are
// read uncached: get to read and patch to write is the whole grant, and a
// widening back to list or watch would put an informer over every object of
// that kind in the operator's memory.
func TestTargetKindsAreGrantedInBothRBACModes(t *testing.T) {
	// The object a target is read from and its annotations written to, then the
	// scale subresource a replica count is read from and written through.
	rules := map[string]string{
		`resources: \["deployments", "statefulsets"\]`:             `verbs: \["get", "patch"\]`,
		`resources: \["deployments/scale", "statefulsets/scale"\]`: `verbs: \["get", "update"\]`,
		`resources: \["cronjobs"\]`:                                `verbs: \["get", "patch"\]`,
	}
	for _, mode := range rbacModes {
		t.Run(mode, func(t *testing.T) {
			manifests := render(t, unifiURL, mode)
			for resources, verbs := range rules {
				if !regexp.MustCompile(resources + `\n\s+` + verbs).MatchString(manifests) {
					t.Errorf("%s is not granted exactly %s", resources, verbs)
				}
			}
		})
	}
}

// TestHPADetectionIsOptInAndReadOnly pins both halves of the one permission
// added for noticing a controller Reactor cannot arbitrate with.
//
// Absent by default in both modes, because it is a permission nothing else here
// needs and an install that does not use the feature should not carry it. And
// when it is on it must be a read: Reactor declines to act on an HPA-managed
// target and deliberately does not suspend the HPA to win, so any write verb
// here would be a capability nothing in the code asks for.
func TestHPADetectionIsOptInAndReadOnly(t *testing.T) {
	const hpaRule = `resources: \["horizontalpodautoscalers"\]`

	for _, mode := range rbacModes {
		t.Run(mode, func(t *testing.T) {
			off := render(t, unifiURL, mode)
			if regexp.MustCompile(hpaRule).MatchString(off) {
				t.Error("autoscaling read is granted by default; it must be opt-in")
			}
			if strings.Contains(off, "--detect-hpa") {
				t.Error("detection is enabled by default, which changes what an existing install does")
			}

			on := render(t, unifiURL, mode, "safety.detectHPA=true")
			granted := regexp.MustCompile(hpaRule + `\n\s+(verbs: \[[^]]*\])`).FindStringSubmatch(on)
			if granted == nil {
				t.Fatal("safety.detectHPA does not grant autoscaling read, so every claim would fail")
			}
			if granted[1] != `verbs: ["list"]` {
				t.Errorf("HPA detection is granted %s; it lists one namespace and needs nothing else", granted[1])
			}
			if !strings.Contains(on, "- --detect-hpa") {
				t.Error("safety.detectHPA grants the permission without turning the behaviour on")
			}
		})
	}
}

// TestDryRunCannotWriteToATarget is what turns "it will not touch your
// workloads" into something the API server enforces.
//
// --dry-run is the operator promising not to write. This is the second lock:
// with safety.dryRun on, the manager's rules must carry no verb that could
// change a target — no patch on the workload kinds, no update on their scale
// subresources, and none on nodes if node actions were also turned on. Reads
// stay, because a dry run that could not read could not report.
//
// Written as an assertion about verbs rather than about rules, because the
// failure it guards against is a rule quietly widening back, not disappearing.
func TestDryRunCannotWriteToATarget(t *testing.T) {
	const dryRun = "safety.dryRun=true"
	writeVerbs := regexp.MustCompile(`verbs: \[[^]]*"(patch|update|create|delete)"`)

	for _, mode := range rbacModes {
		t.Run(mode, func(t *testing.T) {
			manifests := render(t, unifiURL, mode, dryRun, "rbac.allowNodeActions=true")
			for _, rule := range []string{
				`resources: \["deployments", "statefulsets"\]`,
				`resources: \["deployments/scale", "statefulsets/scale"\]`,
				`resources: \["cronjobs"\]`,
				`resources: \["nodes"\]`,
			} {
				granted := regexp.MustCompile(rule + `\n\s+(verbs: \[[^]]*\])`).FindStringSubmatch(manifests)
				if granted == nil {
					t.Errorf("%s is not granted at all, so a dry run could not even read it", rule)
					continue
				}
				if writeVerbs.MatchString(granted[1]) {
					t.Errorf("a dry-run install is granted %s on %s, so it could still write to a target",
						granted[1], rule)
				}
			}
			// The pre-delete sweep hands claims back, which is a write. A dry
			// run never took one, so the Job would have nothing to release and
			// no permission to release it with.
			if strings.Contains(manifests, "--release-claims") {
				t.Error("a dry-run install renders the release-claims hook, which cannot write and has nothing to hand back")
			}
			if !strings.Contains(manifests, "- --dry-run") {
				t.Error("safety.dryRun does not pass --dry-run to the manager, so it would fail every write with a Forbidden")
			}
		})
	}
}

// TestNodeActionsAreOptIn pins the gate on the one permission Reactor asks for
// that reaches outside the workloads it was installed to manage.
//
// Two properties, and the second is the one that would be easy to lose. Node
// RBAC must be absent by default in both modes. And when it is turned on it
// must grant nodes and nothing else — in particular nothing over pods or
// pods/eviction, because kubernetes.drain is deliberately not implemented and
// this is the second lock on it.
func TestNodeActionsAreOptIn(t *testing.T) {
	for _, mode := range rbacModes {
		t.Run(mode, func(t *testing.T) {
			if strings.Contains(render(t, unifiURL, mode), nodesRule) {
				t.Fatal("node RBAC is granted by default; kubernetes.cordon must be opted into")
			}

			enabled := render(t, unifiURL, mode, "rbac.allowNodeActions=true")
			if !strings.Contains(enabled, nodesRule) {
				t.Fatal("rbac.allowNodeActions=true granted no access to nodes")
			}
			// Cluster-scoped in both modes, because a namespaced Role cannot
			// grant access to a cluster-scoped resource at all.
			if !strings.Contains(enabled, "kind: ClusterRole\nmetadata:\n  name: reactor-node-actions") {
				t.Error("node RBAC is not a ClusterRole, so it cannot reach a cluster-scoped resource")
			}
			// A rule, not a mention: the template's own comments name
			// pods/eviction to say it is deliberately absent.
			if pods := regexp.MustCompile(`resources: \[[^\]]*"pods`); pods.MatchString(enabled) {
				t.Error("enabling node actions granted access to pods; draining is not an action Reactor has")
			}
		})
	}
}

// TestNamespacedInstallsStayNamespacedWithoutSecureMetrics states the one place
// rbac.clusterWide=false still produces cluster-scoped RBAC, and the escape
// hatch from it.
//
// The authn/authz filter reviews tokens and access through TokenReview and
// SubjectAccessReview, which are cluster-scoped with no namespaced form. That is
// a real consequence of asking for a protected endpoint, and it should be a
// stated behaviour rather than something an operator discovers in an audit.
func TestNamespacedInstallsStayNamespacedWithoutSecureMetrics(t *testing.T) {
	namespaced := []string{unifiURL, namespacedRBAC}

	if got := strings.Count(render(t, namespaced...), "\nkind: Cluster"); got != 0 {
		t.Errorf("a namespace-scoped install created %d cluster-scoped RBAC objects", got)
	}
	insecure := append(append([]string{}, namespaced...), "metrics.enabled=true", "metrics.secure=false")
	if got := strings.Count(render(t, insecure...), "\nkind: Cluster"); got != 0 {
		t.Errorf("an unprotected endpoint still created %d cluster-scoped RBAC objects", got)
	}

	secure := append(append([]string{}, namespaced...), "metrics.enabled=true")
	manifests := render(t, secure...)
	for _, expected := range []string{tokenReviews, "subjectaccessreviews"} {
		if !strings.Contains(manifests, expected) {
			t.Errorf("the authn/authz filter is not granted %s, so every scrape would be refused", expected)
		}
	}
	// The manager's own rules stay a Role: only the review delegation is
	// cluster-scoped, and it grants nothing about the objects Reactor acts on.
	if strings.Contains(manifests, "kind: ClusterRole\nmetadata:\n  name: reactor-manager") {
		t.Error("enabling metrics widened the manager's own RBAC back to cluster scope")
	}
}
