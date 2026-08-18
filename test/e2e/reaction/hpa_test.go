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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A real HorizontalPodAutoscaler and a real Automation over one Deployment.
//
// This is the spec a unit test cannot stand in for, and not because the logic
// is hard: the claim being made is about a permission, an API group, and a
// scaleTargetRef written by the API server rather than by the test. A fake
// client would prove that the code matches itself.
//
// The HPA never has to actually scale anything for this to be the real thing —
// its existence is the claim on spec.replicas, and Reactor's job is to notice
// the claim before it starts a fight it cannot win.
var _ = Describe("Declining a workload a HorizontalPodAutoscaler drives", Ordered, func() {
	const (
		target   = "autoscaled"
		hpa      = "autoscaled-hpa"
		shed     = "shed-the-autoscaled"
		other    = "not-autoscaled"
		baseline = 3
	)

	onBattery := map[string]string{keyUPS: upsOnBattery}
	automation := scaleAutomation(shed, onBattery, target, 0)

	BeforeAll(func() {
		Expect(cluster.Apply(workload(target, baseline))).To(Succeed())
		Expect(cluster.Apply(horizontalPodAutoscaler(hpa, target))).To(Succeed())
		Expect(cluster.Apply(automation)).To(Succeed())
	})

	AfterAll(func() {
		resetConsole()
		Expect(cluster.Delete(automation)).To(Succeed())
		_, _ = cluster.Kubectl("-n", appsNamespace, "delete", "hpa", hpa, "--ignore-not-found")
	})

	It("does not touch the workload, and records no claim on it", func() {
		generation := generationOf(Default, target)
		Expect(mock.UPS("mode=battery&level=80")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, shed).Status.Matching).To(HaveValue(BeTrue()))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(baseline),
				"Reactor started the fight with the HPA that #50 is about")
			g.Expect(generationOf(g, target)).To(Equal(generation))
			g.Expect(annotationsOf(g, target)).NotTo(HaveKey(annotationBaseline))
			g.Expect(annotationsOf(g, target)).NotTo(HaveKey(annotationClaimedBy))
		}).Should(Succeed())
	})

	It("names the HPA responsible, and is still Ready", func() {
		Eventually(func(g Gomega) {
			shedding := automationOf(g, shed)
			entry := targetStatus(shedding, target)
			g.Expect(entry).NotTo(BeNil())
			g.Expect(entry.ManagedBy).To(Equal(
				fmt.Sprintf("HorizontalPodAutoscaler/%s/%s", appsNamespace, hpa)))

			applied := conditionOf(shedding, conditionApplied)
			g.Expect(applied).NotTo(BeNil())
			g.Expect(applied.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(applied.Reason).To(Equal("TargetManagedByHPA"))
			g.Expect(conditionOf(shedding, conditionReady)).To(HaveField("Status", metav1.ConditionTrue))
		}).Should(Succeed())
	})

	It("says so in a Warning Event, so it is visible without reading status", func() {
		Eventually(func(g Gomega) {
			out, err := eventsWithReason("TargetManagedByHPA")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("Warning"),
				"declining is not the arbitration working, unlike being outvoted by a peer")
			g.Expect(out).To(ContainSubstring(hpa))
		}).Should(Succeed())
	})

	It("still acts on a workload in the same namespace that no HPA drives", func() {
		Expect(cluster.Apply(workload(other, 2))).To(Succeed())
		second := scaleAutomation("shed-the-other", onBattery, other, 0)
		Expect(cluster.Apply(second)).To(Succeed())
		DeferCleanup(func() { Expect(cluster.Delete(second)).To(Succeed()) })

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, other)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "declining one target made Reactor decline a namespace-mate too")
	})

	It("hands a workload it was already holding back when an HPA appears over it", func() {
		By("starting from a workload Reactor genuinely holds")
		Expect(cluster.Apply(workload(other, 2))).To(Succeed())
		held := scaleAutomation("shed-the-held", onBattery, other, 0)
		Expect(cluster.Apply(held)).To(Succeed())
		DeferCleanup(func() { Expect(cluster.Delete(held)).To(Succeed()) })

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, other)).To(BeEquivalentTo(0))
			g.Expect(annotationsOf(g, other)).To(HaveKeyWithValue(annotationBaseline, "2"))
		}).Should(Succeed())

		By("adding an autoscaler over it, as somebody would during the outage")
		Expect(cluster.Apply(horizontalPodAutoscaler("held-hpa", other))).To(Succeed())
		DeferCleanup(func() {
			_, _ = cluster.Kubectl("-n", appsNamespace, "delete", "hpa", "held-hpa", "--ignore-not-found")
		})

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, other)).To(BeEquivalentTo(2),
				"left at 0 an HPA cannot recover it — an HPA does not scale a workload up from zero")
			g.Expect(annotationsOf(g, other)).NotTo(HaveKey(annotationBaseline))
			g.Expect(annotationsOf(g, other)).NotTo(HaveKey(annotationClaimedBy))
		}).Should(Succeed())

		By("and staying let go rather than reclaiming on the next pass")
		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, other)).To(BeEquivalentTo(2))
		}).Should(Succeed())
	})

	It("claims the workload the moment the HPA is deleted", func() {
		_, err := cluster.Kubectl("-n", appsNamespace, "delete", "hpa", hpa)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, target)).To(BeEquivalentTo(0))
			g.Expect(annotationsOf(g, target)).To(HaveKeyWithValue(annotationBaseline, "3"))
			g.Expect(targetStatus(automationOf(g, shed), target).ManagedBy).To(BeEmpty())
		}).Should(Succeed())
	})
})

// horizontalPodAutoscaler renders an HPA over a Deployment. It targets CPU it
// will never be able to read — there is no metrics-server in the Kind cluster —
// which is fine and deliberate: what is under test is that its existence is a
// claim on spec.replicas, not what it would compute.
func horizontalPodAutoscaler(name, target string) string {
	return fmt.Sprintf(`apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: %[3]s
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Resource
      resource:
        name: cpu
        target: {type: Utilization, averageUtilization: 80}
`, name, appsNamespace, target)
}
