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

package controller

import (
	"context"
	"errors"
	"time"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
)

// ConsoleWriter performs the edge actions that write to the console a provider
// observes, rather than to the cluster or to an address an Automation named.
//
// It is the third destination an edge action can have, and it needs its own
// seam for the same reason the other two are separated: a kubernetes.* action
// goes to an API server this operator is already authenticated to, an outbound
// action goes wherever the install-level destination allowlist permits, and a
// console action goes to the one console the provider was configured with,
// under credentials and an allowlist that are that provider's business.
//
// The interface is defined here, at the consumer, and takes the Action whole.
// That is what keeps this package from learning a single field name of any
// provider's write API: it routes on the action type and forwards. Everything
// about what a WLAN is, how the console authenticates a write and which objects
// this install allows lives in internal/providers/<name>, next to the code that
// already talks to that hardware.
type ConsoleWriter interface {
	// Apply performs the action, returning what it acted on and whether it
	// worked. It is expected to check before it writes and to abandon rather
	// than guess, and its Result names the console object rather than an
	// address.
	Apply(ctx context.Context, action reactorv1alpha1.Action, timeout time.Duration) (actions.Result, error)
}

// runConsoleAction performs one edge action against a provider's console.
//
// There is no Enabled() pre-check here, unlike the outbound path. That check
// exists there to avoid reading a Secret an install has no use for; a console
// action reads no Secret, and the writer can say precisely which allowlist the
// missing entry belongs in — which is worth more than a generic refusal.
func (r *AutomationReconciler) runConsoleAction(
	ctx context.Context,
	action reactorv1alpha1.Action,
) (actions.Result, error) {
	if r.Console == nil {
		return actions.Result{}, errors.New(
			"no console is configured on this install, so there is nothing for this action to write to")
	}
	// A console write is a login, a check and a write rather than one request,
	// so it takes the same default budget a kubernetes.* action does rather
	// than the shorter outbound one.
	timeout := defaultActionTimeout
	if action.TimeoutSeconds != nil {
		timeout = time.Duration(*action.TimeoutSeconds) * time.Second
	}
	return r.Console.Apply(ctx, action, timeout)
}
