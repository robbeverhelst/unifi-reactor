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

package unifi

import (
	"context"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// wifiFrom derives the wifi key from the wlan subsystem's own counts.
//
// It is derived from num_disconnected and num_adopted rather than from the
// subsystem's status string, and #9 asks for that choice to be documented
// rather than left mysterious. Three reasons:
//
//   - The counts are a fact this provider can explain. "warning" is a word
//     whose rule lives in the console's firmware, and an operator asking why
//     wifi is warning deserves an answer better than "UniFi said so".
//   - error becomes derivable. No capture has ever shown any subsystem saying
//     "error", so mapping it through would be inference; "every adopted AP is
//     disconnected" is an observation, and it is what error means here.
//   - The two agree in the only capture there is — status "warning" with 1 of 3
//     adopted APs disconnected — so this is a sharpening of the console's
//     verdict rather than a disagreement with it.
//
// The status string is still read, and a mismatch is counted and logged. That
// is the same third-opinion pattern the wan mapping uses: two independent
// signals for one fact, neither silently trusted.
//
// An empty result means the wlan subsystem said nothing this provider can act
// on, and the key is omitted rather than guessed.
func wifiFrom(ctx context.Context, subsystem healthSubsystem) string {
	log := logf.FromContext(ctx).WithName("unifi-wifi")

	adopted, disconnected := subsystem.NumAdopted, subsystem.NumDisconnected
	if adopted == nil || disconnected == nil {
		log.V(1).Info("The wlan subsystem reports no AP counts; wifi will not be published",
			"status", subsystem.Status)
		return ""
	}
	if *adopted == 0 {
		// No access points adopted at all. There is no WiFi here to be healthy
		// or unhealthy, which is "omit what you cannot see" rather than ok.
		log.V(1).Info("No access point is adopted; wifi will not be published")
		return ""
	}

	wifi := wifiOK
	switch {
	case *disconnected >= *adopted:
		// Every adopted AP is out of contact. This is what error means here,
		// and deriving it is why the vendor's own status is not the source:
		// "error" has never been captured on any subsystem.
		wifi = wifiError
	case *disconnected > 0:
		wifi = wifiWarning
	}

	// num_ap is the count of APs actually connected: in the capture it is 2
	// alongside 3 adopted and 1 disconnected, which is the arithmetic that says
	// num_adopted is the right denominator. It is logged rather than derived
	// from, so that a firmware where it stops adding up is visible here first.
	log.V(1).Info("wifi", "wifi", wifi, "adopted", *adopted, "disconnected", *disconnected,
		"connected", derefInt(subsystem.NumAP), "consoleStatus", subsystem.Status)

	crossCheckWiFiStatus(ctx, wifi, subsystem.Status)
	return wifi
}

// crossCheckWiFiStatus reports when the console's own wlan status and the value
// derived from its counts disagree.
//
// Nothing is resolved here: the counts stay the source, because they are the
// half that can be explained. What this buys is that if UniFi's wording turns
// out to mean something else — a "warning" that is about airtime or channel
// interference rather than about an AP being gone — the count rises instead of
// the derivation quietly being wrong.
func crossCheckWiFiStatus(ctx context.Context, derived, status string) {
	if status == "" {
		return
	}
	fromStatus := ""
	switch status {
	case healthStatusOK:
		fromStatus = wifiOK
	case healthStatusWarning:
		fromStatus = wifiWarning
	case healthStatusError:
		fromStatus = wifiError
	default:
		// A status this provider does not recognise is not a disagreement: it
		// is a vocabulary it has never seen, and the counts stand on their own.
		logf.FromContext(ctx).WithName("unifi-wifi").V(1).Info(
			"The wlan subsystem reports an unfamiliar status; wifi is derived from the AP counts regardless",
			"status", status, "wifi", derived)
		return
	}
	if fromStatus == derived {
		return
	}
	metrics.SignalsDisagreed(ProviderName, signalWiFiStatusDisagrees)
	logf.FromContext(ctx).WithName("unifi-wifi").Info(
		"The console's own wlan status and the value derived from its AP counts disagree; "+
			"the counts are what wifi reports, because they are the half that can be explained",
		"wifi", derived, "consoleStatus", status, "consoleStatusWouldBe", fromStatus)
}
