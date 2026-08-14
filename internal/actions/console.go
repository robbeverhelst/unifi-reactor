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
)

// IsConsole reports whether an action type writes to a provider's own console,
// and so whether it is executed by that provider rather than by this package's
// outbound client.
func IsConsole(actionType string) bool {
	switch actionType {
	case TypeUniFiWLANEnable, TypeUniFiWLANDisable:
		return true
	}
	return false
}
