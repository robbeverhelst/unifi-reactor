//go:build e2e

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

// Package reaction runs Reactor against a real API server and a rehearsed
// UniFi console, and asserts on what actually happened to the workloads.
//
// It exists because the behaviour that matters most is the behaviour a unit
// test cannot reach: converging after a restart from state alone, and two
// Automations arbitrating one workload between them. Both were designed
// against a mental model of the API server rather than against one.
package reaction

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/test/e2e/harness"
)

const (
	// The operator image built from this working tree and loaded into Kind.
	managerRepository = "example.com/unifi-reactor"
	managerTag        = "e2e"
	managerImage      = managerRepository + ":" + managerTag
	// releaseNamespace holds the operator and the rehearsed console.
	releaseNamespace = "reactor-e2e"
	// appsNamespace holds the workloads and the Automations acting on them,
	// which is where they belong: an Automation lives next to what it acts on.
	appsNamespace = "reactor-e2e-apps"
	// release is the Helm release name. It matches the chart name, so the
	// operator's Deployment is simply "reactor".
	release = "reactor"
	// pollInterval is how often the operator observes the console. Short
	// enough to keep the suite quick, long enough to be a real poll.
	pollInterval = "2s"

	// deploymentKind is how a target is named in kubectl and in status refs.
	deploymentKind = "deployment"
	// automationKind is the CRD's singular name.
	automationKind = "automation"

	annotationBaseline  = "reactor.robbeverhelst.com/baseline-replicas"
	annotationClaimedBy = "reactor.robbeverhelst.com/claimed-by"
	annotationClaimedAt = "reactor.robbeverhelst.com/claimed-at"

	conditionReady   = "Ready"
	conditionApplied = "Applied"

	// The state values the rehearsed console publishes, written the way an
	// Automation's spec.when.state writes them.
	keyWAN       = "wan"
	keyUPS       = "ups"
	wanPrimary   = "primary"
	wanBackup    = "backup"
	upsOnBattery = "on-battery"

	// settleWindow is how long a workload must hold a value for the suite to
	// accept that nothing is going to move it. It spans several polls and at
	// least one of the reconciler's periodic re-evaluations.
	settleWindow = 20 * time.Second
	// convergeWindow bounds waiting for a reaction that has to come through a
	// periodic re-evaluation rather than through a state change.
	convergeWindow = 90 * time.Second
)

var (
	cluster *harness.Cluster
	mock    *harness.Mock
)

func TestReaction(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "reaction suite")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(convergeWindow)
	SetDefaultEventuallyPollingInterval(time.Second)
	SetDefaultConsistentlyDuration(settleWindow)
	SetDefaultConsistentlyPollingInterval(2 * time.Second)

	var err error
	cluster, err = harness.NewCluster(GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	mock = harness.NewMock(GinkgoWriter)

	By("building and loading the operator image")
	Expect(harness.BuildManagerImage(GinkgoWriter, managerImage)).To(Succeed())
	Expect(cluster.LoadImage(managerImage)).To(Succeed())

	By("building and loading the rehearsed UniFi console")
	context, err := os.MkdirTemp("", "mock-unifi-build")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = os.RemoveAll(context) })
	Expect(harness.BuildMockImage(GinkgoWriter, context)).To(Succeed())
	Expect(cluster.LoadImage(harness.MockImage)).To(Succeed())

	By("creating the namespaces")
	for _, namespace := range []string{releaseNamespace, appsNamespace} {
		_, _ = cluster.Kubectl("create", "namespace", namespace)
	}

	By("starting the rehearsed console")
	Expect(cluster.Apply(harness.MockManifest(releaseNamespace))).To(Succeed())
	_, err = cluster.Kubectl("-n", releaseNamespace, "rollout", "status",
		"deployment/"+harness.MockName, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred())
	Eventually(mock.Reachable).Should(Succeed(), "the mock console is not reachable over the Kind port mapping")

	By("starting from a known console state")
	resetConsole()

	By("installing the chart")
	_, err = cluster.Kubectl("-n", releaseNamespace, "create", "secret", "generic",
		"unifi-reactor-credentials", "--from-literal=UNIFI_API_KEY=rehearsal")
	Expect(err).NotTo(HaveOccurred())
	_, err = cluster.Helm("install", release, "charts/reactor",
		"--namespace", releaseNamespace,
		"--set", "image.repository="+managerRepository,
		"--set", "image.tag="+managerTag,
		"--set", "image.pullPolicy=Never",
		"--set", "unifi.url="+harness.MockURL(releaseNamespace),
		"--set", "unifi.insecureSkipVerify=false",
		"--set", "unifi.pollInterval="+pollInterval,
		// Structured logs, so the suite can assert on what the operator did
		// and did not observe rather than on a rendered sentence.
		"--set", "log.format=json",
		"--wait", "--timeout=180s")
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if cluster == nil {
		return
	}
	if CurrentSpecReport().Failed() {
		logs, _ := cluster.Kubectl("-n", releaseNamespace, "logs", "deployment/"+release, "--tail=200")
		_, _ = fmt.Fprintf(GinkgoWriter, "operator logs:\n%s\n", logs)
	}
	_, _ = cluster.Helm("uninstall", release, "--namespace", releaseNamespace, "--ignore-not-found")
	for _, namespace := range []string{appsNamespace, releaseNamespace} {
		_, _ = cluster.Kubectl("delete", "namespace", namespace, "--wait=false")
	}
})

