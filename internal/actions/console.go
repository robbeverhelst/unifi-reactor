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

package actions

import "slices"

// The console action types: the edge actions that write to the console a
// provider observes, rather than to an address an Automation named.
//
// They are declared here and executed nowhere near here, which is the one thing
// about this file worth understanding. The action-type vocabulary lives in this
// package because every other type is spelled here too — including
// qbittorrent.*, which is exactly as service-specific as these are — and having
// two places to look for "what types exist" is how a type gets added to an enum
// and forgotten in a dispatch.
//
// What is deliberately NOT here is the execution. An outbound action goes to an
// address the Automation chose, so this package's Client and its allowlist are
// the whole security story. A console action goes to the one console the
// operator configured at install time, over an undocumented API, with
// credentials that are install configuration — so the client, the allowlist and
// the check-before-write discipline all belong in the provider package that
// already talks to that console. The controller routes on IsConsole and never
// learns a field name.
const (
	// TypeUniFiWLANEnable and TypeUniFiWLANDisable turn one wireless network on
	// or off.
	//
	// They are named as verbs and there are two of them, for the same reason
	// qbittorrent.pause and qbittorrent.resume are two types rather than one
	// with a boolean: an adjective is a level and gets arbitrated, a verb is an
	// occurrence and does not. A WLAN being enabled is plainly a level in the
	// world, and it is an occurrence here, because there is nowhere to record
	// what it was before Reactor changed it that would outlive the Automation —
	// and no way for the pre-delete sweep, which runs with no credentials at
	// all, to hand it back. See the WLAN type in api/v1alpha1.
	TypeUniFiWLANEnable  = "unifi.wlan.enable"
	TypeUniFiWLANDisable = "unifi.wlan.disable"
	// TypeUniFiPoECycle power-cycles one PoE switch port. It needs no argument
	// about which column of the taxonomy it belongs in: there is no value a port
	// can be held at that means "cycled", so it is an occurrence in the world as
	// well as here, exactly like kubernetes.restart.
	//
	// It is the action in this repository where getting the target wrong does
	// the most visible damage — the wrong port drops an access point, a camera,
	// or the uplink carrying the cluster — which is why identifying the port
	// takes three things that must agree rather than an index. See the PoEPort
	// type in api/v1alpha1.
	TypeUniFiPoECycle = "unifi.poe.cycle"
	// TypeUniFiOutletCut and TypeUniFiOutletRestore open and close one switchable
	// outlet on a UniFi UPS.
	//
	// They are verbs for the reason the WLAN pair is: an outlet's position is
	// plainly a level in the world, and it is an occurrence here, because there
	// is nowhere to record what it was before Reactor changed it that outlives
	// the Automation, and no way for the pre-delete sweep to close a relay it has
	// no credentials to write.
	//
	// The verbs are deliberately not enable/disable. This is not a disabling; it
	// is a mains power cut, and the type is the first thing anyone reads in a
	// diff. "restore" is the pair of "cut" and means only that the relay is
	// closed again — it restores nothing that was recorded, because nothing was.
	//
	// It is the largest blast radius in this repository and the one where Reactor
	// is least able to help. A switch reports which of its ports is the uplink,
	// so unifi.poe.cycle can refuse that one absolutely; a UPS reports nothing
	// at all about what is plugged into an outlet. The whole defence is that the
	// operator allowlisted this outlet by MAC, index AND name, and that an outlet
	// still carrying the console's "Outlet N" placeholder is refused outright.
	// See the Outlet type in api/v1alpha1.
	TypeUniFiOutletCut     = "unifi.outlet.cut"
	TypeUniFiOutletRestore = "unifi.outlet.restore"
)

// consoleTypes is the one list of console action types. Dispatch and the
// startup line that names what an empty allowlist refuses both read it, so a
// type added here is refused-by-name at startup without anyone remembering —
// the outlet actions added in v1.3.0 were refused without the startup line
// naming them (#99).
var consoleTypes = []string{
	TypeUniFiWLANEnable, TypeUniFiWLANDisable, TypeUniFiPoECycle,
	TypeUniFiOutletCut, TypeUniFiOutletRestore,
}

// IsConsole reports whether an action type writes to a provider's own console,
// and so whether it is executed by that provider rather than by this package's
// outbound client.
func IsConsole(actionType string) bool {
	return slices.Contains(consoleTypes, actionType)
}

// ConsoleTypes returns every console action type, in declaration order.
func ConsoleTypes() []string {
	return slices.Clone(consoleTypes)
}
