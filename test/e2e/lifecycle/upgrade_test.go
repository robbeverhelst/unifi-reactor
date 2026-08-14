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
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/robbeverhelst/unifi-reactor/test/e2e/harness"
)

// legacyCRD stands in for the schema chart 0.3.0 shipped under crds/. It is
// deliberately reduced — no spec.reversal, no status.targets — because the
// point of the upgrade is that the live schema changes, and a schema that
// already had the new fields could not show that.
//
// It is written out rather than derived from the current CRD so that what
// "old" means here stays fixed while the real schema moves on.
const legacyCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: automations.reactor.robbeverhelst.com
spec:
  group: reactor.robbeverhelst.com
  names:
    kind: Automation
    listKind: AutomationList
    plural: automations
    singular: automation
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      schema:
        openAPIV3Schema:
          type: object
          properties:
            apiVersion: {type: string}
            kind: {type: string}
            metadata: {type: object}
            spec:
              type: object
              required: [actions]
              properties:
                when:
                  type: object
                  required: [provider, state]
                  properties:
                    provider: {type: string}
                    state:
                      type: object
                      additionalProperties: {type: string}
                actions:
                  type: array
                  items:
                    type: object
                    required: [type]
                    properties:
                      type: {type: string}
                      replicas: {type: integer, format: int32}
                      target:
                        type: object
                        required: [kind, name]
                        properties:
                          kind: {type: string}
                          name: {type: string}
                          namespace: {type: string}
            status:
              type: object
              properties:
                matching: {type: boolean}
                observedState:
                  type: object
                  additionalProperties: {type: string}
`

// automationWithReversal exercises a field that exists only in the current
// schema. Against the old one the API server rejects the resource outright —
// "a valid Automation is rejected", the second symptom in the troubleshooting
// guide's CRD section, and what an install stuck on the crds/ packaging would
// hit on every documented example it tried to apply.
const automationWithReversal = `apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: schema-probe
  namespace: %s
spec:
  when:
    provider: unifi
    state: {wan: backup}
  reversal: None
  actions:
    - type: kubernetes.scale
      target: {kind: Deployment, name: nothing}
      replicas: 0
`

// automationUnderOldSchema uses nothing the old schema lacks, so it can be
// stored before the upgrade and read back after it. Replacing a CRD replaces
// the schema every stored resource is served through, and "your Automations
// survive" is the promise the chart's keep policy is built around.
const automationUnderOldSchema = `apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: %s
  namespace: %s
spec:
  when:
    provider: unifi
    state: {wan: backup}
  actions:
    - type: kubernetes.scale
      target: {kind: Deployment, name: nothing}
      replicas: 0
