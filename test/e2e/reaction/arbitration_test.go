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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Two Automations, one workload, different opinions. The outcome must depend
// only on which conditions currently hold — never on which of them reconciled
// last — and an Automation whose condition ends must not raise a workload
// another one is still holding down.
var _ = Describe("Arbitrating one workload between two Automations", Ordered, func() {
	const (
		shared     = "shared"
		wanShared  = "wan-shared"
		upsShared  = "ups-shared"
		baseline   = 3
		wanReduced = 1
	)

	onBackup := map[string]string{keyWAN: wanBackup}
	onBattery := map[string]string{keyUPS: upsOnBattery}
	upsClaim := scaleAutomation(upsShared, onBattery, shared, 0)

	BeforeAll(func() {
		Expect(cluster.Apply(workload(shared, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(wanShared, onBackup, shared, wanReduced))).To(Succeed())
		Expect(cluster.Apply(upsClaim)).To(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Expect(cluster.Delete(scaleAutomation(wanShared, onBackup, shared, wanReduced))).To(Succeed())
		Expect(cluster.Delete(upsClaim)).To(Succeed())
	})

	It("applies the only claim there is", func() {
		Expect(mock.WAN(wanBackup)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, shared)).To(BeEquivalentTo(wanReduced))
		}).Should(Succeed())

		automation := automationOf(Default, wanShared)
		target := targetStatus(automation, shared)
		Expect(target.Desired).To(HaveValue(BeEquivalentTo(wanReduced)))
		Expect(target.Effective).To(HaveValue(BeEquivalentTo(wanReduced)))
		Expect(target.DeferredBy).To(BeEmpty())
		Expect(conditionOf(automation, conditionApplied)).To(HaveField("Status", metav1.ConditionTrue))
	})

	It("resolves a second, more restrictive claim in favour of the more restrictive one", func() {
		Expect(mock.UPS("mode=battery&level=80")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, shared)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("telling the outvoted automation who is holding its target, and why it is not an error")
		Eventually(func(g Gomega) {
			automation := automationOf(g, wanShared)
			target := targetStatus(automation, shared)
			g.Expect(target).NotTo(BeNil())
			g.Expect(target.Desired).To(HaveValue(BeEquivalentTo(wanReduced)))
			g.Expect(target.Effective).To(HaveValue(BeEquivalentTo(0)))
			g.Expect(target.DeferredBy).To(ConsistOf(claimant(upsShared)))

			applied := conditionOf(automation, conditionApplied)
			g.Expect(applied).NotTo(BeNil())
			g.Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(applied.Reason).To(Equal("DeferredToOtherAutomation"))
		}).Should(Succeed())

		By("and reporting the winning claim as in effect")
		winner := automationOf(Default, upsShared)
		Expect(targetStatus(winner, shared).DeferredBy).To(BeEmpty())
		Expect(conditionOf(winner, conditionApplied)).To(HaveField("Status", metav1.ConditionTrue))
		Expect(annotationsOf(Default, shared)).To(HaveKeyWithValue(annotationClaimedBy,
			claimant(upsShared)+","+claimant(wanShared)))
	})

	It("does not raise the workload when one condition ends while the other still holds", func() {
		By("recovering the WAN in the middle of the power failure")
		Expect(mock.WAN(wanPrimary)).To(Succeed())

		Eventually(func(g Gomega) {
			automation := automationOf(g, wanShared)
			g.Expect(automation.Status.Matching).To(BeFalse())
			target := targetStatus(automation, shared)
			g.Expect(target).NotTo(BeNil())
			g.Expect(target.Desired).To(HaveValue(BeEquivalentTo(baseline)),
				"an automation that stopped matching wants its target back at the baseline")
			g.Expect(target.Effective).To(HaveValue(BeEquivalentTo(0)))
			g.Expect(target.DeferredBy).To(ConsistOf(claimant(upsShared)))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, shared)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "the recovered automation reverted a workload the other one is still claiming")

		By("keeping the baseline recorded, since the target is still claimed")
		Expect(annotationsOf(Default, shared)).To(HaveKeyWithValue(annotationBaseline, "3"))
		Expect(annotationsOf(Default, shared)).To(HaveKeyWithValue(annotationClaimedBy, claimant(upsShared)))
	})

	It("releases the workload once nothing claims it any more", func() {
		Expect(mock.UPS("mode=mains&level=100")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, shared)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
		Expect(annotationsOf(Default, shared)).NotTo(HaveKey(annotationBaseline))
		Expect(annotationsOf(Default, shared)).NotTo(HaveKey(annotationClaimedBy))
	})

	It("hands the workload back when the automation holding it down is deleted mid-outage", func() {
		By("putting the workload back down on battery power")
		Expect(mock.UPS("mode=battery&level=80")).To(Succeed())
		Eventually(func(g Gomega) { g.Expect(replicasOf(g, shared)).To(BeEquivalentTo(0)) }).Should(Succeed())

		By("deleting the automation while its condition still holds")
		Expect(cluster.Delete(upsClaim)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, shared)).To(BeEquivalentTo(baseline))
		}).Should(Succeed(), "removing the policy left the workload stranded, which is the failure #39 is about")

		By("and letting the object go rather than leaving a finalizer nothing will service")
		out, err := cluster.Kubectl("get", automationKind, "-n", appsNamespace, "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring(upsShared))
		Expect(annotationsOf(Default, shared)).NotTo(HaveKey(annotationBaseline))
	})
})
