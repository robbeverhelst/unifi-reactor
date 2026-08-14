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

// The failure mode wan structurally cannot express: the link is up, the uplink
// is unchanged, and there is no internet. Every assertion here holds wan at
// primary throughout, because a spec that also moved the uplink would prove
// nothing this suite does not already prove elsewhere.
var _ = Describe("Reacting to the internet going away under a healthy link", Ordered, func() {
	const (
		offsite  = "offsite-backup"
		shed     = "internet-shed"
		baseline = 2
	)

	when := map[string]string{keyInternet: internetDown}

	BeforeAll(func() {
		Expect(cluster.Apply(workload(offsite, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(shed, when, offsite, 0))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(conditionOf(automationOf(g, shed), conditionReady)).NotTo(BeNil())
		}).Should(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Eventually(func(g Gomega) { g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(baseline)) }).Should(Succeed())
		Expect(cluster.Delete(scaleAutomation(shed, when, offsite, 0))).To(Succeed())
	})

	It("sheds when the internet goes down with the uplink unchanged", func() {
		Expect(mock.Internet("status=error")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		automation := automationOf(Default, shed)
		Expect(automation.Status.Matching).To(BeTrue())
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue(keyInternet, internetDown))
		By("with the uplink exactly where it was: this is the case wan cannot see")
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue(keyWAN, wanPrimary))
	})

	It("holds its claim when the console stops reporting internet health at all", func() {
		Expect(mock.Internet("present=false")).To(Succeed())

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, shed), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"))
			g.Expect(ready.Message).To(ContainSubstring(keyInternet))
		}).Should(Succeed())

		By("leaving the workload down: losing sight of the outage is not the outage ending")
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(0))
		}).Should(Succeed())
	})

	It("restores the workload when the internet comes back", func() {
		Expect(mock.Internet("present=true&status=ok")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// wan.quality answers a different question over a different time horizon,
	// so a link that is measurably bad must not be read as no internet.
	It("keeps a degraded link apart from an absent internet", func() {
		Expect(mock.Quality("availability=80&latency=900")).To(Succeed())

		Eventually(func(g Gomega) {
			state := automationOf(g, shed).Status.ObservedState
			g.Expect(state).To(HaveKeyWithValue(keyWANQuality, wanQualityDegraded))
			g.Expect(state).To(HaveKeyWithValue(keyInternet, internetOK))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(baseline))
		}).Should(Succeed(), "a degraded link is not a down internet, and must not shed on its own")
	})
})
