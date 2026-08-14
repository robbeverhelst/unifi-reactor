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

package reaction

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"

	"github.com/robbeverhelst/unifi-reactor/test/e2e/harness"
)

// Target kinds beyond Deployment, against a real API server and the real chart
// RBAC.
//
// envtest already proves the executor: it reads a replica count through the
// scale subresource, so a StatefulSet needs no code a Deployment does not. What
// it cannot prove is the half that fails in production — whether the
// ServiceAccount the chart actually renders is allowed to do it. A missing
// statefulsets/scale rule, or a cronjobs rule on the wrong API group, compiles,
// passes every unit test, and then refuses the one write the automation existed
// for. That is the shape of both bugs these suites caught before v1.0.
var _ = Describe("Holding target kinds other than Deployment", Ordered, func() {
	const (
		set        = "ledger"
		cron       = "nightly-backup"
		setClaim   = "shed-statefulset"
		cronClaim  = "pause-cronjob"
		setInitial = 2
	)

	onBattery := map[string]string{keyUPS: upsOnBattery}
	manifests := []string{
		statefulSet(set, setInitial),
		cronJob(cron),
		kindAutomation(setClaim, onBattery, `
    - type: kubernetes.scale
      target: {kind: StatefulSet, name: `+set+`}
      replicas: 0`),
		kindAutomation(cronClaim, onBattery, `
    - type: kubernetes.cronjob.suspend
      target: {kind: CronJob, name: `+cron+`}`),
	}

	BeforeAll(func() {
		for _, manifest := range manifests {
			Expect(cluster.Apply(manifest)).To(Succeed())
		}
	})

	AfterAll(func() {
		resetConsole()
		for _, manifest := range manifests {
			Expect(cluster.Delete(manifest)).To(Succeed())
		}
	})

	It("scales a StatefulSet through its scale subresource", func() {
		Expect(mock.UPS("mode=battery&level=80")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(statefulSetReplicas(g, set)).To(BeEquivalentTo(0))
		}, convergeWindow).Should(Succeed())

		By("recording the baseline on the StatefulSet, so it explains itself with Reactor gone")
		Eventually(func(g Gomega) {
			var live appsv1.StatefulSet
			g.Expect(cluster.GetInto(&live, "statefulset", appsNamespace, set)).To(Succeed())
			g.Expect(live.Annotations).To(HaveKeyWithValue(annotationBaseline, "2"))
			g.Expect(live.Annotations).To(HaveKeyWithValue(annotationClaimedBy, claimant(setClaim)))
		}).Should(Succeed())

		By("reporting the kind in the target ref, not assuming Deployment")
		Eventually(func(g Gomega) {
			automation := automationOf(g, setClaim)
			g.Expect(automation.Status.Targets).To(HaveLen(1))
			g.Expect(automation.Status.Targets[0].Ref).To(
				Equal(fmt.Sprintf("StatefulSet/%s/%s", appsNamespace, set)))
			g.Expect(automation.Status.Targets[0].Level).To(Equal("0 replicas"))
		}).Should(Succeed())
	})

	It("suspends a CronJob and restores what it found on the way out", func() {
		Eventually(func(g Gomega) {
			g.Expect(cronJobSuspended(g, cron)).To(BeTrue())
		}, convergeWindow).Should(Succeed())

		By("recording the baseline under its own annotation, never as a replica count")
		Eventually(func(g Gomega) {
			var live batchv1.CronJob
			g.Expect(cluster.GetInto(&live, "cronjob", appsNamespace, cron)).To(Succeed())
			g.Expect(live.Annotations).To(HaveKeyWithValue(
				"reactor.robbeverhelst.com/baseline-suspend", "false"))
			g.Expect(live.Annotations).NotTo(HaveKey(annotationBaseline))
		}).Should(Succeed())

		By("handing both kinds back once the power returns")
		resetConsole()
		Eventually(func(g Gomega) {
			g.Expect(cronJobSuspended(g, cron)).To(BeFalse())
			g.Expect(statefulSetReplicas(g, set)).To(BeEquivalentTo(setInitial))
		}, convergeWindow).Should(Succeed())
	})
})

// statefulSet renders a target StatefulSet. Like workload it runs the mock's
// image, which is already on every node, because nothing here waits for a pod.
func statefulSet(name string, replicas int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: %[3]d
  serviceName: %[1]s
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

// cronJob renders a target CronJob. The schedule is deliberately one that will
// not fire during the suite: what is under test is spec.suspend, and a Job
// starting would only add noise.
func cronJob(name string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: CronJob
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  schedule: "0 3 1 1 *"
  suspend: false
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: app
              image: %[3]s
              imagePullPolicy: Never
`, name, appsNamespace, harness.MockImage)
}

// kindAutomation renders an Automation whose actions block is written out, so
// one helper serves every action type rather than one helper per type.
func kindAutomation(name string, when map[string]string, actions string) string {
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
%s  actions:%s
`, name, appsNamespace, state.String(), actions)
}

func statefulSetReplicas(g Gomega, name string) int32 {
	var live appsv1.StatefulSet
	g.Expect(cluster.GetInto(&live, "statefulset", appsNamespace, name)).To(Succeed())
	g.Expect(live.Spec.Replicas).NotTo(BeNil())
	return *live.Spec.Replicas
}

func cronJobSuspended(g Gomega, name string) bool {
	var live batchv1.CronJob
	g.Expect(cluster.GetInto(&live, "cronjob", appsNamespace, name)).To(Succeed())
	g.Expect(live.Spec.Suspend).NotTo(BeNil())
	return *live.Spec.Suspend
}
