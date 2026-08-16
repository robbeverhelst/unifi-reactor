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
		// And no adoption hook, because 0.3.0 had none: it is what this upgrade
		// brings. Leaving it in would let the old chart adopt its own CRD on
		// install — Helm applies crds/ before it renders templates — and there
		// would be nothing left for the upgrade to prove.
		Expect(os.Remove(filepath.Join(legacyChart, "templates", "crd-adoption-hook.yaml"))).To(Succeed())
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

	// The whole point of #72, and the assertion that has to stay true: one
	// `helm upgrade`, no kubectl, no flag, nothing for the person doing the
	// upgrade to know. If this ever needs a manual step again, this fails.
	It("upgrades with no manual step at all", func() {
		out, err := cluster.Helm("upgrade", release, "charts/reactor", "--namespace", namespace,
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--wait", "--timeout=180s")
		Expect(err).NotTo(HaveOccurred(),
			"the upgrade from the crds/ packaging still needs the CRD handed over by hand: "+out)

		By("having taken ownership of the CRD rather than replacing it")
		labels, err := cluster.Kubectl("get", "crd", crdName, "-o", "jsonpath={.metadata.labels}")
		Expect(err).NotTo(HaveOccurred())
		Expect(labels).To(ContainSubstring(`"app.kubernetes.io/managed-by":"Helm"`))
		annotations, err := cluster.Kubectl("get", "crd", crdName, "-o", "jsonpath={.metadata.annotations}")
		Expect(err).NotTo(HaveOccurred())
		Expect(annotations).To(ContainSubstring(`"meta.helm.sh/release-name":"` + release + `"`))
		Expect(annotations).To(ContainSubstring(`"meta.helm.sh/release-namespace":"` + namespace + `"`))

		// #84: someone reading `helm get manifest` after this upgrade finds no
		// CRD in it and reasonably concludes their deployment tool dropped one.
		// It did not — the chart left it out, because Helm checks ownership
		// before it runs the hook that establishes ownership. This is the one
		// revision where the release does not carry the CRD, and it is pinned
		// here so it stays deliberate rather than becoming a thing to rediscover.
		By("with the CRD deliberately outside the release on this one revision")
		manifest, err := cluster.Helm("get", "manifest", release, "--namespace", namespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest).NotTo(ContainSubstring("kind: CustomResourceDefinition"),
			"the adopting upgrade rendered the CRD, which Helm would have refused before the hook could run")

		// And the keep policy live anyway, applied by the hook rather than by
		// the template that is not part of this revision. Without it there is a
		// window — this revision — where a Helm-owned CRD holding every
		// Automation in the cluster carries nothing saying to leave it alone.
		By("and the keep policy live regardless, because the hook carries it too")
		Expect(annotations).To(ContainSubstring(`"helm.sh/resource-policy":"keep"`))
	})

	// A hook that succeeds takes its own permissions with it. The grant it
	// needs — patch on a CustomResourceDefinition — is one the operator does
	// not have and must not be left holding.
	It("leaves nothing of the hook behind", func() {
		for _, kind := range []string{"job", "serviceaccount"} {
			left, err := cluster.Kubectl("get", kind, adoptJob, "-n", namespace, "--ignore-not-found")
			Expect(err).NotTo(HaveOccurred())
			Expect(left).To(BeEmpty(), "the adoption %s outlived the upgrade that needed it", kind)
		}
		for _, kind := range []string{"clusterrole", "clusterrolebinding"} {
			left, err := cluster.Kubectl("get", kind, adoptJob, "--ignore-not-found")
			Expect(err).NotTo(HaveOccurred())
			Expect(left).To(BeEmpty(), "a cluster-scoped %s over CRDs outlived the upgrade", kind)
		}
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

	// Adoption happens once and then stops existing. From here the CRD is an
	// ordinary part of the release, Helm maintains its schema, and no upgrade
	// runs a Job or asks for a cluster-scoped permission it does not need.
	It("does not render the adoption again on the next upgrade", func() {
		_, err := cluster.Helm("upgrade", release, "charts/reactor", "--namespace", namespace,
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--wait", "--timeout=180s")
		Expect(err).NotTo(HaveOccurred())

		hooks, err := cluster.Helm("get", "hooks", release, "--namespace", namespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(hooks).NotTo(ContainSubstring(adoptJob),
			"the adoption hook is rendered on every upgrade, not only the one that needed it")

		By("with the CRD now carried by the release itself")
		manifest, err := cluster.Helm("get", "manifest", release, "--namespace", namespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest).To(ContainSubstring("kind: CustomResourceDefinition"),
			"the CRD never rejoined the release, so a later schema change would not reach the cluster")
	})
})

// crds.adopt=false says "I will hand the CRD over myself". Until somebody does,
// the CRD belongs to no release and Helm will not update it — so the upgrade
// cannot proceed, and the only question is whether it says why. Helm's own
// refusal names neither the value that turned adoption off nor the two commands
// that finish the job, so the chart refuses first and names both.
var _ = Describe("Declining to adopt a CRD that belongs to no release", Ordered, func() {
	const namespace = "reactor-no-adopt"

	BeforeAll(func() {
		removeCRD()
		_, _ = cluster.Kubectl("create", "namespace", namespace)

		By("leaving the CRD as the crds/ packaging did: applied, owned by nobody")
		Expect(cluster.Apply(legacyCRD)).To(Succeed())
	})

	AfterAll(func() {
		_, _ = cluster.Helm("uninstall", release, "--namespace", namespace, "--ignore-not-found")
		removeCRD()
		_, _ = cluster.Kubectl("delete", "namespace", namespace, "--wait=false")
	})

	It("stops, naming the commands that finish the job", func() {
		out, err := cluster.Helm("install", release, "charts/reactor", "--namespace", namespace,
			set, "crds.adopt=false",
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--timeout=180s")
		Expect(err).To(HaveOccurred(),
			"an install that can neither adopt the CRD nor update it reported success")
		Expect(out).To(ContainSubstring("crds.adopt=false"),
			"the failure does not say which value declined the adoption")
		Expect(out).To(ContainSubstring("kubectl label crd " + crdName))
		Expect(out).To(ContainSubstring("kubectl annotate crd " + crdName))
	})

	It("leaves the CRD untouched", func() {
		labels, err := cluster.Kubectl("get", "crd", crdName, "-o", "jsonpath={.metadata.labels}")
		Expect(err).NotTo(HaveOccurred())
		Expect(labels).NotTo(ContainSubstring("app.kubernetes.io/managed-by"),
			"a refused install adopted the CRD anyway")
	})
})

// Adopting a resource is taking ownership of something, so the case that must
// never work is as important as the one that must: a CRD another release
// installed belongs to that release, and taking it would leave the release that
// owns it unable to update its own object.
var _ = Describe("A CRD that belongs to a different release", Ordered, func() {
	const namespace = "reactor-foreign-crd"
	const otherRelease = "platform-crds"
	const otherNamespace = "platform"

	BeforeAll(func() {
		removeCRD()
		_, _ = cluster.Kubectl("create", "namespace", namespace)

		By("putting a CRD in the cluster that says it belongs to another release")
		Expect(cluster.Apply(legacyCRD)).To(Succeed())
		_, err := cluster.Kubectl("label", "crd", crdName,
			"app.kubernetes.io/managed-by=Helm", "--overwrite")
		Expect(err).NotTo(HaveOccurred())
		_, err = cluster.Kubectl("annotate", "crd", crdName,
			"meta.helm.sh/release-name="+otherRelease,
			"meta.helm.sh/release-namespace="+otherNamespace, "--overwrite")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = cluster.Helm("uninstall", release, "--namespace", namespace, "--ignore-not-found")
		removeCRD()
		_, _ = cluster.Kubectl("delete", "namespace", namespace, "--wait=false")
	})

	It("is refused, by a message naming the release that owns it", func() {
		out, err := cluster.Helm("install", release, "charts/reactor", "--namespace", namespace,
			set, "image.repository="+managerRepository,
			set, "image.tag="+managerTag,
			set, "image.pullPolicy=Never",
			"--timeout=180s")
		Expect(err).To(HaveOccurred(), "a CRD owned by another release was taken from it")
		Expect(out).To(ContainSubstring(otherRelease),
			"the failure does not say which release owns the CRD, which is the one thing it has to say")
		Expect(out).To(ContainSubstring("will not take a CRD from another release"))
	})

	It("is left exactly as it was", func() {
		annotations, err := cluster.Kubectl("get", "crd", crdName, "-o", "jsonpath={.metadata.annotations}")
		Expect(err).NotTo(HaveOccurred())
		Expect(annotations).To(ContainSubstring(`"meta.helm.sh/release-name":"`+otherRelease+`"`),
			"the other release's ownership was overwritten")
		Expect(annotations).To(ContainSubstring(`"meta.helm.sh/release-namespace":"` + otherNamespace + `"`))
	})
})
