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

package lifecycle

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/robbeverhelst/unifi-reactor/test/e2e/harness"
)

// timeline is what an uninstall looked like from outside, sampled while Helm
// was running it.
type timeline struct {
	// sawJob is whether the release-claims hook was ever observed at all.
	// Without it the ordering below would be satisfied vacuously.
	sawJob bool
	// jobRanWithoutOperator is the violation: the hook still present while the
	// operator's Deployment — and with it the release's ServiceAccount and
	// RBAC — had already been removed.
	jobRanWithoutOperator bool
	// samples is how many observations the watch managed, for diagnostics when
	// an assertion about what it saw fails.
	samples int
}

// watchUninstall samples the release namespace while Helm works, so the suite
// can say something about the order Helm did things in rather than only about
// where it ended up.
func watchUninstall(namespace string, done <-chan struct{}) <-chan timeline {
	result := make(chan timeline, 1)
	go func() {
		defer GinkgoRecover()
		var observed timeline
		quiet := cluster.Quiet()
		for {
			select {
			case <-done:
				result <- observed
				return
			default:
			}
			out, _ := quiet.Kubectl("get", "job,deploy", "-n", namespace, "-o", "name")
			observed.samples++
			job := strings.Contains(out, "job.batch/"+releaseJob)
			operator := strings.Contains(out, "deployment.apps/"+release+"\n") ||
				strings.HasSuffix(out, "deployment.apps/"+release)
			if job {
				observed.sawJob = true
				if !operator {
					observed.jobRanWithoutOperator = true
				}
			}
			time.Sleep(150 * time.Millisecond)
		}
	}()
	return result
}

// declareUninstallSpecs describes one uninstall, so the cluster-wide and
// namespace-scoped RBAC modes are held to exactly the same standard. The
// namespaced mode is the one at risk: releasing claims has to enumerate
// Automations, and a namespaced Role does not permit doing that at cluster
// scope.
func declareUninstallSpecs(name, namespace string, values ...string) {
	Describe(name, Ordered, func() {
		const (
			app        = "stranded"
			automation = "wan-shed"
			baseline   = 3
		)
		var uninstallOutput string
		var observed timeline

		BeforeAll(func() {
			removeCRD()
			installOperator(namespace, values...)
			Expect(cluster.Apply(workload(namespace, app, baseline))).To(Succeed())
			Expect(cluster.Apply(wanAutomation(namespace, automation, app))).To(Succeed())
		})

		AfterAll(func() {
			_, _ = cluster.Helm("uninstall", release, "--namespace", namespace, "--ignore-not-found")
			removeCRD()
			_, _ = cluster.Kubectl("delete", "namespace", namespace, "--wait=false")
		})

		It("holds a workload down, and records the finalizer that will hand it back", func() {
			Expect(mock.WAN("backup")).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(replicasOf(g, namespace, app)).To(BeEquivalentTo(0))
			}).Should(Succeed())
			Expect(annotationsOf(Default, namespace, app)).To(HaveKeyWithValue(annotationBaseline, "3"))
			Expect(automationOf(Default, namespace, automation).Finalizers).To(ContainElement(finalizer))
		})

		// The operator reads action credentials with mgr.GetAPIReader(), which
		// is a client built straight against the API server rather than one
		// backed by the cache. Restricting that cache to a single namespace
		// therefore should not reach it — but "should not" is what a cluster
		// is for, and the namespaced mode of this suite is the only place the
		// two are combined.
		It("reads an action's credentials with the cache restricted to one namespace", func() {
			Eventually(func(g Gomega) {
				edge := automationOf(g, namespace, automation).Status.EdgeActions
				g.Expect(edge).To(HaveLen(1), "the edge action did not run on the transition")
				g.Expect(edge[0].Type).To(Equal("http.request"))
				g.Expect(edge[0].Status).To(Equal("Success"),
					"the outbound call failed: %s", edge[0].Reason)
			}).Should(Succeed())

			By("having taken the destination from the Secret, which it could not have read otherwise")
			edge := automationOf(Default, namespace, automation).Status.EdgeActions[0]
			Expect(edge.Destination).To(ContainSubstring(harness.MockName),
				"the request went somewhere other than the destination the Secret named")
		})

		It("runs the pre-delete hook while the release it depends on is still there", func() {
			done := make(chan struct{})
			watching := watchUninstall(namespace, done)

			var err error
			uninstallOutput, err = cluster.Helm("uninstall", release, "--namespace", namespace)
			close(done)
			observed = <-watching

			Expect(err).NotTo(HaveOccurred(),
				"the uninstall failed; a pre-delete hook that cannot finish blocks removing the operator")
			Expect(uninstallOutput).NotTo(ContainSubstring("failed"))
			Expect(observed.sawJob).To(BeTrue(),
				"never observed the release-claims Job in %d samples", observed.samples)
			Expect(observed.jobRanWithoutOperator).To(BeFalse(),
				"the release's own resources were removed while the pre-delete hook was still running")
		})

		It("hands the workload back to what it was before Reactor claimed it", func() {
			// This is also the load-bearing evidence for the ordering above.
			// The hook runs under the manager's ServiceAccount and needs the
			// release's RBAC to patch anything at all, so a workload that came
			// back proves those still existed when the hook ran.
			Expect(replicasOf(Default, namespace, app)).To(BeEquivalentTo(baseline))
			annotations := annotationsOf(Default, namespace, app)
			Expect(annotations).NotTo(HaveKey(annotationBaseline))
			Expect(annotations).NotTo(HaveKey(annotationClaimedBy))
		})

		It("leaves the Automations in place, with nothing left to service their finalizers", func() {
			By("keeping the CRD and the resources stored under it, as the keep policy promises")
			left := automationOf(Default, namespace, automation)
			Expect(left.Finalizers).NotTo(ContainElement(finalizer),
				"a finalizer outlived the controller that services it; deleting this now hangs forever")
		})

		It("lets the CRD be deleted afterwards rather than hanging on a finalizer", func() {
			By("removing the operator's own Deployment first, as the uninstall should have")
			out, err := cluster.Kubectl("get", "deploy", "-n", namespace, "-o", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("deployment.apps/" + release))

			deleted := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				_, err := cluster.Kubectl("delete", "crd", crdName, "--timeout=120s")
				deleted <- err
			}()
			Eventually(deleted, 2*time.Minute).Should(Receive(BeNil()),
				"deleting the CRD hung, which is the trap a finalizer without a controller creates")
		})
	})
}

func init() {
	declareUninstallSpecs("Uninstalling with cluster-wide RBAC", "reactor-uninstall")
	declareUninstallSpecs("Uninstalling with namespace-scoped RBAC", "reactor-uninstall-scoped",
		set, "rbac.clusterWide=false")
}