// resetConsole returns the rehearsed console to mains power on the primary
// uplink, so every spec starts from the same place regardless of what the one
// before it rehearsed.
func resetConsole() {
	Expect(mock.WAN(wanPrimary)).To(Succeed())
	Expect(mock.UPS("mode=mains&level=100&present=true")).To(Succeed())
}

// workload renders a target Deployment. The pods run the mock's image because
// it is already on every node: nothing here waits for them to be ready, and an
// image pull would be the one part of this suite that needs the internet.
func workload(name string, replicas int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: %[3]d
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: app
          image: %[4]s
          imagePullPolicy: Never
`, name, appsNamespace, replicas, harness.MockImage)
}

// scaleAutomation renders an Automation that holds one target at a level while
// a condition holds. Omitting onExit is the common case and means Baseline:
// restore whatever the workload was before Reactor first claimed it.
func scaleAutomation(name string, when map[string]string, target string, replicas int) string {
	var state strings.Builder
	for key, value := range when {
		fmt.Fprintf(&state, "      %s: %q\n", key, value)
	}
	return fmt.Sprintf(`apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: %s
  namespace: %s
spec:
  when:
    provider: unifi
    state:
%s  actions:
    - type: kubernetes.scale
      target: {kind: Deployment, name: %s}
      replicas: %d
`, name, appsNamespace, state.String(), target, replicas)
}

// replicasOf reads what a target is currently scaled to.
func replicasOf(g Gomega, name string) int32 {
	var deployment appsv1.Deployment
	g.Expect(cluster.GetInto(&deployment, deploymentKind, appsNamespace, name)).To(Succeed())
	g.Expect(deployment.Spec.Replicas).NotTo(BeNil())
	return *deployment.Spec.Replicas
}

// annotationsOf reads a target's annotations, which are what a stranded
// workload carries to explain itself once Reactor is gone.
func annotationsOf(g Gomega, name string) map[string]string {
	var deployment appsv1.Deployment
	g.Expect(cluster.GetInto(&deployment, deploymentKind, appsNamespace, name)).To(Succeed())
	return deployment.Annotations
}

// generationOf is how the suite tells a write apart from a no-op: the API
// server bumps it on every change to spec, and on nothing else.
func generationOf(g Gomega, name string) int64 {
	var deployment appsv1.Deployment
	g.Expect(cluster.GetInto(&deployment, deploymentKind, appsNamespace, name)).To(Succeed())
	return deployment.Generation
}

func automationOf(g Gomega, name string) reactorv1alpha1.Automation {
	var automation reactorv1alpha1.Automation
	g.Expect(cluster.GetInto(&automation, automationKind, appsNamespace, name)).To(Succeed())
	return automation
}

// targetStatus finds the entry describing one target, which is where the
// arbitrated outcome is reported.
func targetStatus(automation reactorv1alpha1.Automation, target string) *reactorv1alpha1.TargetStatus {
	ref := fmt.Sprintf("Deployment/%s/%s", appsNamespace, target)
	for i := range automation.Status.Targets {
		if automation.Status.Targets[i].Ref == ref {
			return &automation.Status.Targets[i]
		}
	}
	return nil
}

func conditionOf(automation reactorv1alpha1.Automation, conditionType string) *metav1.Condition {
	for i := range automation.Status.Conditions {
		if automation.Status.Conditions[i].Type == conditionType {
			return &automation.Status.Conditions[i]
		}
	}
	return nil
}

// claimant is how an Automation names itself in another's deferredBy list.
func claimant(name string) string { return appsNamespace + "/" + name }

// restartOperator takes Reactor away, runs the caller's rehearsal while
// nothing is watching, and brings it back. This is the shape of the promise
// the whole design rests on: what the world becomes is decided by the state
// Reactor finds, not by the changes it happened to witness.
func restartOperator(whileDown func()) {
	By("taking the operator down")
	_, err := cluster.Kubectl("-n", releaseNamespace, "scale", "deployment/"+release, "--replicas=0")
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		out, err := cluster.Kubectl("-n", releaseNamespace, "get", "pods",
			"-l", "app.kubernetes.io/name=reactor", "-o", "name")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(BeEmpty(), "the operator is still running")
	}).Should(Succeed())

	whileDown()

	By("bringing the operator back")
	_, err = cluster.Kubectl("-n", releaseNamespace, "scale", "deployment/"+release, "--replicas=1")
	Expect(err).NotTo(HaveOccurred())
	_, err = cluster.Kubectl("-n", releaseNamespace, "rollout", "status",
		"deployment/"+release, "--timeout=180s")
	Expect(err).NotTo(HaveOccurred())
}

// operatorLogs returns what the currently running operator has logged. After a
// restart that is only what it has seen since coming back, which is exactly
// the question the restart specs ask.
func operatorLogs() string {
	logs, err := cluster.Kubectl("-n", releaseNamespace, "logs", "deployment/"+release, "--tail=-1")
	Expect(err).NotTo(HaveOccurred())
	return logs
}

// TestMain keeps the suite from running against whatever cluster happens to be
// current when someone runs `go test` by hand.
func TestMain(m *testing.M) {
	if os.Getenv("KIND_CLUSTER") == "" {
		fmt.Fprintln(os.Stderr, "KIND_CLUSTER is not set; run `make test-reaction`")
		os.Exit(1)
	}
	os.Exit(m.Run())
}
