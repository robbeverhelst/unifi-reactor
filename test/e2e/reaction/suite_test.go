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
	"maps"
	"os"
	"slices"
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
	// maxObservationAge is how old the observed state may get before every
	// automation reports ObservationStale. Fifteen consecutive failed polls,
	// which nothing but a console deliberately taken away reaches.
	maxObservationAge = "30s"

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
	keyWAN         = "wan"
	keyWANQuality  = "wan.quality"
	keyUPS         = "ups"
	keyUPSBattery  = "ups.battery"
	keyUPSRuntime  = "ups.runtime"
	keyUPSLoad     = "ups.load"
	keyInternet    = "internet"
	keyDevices     = "devices"
	keyFirmware    = "firmware"
	keyTemperature = "temperature"
	keyWiFi        = "wifi"
	keyPoE         = "poe"
	// keyOutlet is one UPS outlet, addressed by index because the captured
	// console has not named any of them. keyOutletNamed is what the same outlet
	// is called once it is named, which is the argument for naming them.
	keyOutlet      = "outlet.5"
	keyOutletNamed = "outlet.nas"
	// keyUPSDevice is the per-device key the captured UPS publishes under, and
	// the reason the suite installs with per-device keys on: they are opt-in,
	// so an install that does not ask for them is the default rather than the
	// case under test.
	keyUPSDevice       = "device.ups-2u"
	wanPrimary         = "primary"
	wanBackup          = "backup"
	wanQualityDegraded = "degraded"
	upsOnBattery       = "on-battery"
	batteryNormal      = "normal"
	upsRuntimeAmple    = "ample"
	upsRuntimeCritical = "critical"
	upsLoadNormal      = "normal"
	upsLoadHigh        = "high"
	internetOK         = "ok"
	internetDown       = "down"
	upsOnline          = "online"
	devicesAllOnline   = "all-online"
	devicesDegraded    = "degraded"
	deviceOnline       = "online"
	deviceOffline      = "offline"
	firmwareCurrent    = "current"
	firmwareUpdates    = "updates-available"
	temperatureNormal  = "normal"
	temperatureHigh    = "high"
	wifiOK             = "ok"
	wifiWarning        = "warning"
	wifiError          = "error"
	poeOK              = "ok"
	poeInsufficient    = "insufficient"
	outletOn           = "on"
	outletOff          = "off"

	// witness is an automation that claims nothing and only reports what it
	// sees, which is how the suite asks Reactor whether it has caught up with
	// the console. witnessTarget is the workload it names — nothing else
	// touches it, and a suspended automation writes to nothing anyway.
	witness       = "console-witness"
	witnessTarget = "console-witness-target"

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
	putConsoleAtRest()

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
		// Empty in the chart, and set here: the suite asserts that an automation
		// says how old the state it is deciding against is once the console
		// stops answering — and goes on holding its target while it says it.
		// Comfortably above any normal hiccup at a 2s poll, so only a console
		// that is deliberately taken away reaches it.
		"--set", "unifi.maxObservationAge="+maxObservationAge,
		// Off by default in the chart, and on here: the suite asserts that
		// Reactor declines a workload a HorizontalPodAutoscaler already drives,
		// which is exactly the behaviour this value turns on.
		"--set", "safety.detectHPA=true",
		// The per-device keys are opt-in, so the suite has to ask for them to
		// rehearse one. The aggregate is published either way, and both are
		// asserted — the point of the opt-in is that only one of them is free.
		"--set", "unifi.devices.perDeviceKeys=true",
		// Structured logs, so the suite can assert on what the operator did
		// and did not observe rather than on a rendered sentence.
		"--set", "log.format=json",
		"--wait", "--timeout=180s")
	Expect(err).NotTo(HaveOccurred())

	By("applying the witness that tells the suite when Reactor has caught up with the console")
	Expect(cluster.Apply(workload(witnessTarget, 1))).To(Succeed())
	Expect(cluster.Apply(witnessAutomation())).To(Succeed())
	awaitConsoleObserved()
})

// witnessAutomation renders the automation resetConsole waits on: suspended, so
// it is out of force and claims nothing, and naming every key the reset
// restores so that its status answers "has Reactor seen this yet?".
//
// Suspended is exactly the right shape and not a trick. A suspended automation
// is documented as one that goes on observing and reporting while claiming
// nothing — which is what makes it safe to leave in place beside every other
// spec in this suite, and why it can never be the reason one of them fails.
func witnessAutomation() string {
	var state strings.Builder
	for _, key := range slices.Sorted(maps.Keys(consoleAtRest)) {
		fmt.Fprintf(&state, "      %s: %q\n", key, consoleAtRest[key])
	}
	return fmt.Sprintf(`apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: %s
  namespace: %s
spec:
  suspend: true
  when:
    provider: unifi
    state:
%s  actions:
    - type: kubernetes.scale
      target: {kind: Deployment, name: %s}
      replicas: 0
`, witness, appsNamespace, state.String(), witnessTarget)
}

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

