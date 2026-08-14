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

// Charge ignores load, and load is most of the answer: 30% at 300W and 30% at
// 900W are very different situations. These specs hold the battery level
// constant throughout and move only the runtime and the draw, so what they
// prove is that the new keys are genuinely independent axes rather than
// another reading of ups.battery.
var _ = Describe("Reacting to UPS runtime and load", Ordered, func() {
	const (
		batch    = "nightly-batch"
		shutdown = "runtime-shutdown"
		baseline = 2
		// A comfortable charge for the whole block. Nothing below is allowed
		// to depend on it moving.
		healthyCharge = "level=90"
	)

	when := map[string]string{keyUPSRuntime: upsRuntimeCritical}

	BeforeAll(func() {
		Expect(cluster.Apply(workload(batch, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(shutdown, when, batch, 0))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(conditionOf(automationOf(g, shutdown), conditionReady)).NotTo(BeNil())
		}).Should(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Eventually(func(g Gomega) { g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(baseline)) }).Should(Succeed())
		Expect(cluster.Delete(scaleAutomation(shutdown, when, batch, 0))).To(Succeed())
	})

	It("reports the captured UPS as having ample runtime and a normal load", func() {
		Expect(mock.UPS("mode=battery&" + healthyCharge)).To(Succeed())

		Eventually(func(g Gomega) {
			state := automationOf(g, shutdown).Status.ObservedState
			g.Expect(state).To(HaveKeyWithValue(keyUPSRuntime, upsRuntimeAmple))
			g.Expect(state).To(HaveKeyWithValue(keyUPSLoad, upsLoadNormal))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	It("reports a high load without touching the runtime bucket", func() {
		Expect(mock.UPS("output=850")).To(Succeed())

		Eventually(func(g Gomega) {
			state := automationOf(g, shutdown).Status.ObservedState
			g.Expect(state).To(HaveKeyWithValue(keyUPSLoad, upsLoadHigh))
			g.Expect(state).To(HaveKeyWithValue(keyUPSRuntime, upsRuntimeAmple))
		}).Should(Succeed())
	})

	It("shuts the workload down on runtime, at a charge that says nothing is wrong", func() {
		Expect(mock.UPS("runtime=60")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("with the battery still comfortable: this is the case ups.battery cannot express")
		state := automationOf(Default, shutdown).Status.ObservedState
		Expect(state).To(HaveKeyWithValue(keyUPSRuntime, upsRuntimeCritical))
		Expect(state).To(HaveKeyWithValue("ups.battery", "normal"))
	})

	// The narrower version of a UPS dropping off the console: it is still
	// there and still reports charge, but offers no runtime estimate. Only
	// ups.runtime may disappear, and the claim it is holding must survive.
	It("holds its claim when the UPS stops estimating runtime", func() {
		Expect(mock.UPS("runtime=0")).To(Succeed())

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, shutdown), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"))
			g.Expect(ready.Message).To(ContainSubstring(keyUPSRuntime))
		}).Should(Succeed())

		By("keeping the keys around it, which are a separate observation")
		state := automationOf(Default, shutdown).Status.ObservedState
		Expect(state).To(HaveKeyWithValue(keyUPS, upsOnBattery))
		Expect(state).To(HaveKeyWithValue(keyUPSLoad, upsLoadHigh))

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "losing the runtime estimate is not the outage ending")
	})

	It("restores the workload once the UPS reports runtime again", func() {
		Expect(mock.UPS("runtime=1043&output=310")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})
})
