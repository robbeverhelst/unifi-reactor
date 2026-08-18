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

// A power failure is the case the whole "hold, don't guess" rule exists for:
// the hardware reporting the outage is itself on the failing power, and losing
// sight of it must never be read as the outage having ended.
var _ = Describe("Reacting to a power failure", Ordered, func() {
	const (
		media    = "media"
		core     = "core"
		upsShed  = "ups-shed"
		critical = "ups-critical"
		baseline = 3
	)

	onBattery := map[string]string{keyUPS: upsOnBattery}
	onCritical := map[string]string{keyUPS: upsOnBattery, "ups.battery": "critical"}

	BeforeAll(func() {
		Expect(cluster.Apply(workload(media, baseline))).To(Succeed())
		Expect(cluster.Apply(workload(core, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(upsShed, onBattery, media, 0))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(critical, onCritical, core, 1))).To(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Expect(cluster.Delete(scaleAutomation(upsShed, onBattery, media, 0))).To(Succeed())
		Expect(cluster.Delete(scaleAutomation(critical, onCritical, core, 1))).To(Succeed())
	})

	It("sheds on battery power without escalating while the charge is healthy", func() {
		Expect(mock.UPS("mode=battery&level=80")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, media)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("leaving the escalation alone: one of its two keys does not hold")
		automation := automationOf(Default, critical)
		Expect(automation.Status.Matching).To(HaveValue(BeFalse()))
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue(keyUPS, upsOnBattery))
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue("ups.battery", "normal"))
		Expect(replicasOf(Default, core)).To(BeEquivalentTo(baseline))
	})

	It("holds its claim when the UPS stops being reported at all", func() {
		Expect(mock.UPS("present=false")).To(Succeed())

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, upsShed), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"))
			g.Expect(ready.Message).To(ContainSubstring("ups"))
		}).Should(Succeed())

		By("still considering itself matched, because losing sight is not evidence the outage ended")
		Expect(automationOf(Default, upsShed).Status.Matching).To(HaveValue(BeTrue()))

		By("and leaving the workload down for as long as it cannot see")
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, media)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "the reversal fired on a key that went missing, not on an outage that ended")
	})

	It("picks the state back up when the UPS is reported again", func() {
		Expect(mock.UPS("present=true")).To(Succeed())

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, upsShed), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())
		Expect(replicasOf(Default, media)).To(BeEquivalentTo(0))
	})

	It("does not escalate while only one of the two keys holds", func() {
		before := generationOf(Default, core)
		Expect(mock.UPS("level=25")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, critical).Status.ObservedState).To(HaveKeyWithValue("ups.battery", "low"))
		}).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, core)).To(BeEquivalentTo(baseline))
			g.Expect(generationOf(g, core)).To(Equal(before))
		}, 10*time.Second).Should(Succeed(), "a partially matched multi-key condition acted anyway")
	})

	It("escalates once the battery reaches critical, on both keys together", func() {
		Expect(mock.UPS("level=5")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, core)).To(BeEquivalentTo(1))
		}).Should(Succeed())

		automation := automationOf(Default, critical)
		Expect(automation.Status.Matching).To(HaveValue(BeTrue()))
		Expect(automation.Status.ObservedState).To(HaveKeyWithValue("ups.battery", "critical"))
		Expect(targetStatus(automation, core).Effective).To(HaveValue(BeEquivalentTo(1)))

		By("without disturbing what the coarser automation is already holding")
		Expect(replicasOf(Default, media)).To(BeEquivalentTo(0))
	})

	It("restores both workloads when mains power returns", func() {
		Expect(mock.UPS("mode=mains&level=100")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, media)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, core)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
		Expect(annotationsOf(Default, media)).NotTo(HaveKey(annotationBaseline))
		Expect(annotationsOf(Default, core)).NotTo(HaveKey(annotationBaseline))
	})
})
