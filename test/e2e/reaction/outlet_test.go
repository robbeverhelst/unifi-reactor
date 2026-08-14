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
)

// The outlet keys are the instrument #23 is waiting on, so this file proves the
// instrument works end to end: rehearsed console, poll, condition, action —
// under BOTH answers to the question nobody has asked the hardware yet.
//
// It is the only e2e block whose console starts from a fully populated capture
// rather than from absent fields: outlet_table is real, all eight relays are
// closed, and outlets 1-4 and 5-8 are in two relay groups.
//
// Nothing here writes an outlet. There is no way to; that is #23.
var _ = Describe("Observing UPS outlets", Ordered, func() {
	const (
		outletJob    = "outlet-batch"
		outletPolicy = "shed-when-outlet-off"
		namedPolicy  = "shed-when-nas-outlet-off"
		baseline     = 2
	)

	outletCut := map[string]string{keyOutlet: outletOff}
	namedCut := map[string]string{keyOutletNamed: outletOff}

	// The captured banks, spelled once. Outlet 5 lives in bankTwo, and bankOne
	// staying put is what makes a group switch a relay group rather than the
	// UPS simply dropping everything.
	bankOne := []string{"outlet.1", "outlet.2", "outlet.3", "outlet.4"}
	bankTwo := []string{"outlet.5", "outlet.6", "outlet.7", "outlet.8"}
	bankTwoOthers := bankTwo[1:]

	BeforeAll(func() {
		resetConsole()
		Expect(cluster.Apply(workload(outletJob, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(outletPolicy, outletCut, outletJob, 0))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(conditionOf(automationOf(g, outletPolicy), conditionReady)).NotTo(BeNil())
		}).Should(Succeed())
	})

	AfterAll(func() {
		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
		Expect(cluster.Delete(scaleAutomation(outletPolicy, outletCut, outletJob, 0))).To(Succeed())
		resetConsole()
	})

	// Eight keys, all on, addressed by index — because the console has named
	// none of them, which is exactly the state a real UPS ships in.
	It("publishes one key per outlet, addressed by index while nothing is named", func() {
		Eventually(func(g Gomega) {
			state := automationOf(g, outletPolicy).Status.ObservedState
			for _, key := range append(append([]string{}, bankOne...), bankTwo...) {
				g.Expect(state).To(HaveKeyWithValue(key, outletOn))
			}
		}).Should(Succeed())
	})

	// H1's first answer: one outlet moves and its three bank-mates do not. If
	// this is what a real console does, #23 can ship a per-outlet allowlist.
	It("reacts to one outlet opening, and leaves the rest of its relay group alone", func() {
		Expect(mock.Outlets("switching=individual&outlet=5&state=off")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		state := automationOf(Default, outletPolicy).Status.ObservedState
		Expect(state).To(HaveKeyWithValue(keyOutlet, outletOff))
		for _, stillOn := range bankTwoOthers {
			Expect(state).To(HaveKeyWithValue(stillOn, outletOn),
				"switching one outlet must not move the rest of its relay group")
		}

		Expect(mock.Outlets("outlet=5&state=on")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// H1's second answer, and the dangerous one: the same request for outlet 5
	// takes outlets 5-8 with it. An automation written as "switch outlet 5"
	// would mean "cut outlets 5 to 8", which is why #23 is deferred rather than
	// implemented behind a warning.
	It("observes a whole relay group moving at once when one outlet is asked for", func() {
		Expect(mock.Outlets("switching=group&outlet=5&state=off")).To(Succeed())

		Eventually(func(g Gomega) {
			state := automationOf(g, outletPolicy).Status.ObservedState
			for _, key := range bankTwo {
				g.Expect(state).To(HaveKeyWithValue(key, outletOff))
			}
			// The other bank is untouched, which is what makes this a relay
			// group rather than the UPS simply dropping everything.
			for _, key := range bankOne {
				g.Expect(state).To(HaveKeyWithValue(key, outletOn))
			}
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// Naming an outlet on the console moves its key, which is the whole reason
	// to name them — and it is also the reason an automation written against an
	// index is fragile. The old key vanishing is read as lost visibility, so the
	// claim is held rather than released, exactly as a renamed device is.
	It("moves the key when an outlet is named, holding the old key's claim", func() {
		Expect(mock.Outlets("outlet=5&state=off")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		Expect(cluster.Apply(workload(outletJob+"-named", baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(namedPolicy, namedCut, outletJob+"-named", 0))).To(Succeed())

		By("naming outlet 5, which moves its key from outlet.5 to outlet.nas")
		Expect(mock.Outlets("outlet=5&label=NAS")).To(Succeed())

		Eventually(func(g Gomega) {
			named := automationOf(g, namedPolicy).Status.ObservedState
			g.Expect(named).To(HaveKeyWithValue(keyOutletNamed, outletOff))
			g.Expect(named).NotTo(HaveKey(keyOutlet))
			g.Expect(replicasOf(g, outletJob+"-named")).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("holding rather than releasing the automation whose key went away")
		ready := conditionOf(automationOf(Default, outletPolicy), conditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("StateKeyUnavailable"),
			"an outlet renamed out from under a key is lost visibility, not a recovery")
		Expect(replicasOf(Default, outletJob)).To(BeEquivalentTo(0))

		Expect(cluster.Delete(scaleAutomation(namedPolicy, namedCut, outletJob+"-named", 0))).To(Succeed())
		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// A UPS that reports no outlet table publishes no outlet key, rather than a
	// row of "on" that reads as eight healthy relays.
	It("publishes nothing when the UPS reports no outlets", func() {
		Expect(mock.Outlets("present=false")).To(Succeed())

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, outletPolicy), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"))
		}).Should(Succeed())

		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, outletPolicy).Status.ObservedState).
				To(HaveKeyWithValue(keyOutlet, outletOn))
		}).Should(Succeed())
	})
})
