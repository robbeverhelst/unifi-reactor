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
)

// The reaction specs share one console, so each block leaves it on mains power
// and the primary uplink for the next one.
var _ = Describe("Reacting to observed state", Ordered, func() {
	const (
		hello    = "hello"
		wanShed  = "wan-shed"
		baseline = 2
	)

	BeforeAll(func() {
		Expect(cluster.Apply(workload(hello, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(wanShed, map[string]string{keyWAN: wanBackup}, hello, 0))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(conditionOf(automationOf(g, wanShed), conditionReady)).NotTo(BeNil())
		}).Should(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Eventually(func(g Gomega) { g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(baseline)) }).Should(Succeed())
		Expect(cluster.Delete(scaleAutomation(wanShed, map[string]string{keyWAN: wanBackup}, hello, 0))).To(Succeed())
	})

	It("leaves a workload alone until the condition it depends on holds", func() {
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(baseline))
			g.Expect(annotationsOf(g, hello)).NotTo(HaveKey(annotationBaseline))
		}, 6*time.Second).Should(Succeed(), "a workload nothing claims must stay where its owner put it")
	})

	It("scales the workload down when the WAN fails over", func() {
		Expect(mock.WAN(wanBackup)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("recording on the target what it was before, so it can be restored without Reactor")
		annotations := annotationsOf(Default, hello)
		Expect(annotations).To(HaveKeyWithValue(annotationBaseline, "2"))
		Expect(annotations).To(HaveKeyWithValue(annotationClaimedBy, claimant(wanShed)))
		Expect(annotations).To(HaveKey(annotationClaimedAt))

		By("reporting the arbitrated outcome in status")
		automation := automationOf(Default, wanShed)
		Expect(automation.Status.Matching).To(HaveValue(BeTrue()))
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue(keyWAN, wanBackup))
		target := targetStatus(automation, hello)
		Expect(target).NotTo(BeNil())
		Expect(target.Desired).To(HaveValue(BeEquivalentTo(0)))
		Expect(target.Effective).To(HaveValue(BeEquivalentTo(0)))
		Expect(target.DeferredBy).To(BeEmpty())
		Expect(conditionOf(automation, conditionApplied)).To(HaveField("Status", metav1.ConditionTrue))
		Expect(conditionOf(automation, conditionReady)).To(HaveField("Status", metav1.ConditionTrue))
	})

	It("treats every later observation of the same state as a no-op", func() {
		before := generationOf(Default, hello)
		Consistently(func(g Gomega) {
			g.Expect(generationOf(g, hello)).To(Equal(before))
		}).Should(Succeed(), "repeated observations of an unchanged state rewrote the target")
	})

	It("puts a workload back where Reactor is holding it when something else moves it", func() {
		By("scaling the claimed workload by hand, with no console state change at all")
		_, err := cluster.Kubectl("-n", appsNamespace, "scale", "deployment/"+hello, "--replicas=4")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "a claimed target is reconciled continuously, not only on transitions")

		By("leaving the recorded baseline as it was before Reactor claimed the target")
		Expect(annotationsOf(Default, hello)).To(HaveKeyWithValue(annotationBaseline, "2"))
	})

	It("restores the workload when the WAN recovers", func() {
		Expect(mock.WAN(wanPrimary)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())

		By("no longer asserting anything about a target nothing claims")
		annotations := annotationsOf(Default, hello)
		Expect(annotations).NotTo(HaveKey(annotationBaseline))
		Expect(annotations).NotTo(HaveKey(annotationClaimedBy))
		Expect(annotations).NotTo(HaveKey(annotationClaimedAt))

		automation := automationOf(Default, wanShed)
		Expect(automation.Status.Matching).To(HaveValue(BeFalse()))
		Expect(targetStatus(automation, hello).Effective).To(BeNil())
	})

	It("converges on a state that changed while it was down, having never seen the change", func() {
		restartOperator(func() {
			By("failing the WAN over with nothing running to witness it")
			Expect(mock.WAN(wanBackup)).To(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(baseline))
			}, 5*time.Second, time.Second).Should(Succeed(), "something reacted with the operator down")
		})

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "the operator did not re-derive its reaction from the state it found")

		By("having derived that from current state rather than from a transition it observed")
		logs := operatorLogs()
		Expect(logs).To(ContainSubstring(`"key":"wan","from":"","to":"backup"`),
			"the restarted operator did not report the WAN as first seen on backup")
		Expect(logs).NotTo(ContainSubstring(`"key":"wan","from":"primary","to":"backup"`),
			"the operator reported an edge it cannot have observed; it was not running when the WAN failed over")
	})

	It("keeps the baseline it recorded before the restart, not the value it is holding", func() {
		By("restarting mid-outage, while the workload is scaled to zero")
		Expect(replicasOf(Default, hello)).To(BeEquivalentTo(0))
		restartOperator(func() {})

		Eventually(func(g Gomega) {
			g.Expect(annotationsOf(g, hello)).To(HaveKeyWithValue(annotationBaseline, "2"))
		}).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(annotationsOf(g, hello)).To(HaveKeyWithValue(annotationBaseline, "2"))
		}, 10*time.Second).Should(Succeed(), "re-claiming after a restart overwrote the baseline with the held value")

		By("so that recovery restores the workload rather than leaving it at zero")
		Expect(mock.WAN(wanPrimary)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	It("restores a workload on recovery it never saw fail, and never saw recover", func() {
		By("failing the WAN over and letting the operator claim the workload")
		Expect(mock.WAN(wanBackup)).To(Succeed())
		Eventually(func(g Gomega) { g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(0)) }).Should(Succeed())

		restartOperator(func() {
			By("restoring the WAN with nothing running to witness it")
			Expect(mock.WAN(wanPrimary)).To(Succeed())
		})

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, hello)).To(BeEquivalentTo(baseline))
		}).Should(Succeed(), "the workload stayed down after a recovery the operator never observed as an edge")
		Expect(annotationsOf(Default, hello)).NotTo(HaveKey(annotationBaseline))
	})
})
