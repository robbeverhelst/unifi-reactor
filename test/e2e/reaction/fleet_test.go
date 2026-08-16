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

// Each key in this batch is observed through an Automation that matches on it,
// because a key that nothing can react to is not a feature. The provider's own
// tests cover the derivations; these cover the round trip — rehearsed console,
// poll, debounce, condition, action — for keys whose failure modes are all
// about what happens when something stops being reported.
//
// The console starts from the captures, where firmware, temperature and PoE are
// not reported at all. That is deliberate: the first assertion of each block is
// that the key is absent, which is the state a real install of this version
// against a real console is in until those fields are confirmed to exist.
var _ = Describe("Reacting to the fleet, firmware, thermals, WiFi and PoE", Ordered, func() {
	const (
		fleetJob     = "fleet-batch"
		deviceJob    = "device-batch"
		firmwareJob  = "firmware-batch"
		thermalJob   = "thermal-batch"
		wifiJob      = "wifi-batch"
		poeJob       = "poe-batch"
		fleetPolicy  = "hold-on-degraded-fleet"
		devicePolicy = "hold-on-ups-offline"
		firmwarePol  = "hold-on-updates"
		thermalPol   = "shed-when-hot"
		wifiPolicy   = "hold-on-wifi-warning"
		poePolicy    = "shed-when-poe-tight"
		baseline     = 2
	)

	degradedFleet := map[string]string{keyDevices: devicesDegraded}
	upsOffline := map[string]string{keyUPSDevice: deviceOffline}
	updatesWaiting := map[string]string{keyFirmware: firmwareUpdates}
	tooHot := map[string]string{keyTemperature: temperatureHigh}
	wifiDegraded := map[string]string{keyWiFi: wifiWarning}
	noHeadroom := map[string]string{keyPoE: poeInsufficient}

	policies := []struct {
		name   string
		when   map[string]string
		target string
	}{
		{fleetPolicy, degradedFleet, fleetJob},
		{devicePolicy, upsOffline, deviceJob},
		{firmwarePol, updatesWaiting, firmwareJob},
		{thermalPol, tooHot, thermalJob},
		{wifiPolicy, wifiDegraded, wifiJob},
		{poePolicy, noHeadroom, poeJob},
	}

	BeforeAll(func() {
		resetConsole()
		// The capture itself has one of four access points disconnected, so
		// the console's own baseline is wifi: warning. This block starts from a
		// healthy one and breaks it deliberately, rather than beginning
		// half-broken.
		Expect(mock.WiFi("adopted=4&disconnected=0")).To(Succeed())
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

	// Every claim is released through a HEALTHY observation rather than through
	// resetConsole, and the difference matters: reset makes these keys vanish,
	// and a key that vanishes while its Automation is matching holds the claim
	// instead of releasing it. That is the behaviour half this file exists to
	// prove, and it would hang the teardown of the other half.
	AfterAll(func() {
		Expect(mock.WiFi("adopted=4&disconnected=0")).To(Succeed())
		Expect(mock.Temperature("celsius=45")).To(Succeed())
		Expect(mock.PoE("watts=12&budget=60&silent=false")).To(Succeed())
		Expect(mock.Firmware("upgradable=false")).To(Succeed())
		Expect(mock.Device("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			for _, policy := range policies {
				g.Expect(replicasOf(g, policy.target)).To(BeEquivalentTo(baseline))
			}
		}).Should(Succeed())
		for _, policy := range policies {
			Expect(cluster.Delete(scaleAutomation(policy.name, policy.when, policy.target, 0))).To(Succeed())
		}
		resetConsole()
	})

	// The capture has two adopted devices, both online, and no upgrade, thermal
	// or PoE field anywhere. A console in that state must publish the two keys
	// it can and none of the three it cannot.
	It("reports the captured console as a healthy fleet, and says nothing about what it cannot see", func() {
		Eventually(func(g Gomega) {
			state := automationOf(g, fleetPolicy).Status.ObservedState
			g.Expect(state).To(HaveKeyWithValue(keyDevices, devicesAllOnline))
			g.Expect(automationOf(g, devicePolicy).Status.ObservedState).
				To(HaveKeyWithValue(keyUPSDevice, deviceOnline))
			g.Expect(automationOf(g, wifiPolicy).Status.ObservedState).
				To(HaveKeyWithValue(keyWiFi, wifiOK))
		}).Should(Succeed())

		By("withholding the keys whose fields no capture contains")
		for _, policy := range []struct {
			name string
			key  string
		}{{firmwarePol, keyFirmware}, {thermalPol, keyTemperature}, {poePolicy, keyPoE}} {
			ready := conditionOf(automationOf(Default, policy.name), conditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal("StateKeyUnavailable"),
				"%s should report the key it needs as unavailable, not react to a value nobody observed", policy.key)
		}

		Consistently(func(g Gomega) {
			for _, policy := range policies {
				g.Expect(replicasOf(g, policy.target)).To(BeEquivalentTo(baseline))
			}
		}).Should(Succeed())
	})

	// The capture's own wlan subsystem is the first half of this: 1 of 4 adopted
	// access points disconnected, which is what a real console was reporting
	// when it was captured.
	//
	// The second half is the caveat every three-value key carries, and it is the
	// same one `internet: ok | degraded | down` has: an Automation matching
	// `warning` stops matching at `error`, because those are different values of
	// one key rather than steps of a ladder. Match the value you mean.
	It("sheds while access points are missing, and reports all of them gone as a different value", func() {
		By("restoring the capture: 1 of 4 access points disconnected")
		Expect(mock.WiFi("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, wifiPolicy).Status.ObservedState).To(HaveKeyWithValue(keyWiFi, wifiWarning))
			g.Expect(replicasOf(g, wifiJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("losing all of them, which is error rather than a worse warning")
		Expect(mock.WiFi("adopted=4&disconnected=4")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, wifiPolicy).Status.ObservedState).To(HaveKeyWithValue(keyWiFi, wifiError))
			// An Automation matching `warning` no longer matches, so its claim
			// is released. Anyone wanting to react to the whole WLAN being gone
			// writes `wifi: error` — the values are alternatives, not degrees.
			g.Expect(replicasOf(g, wifiJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())

		By("bringing every access point back")
		Expect(mock.WiFi("disconnected=0")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, wifiPolicy).Status.ObservedState).To(HaveKeyWithValue(keyWiFi, wifiOK))
			g.Expect(replicasOf(g, wifiJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	It("reacts to a device going offline, on both the fleet key and the device's own", func() {
		Expect(mock.Device("name=ups-2u&state=offline")).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, fleetJob)).To(BeEquivalentTo(0))
			g.Expect(replicasOf(g, deviceJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		state := automationOf(Default, fleetPolicy).Status.ObservedState
		Expect(state).To(HaveKeyWithValue(keyDevices, devicesDegraded))
		Expect(automationOf(Default, devicePolicy).Status.ObservedState).
			To(HaveKeyWithValue(keyUPSDevice, deviceOffline))
	})

	// #8's acceptance criterion, and the reason the reconciler's hold-state
	// behaviour matters here more than anywhere else: a device name is a key
	// name, so renaming a switch on the console deletes a key. That must read as
	// lost visibility, not as recovery.
	It("holds its claim when a device is renamed out from under the key", func() {
		Expect(mock.Device("name=ups-2u&rename=Rack+Power")).To(Succeed())

		Eventually(func(g Gomega) {
			ready := conditionOf(automationOf(g, devicePolicy), conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"))
			g.Expect(ready.Message).To(ContainSubstring(keyUPSDevice))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, deviceJob)).To(BeEquivalentTo(0))
		}).Should(Succeed(), "renaming a device is not the device coming back")

		By("leaving the aggregate alone, which counts the device under either name")
		Expect(automationOf(Default, fleetPolicy).Status.ObservedState).
			To(HaveKeyWithValue(keyDevices, devicesDegraded))

		By("restoring both once the device is back under its captured name")
		Expect(mock.Device("reset=true")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, fleetJob)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, deviceJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	It("reacts to firmware becoming available, and to it being applied", func() {
		Expect(mock.Firmware("upgradable=false")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(automationOf(g, firmwarePol).Status.ObservedState).
				To(HaveKeyWithValue(keyFirmware, firmwareCurrent))
		}).Should(Succeed())

		Expect(mock.Firmware("upgradable=true&name=ups-2u")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, firmwareJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		By("one upgradable device being enough, whatever the rest of the fleet says")
		Expect(automationOf(Default, firmwarePol).Status.ObservedState).
			To(HaveKeyWithValue(keyFirmware, firmwareUpdates))

		Expect(mock.Firmware("upgradable=false")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, firmwareJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed())
	})

	// temperature and poe are the two bucketed measurements in this batch, and
	// they fail the same way twice over: a threshold crossed sheds the
	// workload, and a reading that stops arriving holds the claim rather than
	// reading as zero. One spec for both, because stating it once is the point
	// — the second measurement key must not quietly grow its own rules.
	DescribeTable("a bucketed measurement sheds on its threshold and holds when the reading stops",
		func(m measurement) {
			Expect(m.drive(m.healthy)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(automationOf(g, m.policy).Status.ObservedState).To(HaveKeyWithValue(m.key, m.healthyValue))
			}).Should(Succeed())

			By("crossing the configured threshold")
			Expect(m.drive(m.tight)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(replicasOf(g, m.target)).To(BeEquivalentTo(0))
			}).Should(Succeed())

			By("losing the reading, which is not the measurement going back to zero")
			Expect(m.drive(m.unreadable)).To(Succeed())
			Eventually(func(g Gomega) {
				ready := conditionOf(automationOf(g, m.policy), conditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Reason).To(Equal("StateKeyUnavailable"))
			}).Should(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(replicasOf(g, m.target)).To(BeEquivalentTo(0))
			}).Should(Succeed(), m.holdReason)

			Expect(m.drive(m.healthy)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(replicasOf(g, m.target)).To(BeEquivalentTo(baseline))
			}).Should(Succeed())
		},
		Entry("temperature", measurement{
			policy: thermalPol, target: thermalJob, key: keyTemperature,
			healthy: "celsius=45", healthyValue: temperatureNormal,
			tight: "celsius=82", unreadable: "present=false",
			holdReason: "a device that stopped reporting its temperature is not a device at 0 °C",
			drive:      func(query string) error { return mock.Temperature(query) },
		}),
		Entry("poe", measurement{
			policy: poePolicy, target: poeJob, key: keyPoE,
			// silent=false is part of "healthy" because the mock holds that
			// override until it is told otherwise, and the restore at the end
			// of this spec has to undo it.
			healthy: "watts=12&budget=60&silent=false", healthyValue: poeOK,
			tight: "watts=57", unreadable: "silent=true",
			holdReason: "an unreadable switch is not a switch with headroom",
			drive:      func(query string) error { return mock.PoE(query) },
		}),
	)

	// The keys in this batch come from two different endpoints and four
	// different derivations, and they are independent observations: one going
	// away must not move any of the others.
	It("keeps every key independent of the others", func() {
		Expect(mock.Temperature("celsius=82")).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(replicasOf(g, thermalJob)).To(BeEquivalentTo(0))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(replicasOf(g, fleetJob)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, deviceJob)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, firmwareJob)).To(BeEquivalentTo(baseline))
			g.Expect(replicasOf(g, poeJob)).To(BeEquivalentTo(baseline))
		}).Should(Succeed(), "a hot device is not a firmware update, a missing AP or a full PoE budget")
	})
})

// measurement is one bucketed key's rehearsal: the console queries that put it
// on either side of its threshold, and the one that stops the reading arriving
// at all.
type measurement struct {
	policy, target, key   string
	healthy, healthyValue string
	tight, unreadable     string
	holdReason            string
	drive                 func(query string) error
}
