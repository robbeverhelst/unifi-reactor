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

// Package lifecycle covers what happens to a cluster around a release rather
// than during one: uninstalling Reactor while it is holding workloads down,
// and upgrading from the chart packaging that shipped the CRD under crds/.
//
// Both are paths nothing else can test. They involve Helm's own ordering
// guarantees, RBAC that only exists once installed, and a CRD deletion that
// hangs forever if a finalizer outlives the controller that services it.
package lifecycle

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/test/e2e/harness"
)

const (
	managerRepository = "example.com/unifi-reactor"
	managerTag        = "lifecycle"
	managerImage      = managerRepository + ":" + managerTag

	// crdName is the CRD every spec here installs, adopts, or deletes.
	crdName = "automations.reactor.robbeverhelst.com"
	// release is the Helm release name each spec installs under its own
	// namespace. It matches the chart name, so the operator's Deployment and
	// the hook Job are named from it directly.
	release = "reactor"
	// releaseJob is the pre-delete hook that hands claimed workloads back.
	releaseJob = release + "-release-claims"
	// adoptJob is the pre-upgrade hook that takes over a CRD belonging to no
	// release, and the name its ServiceAccount and cluster-scoped RBAC share.
	adoptJob = release + "-adopt-crd"

	annotationBaseline  = "reactor.robbeverhelst.com/baseline-replicas"
	annotationClaimedBy = "reactor.robbeverhelst.com/claimed-by"

	deploymentKind = "deployment"
	automationKind = "automation"

	// mockNamespace holds one rehearsed console for the whole suite. It is
	// shared because the mock is reached on a fixed node port, which only one
	// Service in the cluster can hold.
	mockNamespace = "reactor-console"

	// finalizer is what a claim registers, and what must not outlive the
	// controller that services it.
	finalizer = "reactor.robbeverhelst.com/release-claims"

	pollInterval = "2s"

	// notifySecret holds the destination an outbound action calls. Naming it
	// here keeps the Secret, the Automation that references it, and the
	// assertion about the read in one place.
	notifySecret = "outbound-destination"

	// set is helm's value flag, repeated on every install and upgrade here.
	set = "--set"
)

var (
	cluster *harness.Cluster
	mock    *harness.Mock
)

func TestLifecycle(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "lifecycle suite")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

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

	By("starting the rehearsed console")
	_, _ = cluster.Kubectl("create", "namespace", mockNamespace)
	Expect(cluster.Apply(harness.MockManifest(mockNamespace))).To(Succeed())
	_, err = cluster.Kubectl("-n", mockNamespace, "rollout", "status",
		"deployment/"+harness.MockName, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred())
	Eventually(mock.Reachable).Should(Succeed())
})

var _ = AfterSuite(func() {
	if cluster == nil {
		return
	}
	// Nothing may outlive the suite: every spec here installs cluster-scoped
	// objects, and a leftover CRD would change what the next run observes.
	removeCRD()
	_, _ = cluster.Kubectl("delete", "namespace", mockNamespace, "--wait=false")
})

