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
// is unchanged, and there is no internet.
//
// The shedding Automation states that compound condition literally — `internet:
// down` *and* `wan: primary` — rather than matching on `internet` alone. All
// keys in a state block must match, so the shed firing is itself the evidence
// that the uplink never moved, which is the claim being made. It also means
// status.observedState carries both keys: it holds the subset of provider
// state an Automation references, so a key this spec did not match on could
// not be asserted here at all.
var _ = Describe("Reacting to the internet going away under a healthy link", Ordered, func() {
	const (
		offsite  = "offsite-backup"
		bulkSync = "bulk-sync"
		shed     = "internet-shed"
		quality  = "quality-shed"
		baseline = 2
	)

	// The uplink is up and selected; the internet behind it is not there.
	outage := map[string]string{keyInternet: internetDown, keyWAN: wanPrimary}
	// A link that is measurably bad rather than absent — a different question
	// over a different time horizon, and its own Automation.
	flaky := map[string]string{keyWANQuality: wanQualityDegraded}

	BeforeAll(func() {
		// Stated rather than inherited: this block's condition includes
		// wan: primary, so the uplink it starts on is part of its setup.
		resetConsole()
		Expect(cluster.Apply(workload(offsite, baseline))).To(Succeed())
		Expect(cluster.Apply(workload(bulkSync, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(shed, outage, offsite, 0))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(quality, flaky, bulkSync, 0))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(conditionOf(automationOf(g, shed), conditionReady)).NotTo(BeNil())
			g.Expect(conditionOf(automationOf(g, quality), conditionReady)).NotTo(BeNil())
		}).Should(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, bulkSync)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
		Expect(cluster.Delete(scaleAutomation(shed, outage, offsite, 0))).To(Succeed())
		Expect(cluster.Delete(scaleAutomation(quality, flaky, bulkSync, 0))).To(Succeed())
	})

	It("sheds when the internet goes down with the uplink unchanged", func() {
		Expect(mock.Internet("status=error")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		automation := automationOf(Default, shed)
		Expect(automation.Status.Matching).To(BeTrue())
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue(keyInternet, internetDown))
		By("with the uplink exactly where it was — the shed could not have fired otherwise")
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

		By("keeping the key beside it, which comes from a different endpoint")
		Expect(automationOf(Default, shed).Status.ObservedState).To(HaveKeyWithValue(keyWAN, wanPrimary))

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
			g.Expect(replicasOf(g, bulkSync)).To(BeEquivalentTo(0))
		}).Should(Succeed())
		Expect(automationOf(Default, quality).Status.ObservedState).
			To(HaveKeyWithValue(keyWANQuality, wanQualityDegraded))

		By("without the internet automation reacting: the internet is still there")
		Expect(automationOf(Default, shed).Status.ObservedState).To(HaveKeyWithValue(keyInternet, internetOK))
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, offsite)).To(BeEquivalentTo(baseline))
		}).Should(Succeed(), "a degraded link is not a down internet, and must not shed what an outage would")
	})
})
