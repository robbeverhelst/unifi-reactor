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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A dry run applied beside an automation that is live.
//
// The property a unit test cannot reach is the one that matters here: a real
// API server, a real workload, and a real peer holding it — and the dry run
// still not moving anything. Its whole claim to safety is that being applied
// is not an act.
var _ = Describe("Previewing an automation without running it", Ordered, func() {
	const (
		target   = "previewed"
		live     = "live-on-battery"
		previews = "preview-on-backup-wan"
		baseline = 2
		liveWant = 1
	)

	onBattery := map[string]string{keyUPS: upsOnBattery}
	onBackup := map[string]string{keyWAN: wanBackup}

	BeforeAll(func() {
		Expect(cluster.Apply(workload(target, baseline))).To(Succeed())
		Expect(cluster.Apply(scaleAutomation(live, onBattery, target, liveWant))).To(Succeed())
		Expect(cluster.Apply(dryRunAutomation(previews, onBackup, target, 0))).To(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Expect(cluster.Delete(scaleAutomation(live, onBattery, target, liveWant))).To(Succeed())
		Expect(cluster.Delete(dryRunAutomation(previews, onBackup, target, 0))).To(Succeed())
	})

	It("says what it would do before its condition has ever held", func() {
		// The case the feature exists for: the automation is written on an
		// afternoon when nothing is wrong, and the question is what it will do
		// when something is.
		Eventually(func(g Gomega) {
			automation := automationOf(g, previews)
			g.Expect(automation.Status.Matching).To(BeFalse())

			entry := targetStatus(automation, target)
			g.Expect(entry).NotTo(BeNil())
			g.Expect(entry.Preview).NotTo(BeNil())
			g.Expect(entry.Preview.Desired).To(HaveValue(BeEquivalentTo(0)))
			g.Expect(entry.Preview.Effective).To(HaveValue(BeEquivalentTo(0)))
			g.Expect(entry.Preview.Level).To(Equal("0 replicas"))
			g.Expect(entry.Preview.OnExit).To(Equal(fmt.Sprintf("%d replicas", baseline)))
		}).Should(Succeed())

		By("and being healthy while it does it")
		automation := automationOf(Default, previews)
		Expect(conditionOf(automation, conditionReady)).To(HaveField("Status", metav1.ConditionTrue))
		applied := conditionOf(automation, conditionApplied)
		Expect(applied).NotTo(BeNil())
		Expect(applied.Status).To(Equal(metav1.ConditionFalse))
		Expect(applied.Reason).To(Equal("DryRun"))
	})

	It("does not touch the workload when its condition starts holding", func() {
		generation := generationOf(Default, target)
		Expect(mock.WAN(wanBackup)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, previews).Status.Matching).To(BeTrue())
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(baseline))
			g.Expect(generationOf(g, target)).To(Equal(generation),
				"a dry run wrote to its target, which is the one thing it must never do")
			g.Expect(annotationsOf(g, target)).NotTo(HaveKey(annotationBaseline))
			g.Expect(annotationsOf(g, target)).NotTo(HaveKey(annotationClaimedBy))
		}).Should(Succeed())
	})

	It("says what it almost did, in an Event", func() {
		Eventually(func(g Gomega) {
			out, err := eventsWithReason("DryRun")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("Normal"), "a dry run is not a fault")
			g.Expect(out).To(ContainSubstring("nothing was written"))
			g.Expect(out).To(ContainSubstring("0 replicas"))
		}).Should(Succeed())
	})

	It("cannot change what the live automation resolves to", func() {
		By("putting the live automation into its own state, while the dry run still matches")
		Expect(mock.UPS("mode=battery&level=80")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(liveWant))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(liveWant),
				"the dry run's more restrictive claim was folded into a live arbitration")
		}).Should(Succeed())

		By("and the live automation getting exactly what it asked for")
		winner := automationOf(Default, live)
		Expect(targetStatus(winner, target).DeferredBy).To(BeEmpty())
		Expect(conditionOf(winner, conditionApplied)).To(HaveField("Status", metav1.ConditionTrue))
		Expect(annotationsOf(Default, target)).To(HaveKeyWithValue(annotationClaimedBy, claimant(live)))
	})

	It("names the peer it would outvote, and what the target is actually held at meanwhile", func() {
		Eventually(func(g Gomega) {
			entry := targetStatus(automationOf(g, previews), target)
			g.Expect(entry).NotTo(BeNil())
			g.Expect(entry.Effective).To(HaveValue(BeEquivalentTo(liveWant)),
				"what somebody else is holding the target at")
			g.Expect(entry.Preview).NotTo(BeNil())
			g.Expect(entry.Preview.Effective).To(HaveValue(BeEquivalentTo(0)),
				"and what it would be held at if this automation counted")
			g.Expect(entry.Preview.WouldDefer).To(ConsistOf(claimant(live)))
			g.Expect(entry.Preview.DeferredBy).To(BeEmpty())
		}).Should(Succeed())
	})

	It("starts acting the moment the dry run is turned off, with nothing else to change", func() {
		Expect(cluster.Apply(scaleAutomation(previews, onBackup, target, 0))).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(0))
			entry := targetStatus(automationOf(g, previews), target)
			g.Expect(entry).NotTo(BeNil())
			g.Expect(entry.Preview).To(BeNil(), "an automation in force describes what is, not what would be")
			g.Expect(entry.Effective).To(HaveValue(BeEquivalentTo(0)))
		}).Should(Succeed())

		By("and the peer it outvoted saying so, exactly as the preview said it would")
		Eventually(func(g Gomega) {
			g.Expect(targetStatus(automationOf(g, live), target).DeferredBy).To(ConsistOf(claimant(previews)))
		}).Should(Succeed())
	})
})

// eventsWithReason returns one line per Event carrying a reason, as
// "<type> | <message>".
//
// jsonpath rather than -o yaml or custom-columns, and both alternatives are
// traps rather than preferences. `kubectl get events` serves the core/v1 view of
// the storage the manager writes through events.k8s.io/v1, and the two spell the
// same field differently — .note there is .message here — so asking for the
// wrong one yields "<none>" on every row rather than an error. YAML wraps a long
// message across lines, which quietly breaks matching on any phrase longer than
// a few words.
func eventsWithReason(reason string) (string, error) {
	return cluster.Kubectl("-n", appsNamespace, "get", "events",
		"--field-selector", "reason="+reason,
		"-o", `jsonpath={range .items[*]}{.type}{" | "}{.message}{"\n"}{end}`)
}

// dryRunAutomation is scaleAutomation with spec.dryRun set, so the same
// automation can be applied twice — once to be told what it would do, and once
// to let it do it.
func dryRunAutomation(name string, when map[string]string, target string, replicas int) string {
	live := scaleAutomation(name, when, target, replicas)
	return strings.Replace(live, "spec:\n", "spec:\n  dryRun: true\n", 1)
}