// removeCRD takes the Automation CRD back out of the cluster so the next spec
// starts from one that belongs to no release.
//
// It strips finalizers first. A finalizer whose controller is gone makes this
// hang forever — which is precisely the failure the uninstall specs are here
// to catch, so teardown must not itself depend on it never happening.
func removeCRD() {
	out, err := cluster.Kubectl("get", automationKind, "--all-namespaces", "-o",
		`jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}`)
	if err == nil {
		for ref := range strings.FieldsSeq(out) {
			namespace, name, found := strings.Cut(ref, "/")
			if !found {
				continue
			}
			_, _ = cluster.Kubectl("patch", automationKind, name, "-n", namespace,
				"--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		}
	}
	_, _ = cluster.Kubectl("delete", "crd", crdName, "--ignore-not-found", "--timeout=120s")
}

// installOperator brings up a release pointed at the shared rehearsed console,
// on a known starting state, and returns once the operator is running.
//
// Outbound actions are allowed to the console's own address, which is a
// ClusterIP Service — deliberately, because that is what makes the release
// render the conditional Secret rule and exercise the credential read that
// comes with it.
func installOperator(namespace string, values ...string) {
	_, _ = cluster.Kubectl("create", "namespace", namespace)
	Expect(mock.WAN("primary")).To(Succeed())
	Expect(mock.UPS("mode=mains&level=100&present=true")).To(Succeed())

	_, err := cluster.Kubectl("-n", namespace, "create", "secret", "generic",
		"unifi-reactor-credentials", "--from-literal=UNIFI_API_KEY=rehearsal")
	Expect(err).NotTo(HaveOccurred())

	// The destination lives in the Secret rather than in the Automation, so a
	// Secret that cannot be read produces no request at all. That is what makes
	// the assertion about it sharp.
	_, err = cluster.Kubectl("-n", namespace, "create", "secret", "generic", notifySecret,
		"--from-literal=url="+harness.MockURL(mockNamespace)+"/ups")
	Expect(err).NotTo(HaveOccurred())

	args := append([]string{"install", release, "charts/reactor", "--namespace", namespace,
		set, "image.repository=" + managerRepository,
		set, "image.tag=" + managerTag,
		set, "image.pullPolicy=Never",
		set, "unifi.url=" + harness.MockURL(mockNamespace),
		set, "unifi.pollInterval=" + pollInterval,
		set, "unifi.insecureSkipVerify=false",
		set, "actions.allowedDestinations={" + harness.MockURL(mockNamespace) + "}",
		"--wait", "--timeout=180s"}, values...)
	_, err = cluster.Helm(args...)
	Expect(err).NotTo(HaveOccurred())
}

// workload renders a target Deployment, running the mock's image because it is
// already on every node.
func workload(namespace, name string, replicas int) string {
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
`, name, namespace, replicas, harness.MockImage)
}

// wanAutomation scales a target to zero while the gateway is on its backup
// uplink, and restores whatever it was before once nothing claims it.
//
// It also carries an edge action, so one Automation exercises both kinds at
// once: a desired-state claim that is arbitrated and released, and an outbound
// call that fires on the transition and owns nothing. The call takes its
// destination from a Secret, which is the part worth having in a cluster —
// Reactor reads that Secret with an uncached reader, and whether an uncached
// read still works when the operator's cache is restricted to one namespace is
// not a question the unit tests can answer.
func wanAutomation(namespace, name, target string) string {
	return fmt.Sprintf(`apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  when:
    provider: unifi
    state: {wan: backup}
  actions:
    - type: kubernetes.scale
      target: {kind: Deployment, name: %[3]s}
      replicas: 0
    - type: http.request
      request:
        secretRef: {name: %[4]s}
        body: '{"wan":{{ .To | json }}}'
`, name, namespace, target, notifySecret)
}

func replicasOf(g Gomega, namespace, name string) int32 {
	var deployment appsv1.Deployment
	g.Expect(cluster.GetInto(&deployment, deploymentKind, namespace, name)).To(Succeed())
	g.Expect(deployment.Spec.Replicas).NotTo(BeNil())
	return *deployment.Spec.Replicas
}

func annotationsOf(g Gomega, namespace, name string) map[string]string {
	var deployment appsv1.Deployment
	g.Expect(cluster.GetInto(&deployment, deploymentKind, namespace, name)).To(Succeed())
	return deployment.Annotations
}

func automationOf(g Gomega, namespace, name string) reactorv1alpha1.Automation {
	var automation reactorv1alpha1.Automation
	g.Expect(cluster.GetInto(&automation, automationKind, namespace, name)).To(Succeed())
	return automation
}

// TestMain keeps the suite from running against whatever cluster happens to be
// current when someone runs `go test` by hand.
func TestMain(m *testing.M) {
	if os.Getenv("KIND_CLUSTER") == "" {
		fmt.Fprintln(os.Stderr, "KIND_CLUSTER is not set; run `make test-lifecycle`")
		os.Exit(1)
	}
	os.Exit(m.Run())
}
