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
// 900W are very different situations.
//
// The shutdown Automation states issue #7's argument as its condition — on
// battery, the charge says nothing is wrong, and we are about to run out — so
// the shed firing at a healthy battery level is the evidence that runtime, not
// charge, drove it. status.observedState holds the subset of provider state an
// Automation references, so each key asserted below is matched on by the
// Automation it is read from.
var _ = Describe("Reacting to UPS runtime and load", Ordered, func() {
	const (
		batch     = "nightly-batch"
		transcode = "bulk-transcode"
		shutdown  = "runtime-shutdown"
		loadShed  = "load-shed"
		baseline  = 2
		// A comfortable charge held for the whole block. Nothing here is
		// allowed to depend on it moving.
		healthyCharge = "mode=battery&level=90"
	)

	// "On battery, the charge looks fine, and we are about to run out."
	aboutToRunOut := map[string]string{
		keyUPS:        upsOnBattery,
		keyUPSBattery: batteryNormal,
		keyUPSRuntime: upsRuntimeCritical,
	}
	heavy := map[string]string{keyUPSLoad: upsLoadHigh}

	BeforeAll(func() {
		resetConsole()
		Expect(cluster.Apply(workload(batch, baseline))).To(Succeed())
		Expect(cluster.Apply(workload(transcode, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(shutdown, aboutToRunOut, batch, 0))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(loadShed, heavy, transcode, 0))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(conditionOf(automationOf(g, shutdown), conditionReady)).NotTo(BeNil())
			g.Expect(conditionOf(automationOf(g, loadShed), conditionReady)).NotTo(BeNil())
		}).Should(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, transcode)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
		Expect(cluster.Delete(scaleAutomation(shutdown, aboutToRunOut, batch, 0))).To(Succeed())
		Expect(cluster.Delete(scaleAutomation(loadShed, heavy, transcode, 0))).To(Succeed())
	})

	It("reports the captured UPS as having ample runtime and a normal load", func() {
		Expect(mock.UPS(healthyCharge)).To(Succeed())

		Eventually(func(g Gomega) {
			state := automationOf(g, shutdown).Status.ObservedState
			g.Expect(state).To(HaveKeyWithValue(keyUPS, upsOnBattery))
			g.Expect(state).To(HaveKeyWithValue(keyUPSBattery, batteryNormal))
			g.Expect(state).To(HaveKeyWithValue(keyUPSRuntime, upsRuntimeAmple))
			g.Expect(automationOf(g, loadShed).Status.ObservedState).
				To(HaveKeyWithValue(keyUPSLoad, upsLoadNormal))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, transcode)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	It("sheds on a high load without touching the runtime bucket", func() {
		Expect(mock.UPS("output=850")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, transcode)).To(BeEquivalentTo(0))
		}).Should(Succeed())
		Expect(automationOf(Default, loadShed).Status.ObservedState).To(HaveKeyWithValue(keyUPSLoad, upsLoadHigh))

		By("leaving the runtime alone: they are independent axes, not one ladder")
		Expect(automationOf(Default, shutdown).Status.ObservedState).
			To(HaveKeyWithValue(keyUPSRuntime, upsRuntimeAmple))
		Expect(replicasOf(Default, batch)).To(BeEquivalentTo(baseline))
	})

	It("shuts the workload down on runtime, at a charge that says nothing is wrong", func() {
		Expect(mock.UPS("runtime=60")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("with the battery still comfortable: this is the case ups.battery cannot express")
		state := automationOf(Default, shutdown).Status.ObservedState
		Expect(state).To(HaveKeyWithValue(keyUPSRuntime, upsRuntimeCritical))
		Expect(state).To(HaveKeyWithValue(keyUPSBattery, batteryNormal))
		Expect(state).To(HaveKeyWithValue(keyUPS, upsOnBattery))
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
		Expect(state).To(HaveKeyWithValue(keyUPSBattery, batteryNormal))
		Expect(automationOf(Default, loadShed).Status.ObservedState).To(HaveKeyWithValue(keyUPSLoad, upsLoadHigh))

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "losing the runtime estimate is not the outage ending")
	})

	It("restores both workloads once the UPS reports runtime and a normal load again", func() {
		Expect(mock.UPS("runtime=1043&output=310")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, batch)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, transcode)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})
})
