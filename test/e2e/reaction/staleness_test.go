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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robbeverhelst/unifi-reactor/test/e2e/harness"
)

// The console stops answering in the middle of an outage, which is the ordinary
// way for it to stop answering: the gateway is on the power that just failed,
// or on the uplink that just went down.
//
// Two things have to be true at once and they pull in opposite directions.
// Reactor must go on holding what it is holding, because handing workloads back
// the moment it loses sight of a UPS would bring them up on battery — and it
// must stop being silent about it, because a decision re-taken every fifteen
// seconds against a reading nobody has confirmed in ten minutes is not the same
// thing as a decision.
//
// Only a real cluster can show both halves: the workload staying at zero is a
// fact about the API server, not about a fold.
var _ = Describe("Deciding while the console has stopped answering", Ordered, func() {
	const (
		target   = "blind"
		shed     = "shed-while-blind"
		baseline = 2
	)

	onBackup := map[string]string{keyWAN: wanBackup}

	BeforeAll(func() {
		Expect(cluster.Apply(workload(target, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(shed, onBackup, target, 0))).To(Succeed())
	})

	AfterAll(func() {
		// Before the reset, not after: putting the console back is a request TO
		// the console, and resetConsole waits for Reactor to observe one.
		startConsole()
		resetConsole()
		Expect(cluster.Delete(scaleAutomation(shed, onBackup, target, 0))).To(Succeed())
	})

	It("claims the workload while the console is still answering", func() {
		Expect(mock.WAN(wanBackup)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("and reporting the observation it decided against")
		Expect(automationOf(Default, shed).Status.ObservedAt).NotTo(BeNil(),
			"nothing said how current the state behind this decision is")
	})

	It("keeps holding the workload once the console goes quiet, and says the state is old", func() {
		stopConsole()

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, shed), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("ObservationStale"))
			g.Expect(ready.Message).To(ContainSubstring("still acting on the state it last reported"))
		}).Should(Succeed(), "the console stopped answering and nothing on the automation said so")

		By("standing still, because it names the observation rather than how old it is")
		// Read once the report has settled, so this is about the timestamp not
		// advancing rather than about the last successful poll still arriving.
		observedAt := automationOf(Default, shed).Status.ObservedAt
		Expect(observedAt).NotTo(BeNil())
		Consistently(func(g Gomega) {
			g.Expect(automationOf(g, shed).Status.ObservedAt).To(HaveValue(
				HaveField("Time", BeTemporally("==", observedAt.Time))))
		}).Should(Succeed(), "the reported observation moved while nothing was being observed")

		By("and saying it out loud once, as a fault somebody has to act on")
		Eventually(func(g Gomega) {
			out, err := eventsWithReason("ObservationStale")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("Warning"), "an operator that has gone blind is not routine")
		}).Should(Succeed())

		By("while changing nothing at all about what is being held")
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(0))
			g.Expect(automationOf(g, shed).Status.Matching).To(HaveValue(BeTrue()))
			g.Expect(annotationsOf(g, target)).To(HaveKeyWithValue(annotationBaseline, "2"))
		}).Should(Succeed(), "losing sight of the console released a claim, which brings a workload back mid-outage")
	})

	It("clears the moment the console answers again, with nothing to reset", func() {
		startConsole()

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, shed), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal("Reconciled"))
		}).Should(Succeed())

		By("and the recovery being a normal reaction rather than a special case")
		Expect(mock.WAN(wanPrimary)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})
})

// stopConsole takes the rehearsed console away without touching Reactor, which
// is the shape of the failure being rehearsed: the operator is healthy, its
// reconciles are running, and its observations simply stop succeeding.
func stopConsole() {
	By("taking the rehearsed console away")
	_, err := cluster.Kubectl("-n", releaseNamespace, "scale",
		"deployment/"+harness.MockName, "--replicas=0")
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		out, err := cluster.Quiet().Kubectl("-n", releaseNamespace, "get", "pods",
			"-l", "app="+harness.MockName, "-o", "name")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(BeEmpty(), "the console is still answering")
	}).Should(Succeed())
}

// startConsole puts it back, and is safe to call on a console that never left.
func startConsole() {
	By("bringing the rehearsed console back")
	_, err := cluster.Kubectl("-n", releaseNamespace, "scale",
		"deployment/"+harness.MockName, "--replicas=1")
	Expect(err).NotTo(HaveOccurred())
	_, err = cluster.Kubectl("-n", releaseNamespace, "rollout", "status",
		"deployment/"+harness.MockName, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred())
	Eventually(mock.Reachable, 60*time.Second).Should(Succeed())
}