`

// storedBeforeUpgrade is the Automation written under the old schema and
// expected to still be there, intact, once the new one is live.
const storedBeforeUpgrade = "stored-before-upgrade"

// Upgrading from the packaging that shipped the CRD under crds/ is a one-way
// door every existing install goes through exactly once. It was verified by
// hand when the packaging changed; this keeps it verified.
var _ = Describe("Upgrading from a chart that shipped the CRD under crds/", Ordered, func() {
	const namespace = "reactor-upgrade"
	var legacyChart string

	BeforeAll(func() {
		By("starting from a cluster where the CRD is not part of any release")
		removeCRD()
		_, _ = cluster.Kubectl("create", "namespace", namespace)

		By("assembling the chart as 0.3.0 packaged it: the CRD in crds/, not in templates/")
		root, err := os.MkdirTemp("", "legacy-chart")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(root) })
		legacyChart = filepath.Join(root, "reactor")
		_, err = harness.Run(GinkgoWriter, harness.ProjectDir(), "cp", "-R", "charts/reactor", legacyChart)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.Remove(filepath.Join(legacyChart, "templates", "crds.yaml"))).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(legacyChart, "crds"), 0o750)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(legacyChart, "crds", "automations.yaml"),
			[]byte(legacyCRD), 0o600)).To(Succeed())
	})

	AfterAll(func() {
		_, _ = cluster.Helm("uninstall", release, "--namespace", namespace, "--ignore-not-found")
		removeCRD()
		_, _ = cluster.Kubectl("delete", "namespace", namespace, "--wait=false")
	})

	It("installs the old packaging, leaving the CRD owned by nobody", func() {
		_, err := cluster.Helm("install", release, legacyChart, "--namespace", namespace,
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--wait", "--timeout=180s")
		Expect(err).NotTo(HaveOccurred())

		labels, err := cluster.Kubectl("get", "crd", crdName, "-o", "jsonpath={.metadata.labels}")
		Expect(err).NotTo(HaveOccurred())
		Expect(labels).NotTo(ContainSubstring("app.kubernetes.io/managed-by"),
			"a CRD installed from crds/ is applied by Helm but never recorded as part of the release")
	})

	It("rejects an Automation using a field the old schema does not have", func() {
		out, err := cluster.KubectlInput(fmt.Sprintf(automationWithReversal, namespace), "apply", "-f", "-")
		Expect(err).To(HaveOccurred(), "the old schema is not actually the one in force")
		Expect(out).To(ContainSubstring(`unknown field "spec.reversal"`))

		By("but accepting one the old schema does understand, to be read back after the upgrade")
		Expect(cluster.Apply(fmt.Sprintf(automationUnderOldSchema, storedBeforeUpgrade, namespace))).To(Succeed())
	})

	It("refuses the upgrade until the CRD is handed over to the release", func() {
		out, err := cluster.Helm("upgrade", release, "charts/reactor", "--namespace", namespace,
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--timeout=180s")
		Expect(err).To(HaveOccurred(), "the upgrade succeeded where the documented one-time adoption is required")
		Expect(out).To(ContainSubstring("invalid ownership metadata"),
			"the upgrade failed for a reason the troubleshooting guide does not describe")
	})

	It("upgrades once the documented adoption has been done", func() {
		By("running exactly what the chart README and troubleshooting guide tell people to run")
		_, err := cluster.Kubectl("label", "crd", crdName,
			"app.kubernetes.io/managed-by=Helm", "--overwrite")
		Expect(err).NotTo(HaveOccurred())
		_, err = cluster.Kubectl("annotate", "crd", crdName,
			"meta.helm.sh/release-name="+release,
			"meta.helm.sh/release-namespace="+namespace, "--overwrite")
		Expect(err).NotTo(HaveOccurred())

		_, err = cluster.Helm("upgrade", release, "charts/reactor", "--namespace", namespace,
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--wait", "--timeout=180s")
		Expect(err).NotTo(HaveOccurred())
	})

	It("puts the current schema live, not merely in the release", func() {
		By("asking the API server what it now knows about the type")
		out, err := cluster.Kubectl("get", "crd", crdName, "-o",
			"jsonpath={.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.reversal.enum}")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("Baseline"), "helm upgrade did not update the CRD's schema")

		By("and by round-tripping a resource that only the current schema accepts")
		// Retried because the API server rebuilds the handler that validates
		// and prunes a type a moment after the CRD object itself changes, so
		// the first write after an upgrade can still meet the old schema.
		Eventually(func(g Gomega) {
			g.Expect(cluster.Apply(fmt.Sprintf(automationWithReversal, namespace))).To(Succeed())
			reversal, err := cluster.Kubectl("get", automationKind, "schema-probe", "-n", namespace,
				"-o", "jsonpath={.spec.reversal}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(reversal).To(Equal("None"),
				"the same resource the old schema rejected is now stored with the field intact")
		}).Should(Succeed())

		By("without losing what was already stored under the schema it replaced")
		stored := automationOf(Default, namespace, storedBeforeUpgrade)
		Expect(stored.Spec.When).NotTo(BeNil())
		Expect(stored.Spec.When.State).To(HaveKeyWithValue("wan", "backup"),
			"replacing the CRD did not preserve an Automation written under the old schema")
	})
})