// consoleAtRest is what the keys putConsoleAtRest restores read as once Reactor
// has observed the reset. It is what the witness automation below watches for.
//
// It names the keys that are always PRESENT after a reset, which is all of them
// bar three: firmware, temperature and poe reset to "not reported at all", and
// an absent key cannot bleed into the next suite anyway — an automation that
// cannot see a key holds its last matching, which for one that has just been
// created is false. wan.quality and isp are left out for the opposite reason:
// they are derived from probes and lookups rather than restored to a value.
var consoleAtRest = map[string]string{
	keyWAN:        wanPrimary,
	keyUPS:        upsOnline,
	keyUPSBattery: batteryNormal,
	keyUPSRuntime: upsRuntimeAmple,
	keyUPSLoad:    upsLoadNormal,
	keyInternet:   internetOK,
	keyDevices:    devicesAllOnline,
	// warning, not ok: the capture has one of three access points
	// disconnected, so a console put back to it is half-broken by construction.
	// The fleet specs start by fixing that; a reset does not.
	keyWiFi:   wifiWarning,
	keyOutlet: outletOn,
}

// resetConsole returns the rehearsed console to mains power on the primary
// uplink and does not come back until Reactor has observed that, so every spec
// starts from a console Reactor already agrees with rather than from one it is
// about to.
//
// The waiting is the point, and it is not test hygiene. Writing to the console
// changes what is true; it does not change what Reactor is acting on. Between
// the two there is a window bounded by the poll interval times the debounce
// samples of the slowest key touched — a real, documented property of the
// product, not an artefact of the harness. A suite that applies an automation
// inside that window is applying it against the PREVIOUS suite's state, and
// what it then observes is a reaction to a console that no longer exists.
//
// That is what made three of these suites intermittently red, and it is why the
// fix belongs here rather than as a defensive reset in every BeforeAll. One
// place has to know that the window exists; seven places quietly working around
// it is how it stops being known at all.
func resetConsole() {
	putConsoleAtRest()
	awaitConsoleObserved()
}

// awaitConsoleObserved blocks until Reactor reports every key the reset
// restored at its at-rest value, which is the only evidence available that the
// window above has closed.
func awaitConsoleObserved() {
	Eventually(func(g Gomega) {
		observer := automationOf(g, witness)
		g.Expect(observer.Status.ObservedState).To(Equal(consoleAtRest))
		g.Expect(observer.Status.Matching).To(BeTrue())
	}).Should(Succeed(), "Reactor is still acting on the console state the previous spec left behind")
}

// putConsoleAtRest performs the writes, without waiting for anyone to notice
// them. It is separate only for the one caller that runs before the operator
// exists at all.
func putConsoleAtRest() {
	Expect(mock.WAN(wanPrimary)).To(Succeed())
	// runtime and output are put back to the captured figures explicitly: the
	// mock holds an override until it is told otherwise, and a spec that
	// inherited "850W" from the one before it would be testing the wrong thing.
	Expect(mock.UPS("mode=mains&level=100&present=true&runtime=1043&output=310&budget=1000")).To(Succeed())
	Expect(mock.Internet("present=true&status=ok")).To(Succeed())
	Expect(mock.Quality("reset=true")).To(Succeed())
	// The fleet keys hold their overrides the same way the UPS numbers do, so
	// each is put back explicitly rather than left wherever the last spec left
	// it. firmware, temperature and poe are reset to "not reported at all",
	// which is what the captures actually show.
	Expect(mock.Device("reset=true")).To(Succeed())
	Expect(mock.WiFi("reset=true")).To(Succeed())
	Expect(mock.Firmware("present=false")).To(Succeed())
	Expect(mock.Temperature("present=false")).To(Succeed())
	Expect(mock.PoE("present=false")).To(Succeed())
	// Unlike the three above, the outlets ARE in the capture, so their reset is
	// back to eight closed relays in two relay groups rather than to nothing.
	Expect(mock.Outlets("reset=true")).To(Succeed())
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

// declaringExit is scaleAutomation with an explicit onExit level, which is how
// an automation says what it thinks its target's normal size is rather than
// deferring to the baseline Reactor recorded.
func declaringExit(name string, when map[string]string, target string, replicas, onExit int) string {
	return scaleAutomation(name, when, target, replicas) + fmt.Sprintf(`  onExit:
    - type: kubernetes.scale
      target: {kind: Deployment, name: %s}
      replicas: %d
`, target, onExit)
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
