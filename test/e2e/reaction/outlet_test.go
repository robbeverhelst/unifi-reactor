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
// The two answers are told apart by which Automation fires, which is the same
// way an operator would find out. `shed-when-outlet-off` names one outlet;
// `shed-when-bank-off` names all four in its relay group. Asking the console
// for outlet 5 fires the first under either hypothesis and the second only
// under the dangerous one — where "switch outlet 5" means "cut outlets 5 to 8".
//
// It is also the only e2e block whose console starts from a fully populated
// capture rather than from absent fields: outlet_table is real, all eight
// relays are closed, and outlets 1-4 and 5-8 are in two relay groups.
//
// Nothing here writes an outlet. There is no way to; that is #23.
var _ = Describe("Observing UPS outlets", Ordered, func() {
	const (
		watchJob     = "outlet-watch"
		outletJob    = "outlet-batch"
		bankJob      = "bank-batch"
		namedJob     = "named-batch"
		watchPolicy  = "watch-every-outlet"
		outletPolicy = "shed-when-outlet-off"
		bankPolicy   = "shed-when-bank-off"
		namedPolicy  = "shed-when-nas-outlet-off"
		baseline     = 2
	)

	// The captured banks. Outlet 5 lives in bankTwo; bankOne staying put is
	// what makes a group switch a relay group rather than the UPS dropping
	// everything.
	bankOne := []string{"outlet.1", "outlet.2", "outlet.3", "outlet.4"}
	bankTwo := []string{keyOutlet, "outlet.6", "outlet.7", "outlet.8"}

	// An Automation's status.observedState carries the keys it NAMES, so
	// watching all eight takes an Automation that names all eight. It also
	// makes the captured console's own answer — every relay closed — something
	// a condition can hold rather than something only a log line knows.
	everyOutletOn := map[string]string{}
	for _, key := range append(append([]string{}, bankOne...), bankTwo...) {
		everyOutletOn[key] = outletOn
	}
	wholeBankOff := map[string]string{}
	for _, key := range bankTwo {
		wholeBankOff[key] = outletOff
	}
	outletCut := map[string]string{keyOutlet: outletOff}
	namedCut := map[string]string{keyOutletNamed: outletOff}

	policies := []struct {
		name   string
		when   map[string]string
		target string
	}{
		{watchPolicy, everyOutletOn, watchJob},
		{outletPolicy, outletCut, outletJob},
		{bankPolicy, wholeBankOff, bankJob},
	}

	BeforeAll(func() {
		resetConsole()
		for _, policy := range policies {
			Expect(cluster.Apply(workload(policy.target, baseline))).To(Succeed())
			Expect(cluster.Apply(scaleAutomation(policy.name, policy.when, policy.target, 0))).To(Succeed())
		}
		Eventually(func(g Gomega) {
			for _, policy := range policies {
				g.Expect(conditionOf(automationOf(g, policy.name), conditionReady)).NotTo(BeNil())
			}
		}).Should(Succeed())
	})

	AfterAll(func() {
		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, bankJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
		for _, policy := range policies {
			Expect(cluster.Delete(scaleAutomation(policy.name, policy.when, policy.target, 0))).To(Succeed())
		}
		resetConsole()
	})

	// Eight keys, all on, addressed by index — because the console has named
	// none of them, which is exactly the state a real UPS ships in.
	It("publishes one key per outlet, addressed by index while nothing is named", func() {
		Eventually(func(g Gomega) {
			state := automationOf(g, watchPolicy).Status.ObservedState
			for key := range everyOutletOn {
				g.Expect(state).To(HaveKeyWithValue(key, outletOn))
			}
			// Every relay closed is the captured console's own answer, so the
			// condition naming all eight holds and its target is shed.
			g.Expect(replicasOf(g, watchJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())
	})

	// H1's first answer: one outlet moves and its three bank-mates do not, so
	// the automation naming the whole bank never fires. If this is what a real
	// console does, #23 can ship with a per-outlet allowlist.
	It("reacts to one outlet opening, and leaves the rest of its relay group alone", func() {
		Expect(mock.Outlets("switching=individual&outlet=5&state=off")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(0))
			// The bank automation has seen the same observation — its own key
			// moved — and did not fire, which is the assertion rather than a
			// race: the other three are still closed.
			bank := automationOf(g, bankPolicy).Status.ObservedState
			g.Expect(bank).To(HaveKeyWithValue(keyOutlet, outletOff))
			for _, stillOn := range bankTwo[1:] {
				g.Expect(bank).To(HaveKeyWithValue(stillOn, outletOn))
			}
			g.Expect(replicasOf(g, bankJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())

		Expect(mock.Outlets("outlet=5&state=on")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// H1's second answer, and the dangerous one: the same request for outlet 5
	// takes outlets 5-8 with it, so both automations fire. An automation
	// written as "switch outlet 5" would mean "cut outlets 5 to 8", which is
	// why #23 is deferred rather than implemented behind a warning.
	It("observes a whole relay group moving at once when one outlet is asked for", func() {
		Expect(mock.Outlets("switching=group&outlet=5&state=off")).To(Succeed())

		Eventually(func(g Gomega) {
			bank := automationOf(g, bankPolicy).Status.ObservedState
			for _, key := range bankTwo {
				g.Expect(bank).To(HaveKeyWithValue(key, outletOff))
			}
			g.Expect(replicasOf(g, bankJob)).To(BeEquivalentTo(0))
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(0))

			// The other bank is untouched, which is what makes this a relay
			// group rather than the UPS simply dropping everything.
			watched := automationOf(g, watchPolicy).Status.ObservedState
			for _, key := range bankOne {
				g.Expect(watched).To(HaveKeyWithValue(key, outletOn))
			}
		}).Should(Succeed())

		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, bankJob)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// Naming an outlet on the console moves its key, which is the whole reason
	// to name them — and is also why an automation written against an index is
	// fragile. The old key vanishing is read as lost visibility, so the claim
	// is held rather than released, exactly as a renamed device's is.
	It("moves the key when an outlet is named, holding the old key's claim", func() {
		Expect(mock.Outlets("outlet=5&state=off")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		Expect(cluster.Apply(workload(namedJob, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(namedPolicy, namedCut, namedJob, 0))).To(Succeed())

		By("naming outlet 5, which moves its key from outlet.5 to outlet.nas")
		Expect(mock.Outlets("outlet=5&label=NAS")).To(Succeed())

		Eventually(func(g Gomega) {
			named := automationOf(g, namedPolicy).Status.ObservedState
			g.Expect(named).To(HaveKeyWithValue(keyOutletNamed, outletOff))
			g.Expect(replicasOf(g, namedJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("holding rather than releasing the automation whose key went away")
		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, outletPolicy), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"),
				"an outlet renamed out from under a key is lost visibility, not a recovery")
		}).Should(Succeed())
		Expect(replicasOf(Default, outletJob)).To(BeEquivalentTo(0))

		Expect(cluster.Delete(scaleAutomation(namedPolicy, namedCut, namedJob, 0))).To(Succeed())
		Expect(mock.Outlets("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, outletJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// A UPS that reports no outlet table publishes no outlet key, rather than a
	// row of "on" that would read as eight healthy relays.
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
