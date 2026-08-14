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
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

const (
	// executionSuccess, executionFailed and executionSkipped are the values
	// status.edgeActions[].status takes.
	executionSuccess = "Success"
	executionFailed  = "Failed"
	executionSkipped = "Skipped"
	// reasonEdgeActionSent, reasonEdgeActionFailed and reasonEdgeActionSkipped
	// are the Event reasons an operator greps for. A reaction Reactor performed
	// should be visible without reading controller logs, which is the whole
	// point of having these actions at all.
	reasonEdgeActionSent    = "EdgeActionSent"
	reasonEdgeActionFailed  = "EdgeActionFailed"
	reasonEdgeActionSkipped = "EdgeActionSkipped"
	// verbApplied, verbCommanded and verbDelivered are how an Event says what an
	// edge action did. See edgeVerbs for which goes with what.
	verbApplied   = "applied to"
	verbCommanded = "applied at"
	verbDelivered = "delivered to"
	// edgeActionBudget bounds every edge action on one transition put together,
	// so a list of unreachable endpoints delays this Automation rather than
	// occupying a reconcile worker indefinitely. Actions left when it runs out
	// are recorded as skipped rather than quietly dropped.
	edgeActionBudget = time.Minute
)

// edgeActionsOf lists the edge actions that fire for one direction of a
// transition. Entering the condition runs spec.actions, leaving it runs
// spec.onExit — an edge action in an onExit block fires on this Automation's
// own edge, unlike the desired-state entries beside it, which are folded across
// every Automation sharing the target and have no edge to fire on.
func edgeActionsOf(automation *reactorv1alpha1.Automation, matching bool) []reactorv1alpha1.Action {
	list := automation.Spec.Actions
	if !matching {
		list = automation.Spec.OnExit
	}
	var edges []reactorv1alpha1.Action
	for _, action := range list {
		if !isDesiredState(action.Type) {
			edges = append(edges, action)
		}
	}
	return edges
}

// runEdgeActions fires this Automation's edge actions for the transition that
// has just been committed to status, and returns what happened to each.
//
// Three properties are deliberate.
//
// It runs after the transition is written, not before. An edge action reports
// an occurrence, so firing it twice is a lie told twice; committing first means
// a status write that conflicts or fails cannot leave wasMatching stale and
// send the same notification again on the retry. The cost is that a crash in
// the window between the write and the send loses the notification, which is
// the right way round: the realistic failure is an endpoint that is down for a
// few seconds, and that is what the per-action retry covers.
//
// It runs only after the desired-state actions applied. Reconcile returns
// early when a target could not be written, leaving status.matching unchanged,
// so the edge fires on the retry that succeeds instead — a notification saying
// a workload was scaled down should not go out while it is still up.
//
// A failure here never fails the Automation. The desired-state action is the
// thing that had to happen; the notification is the report of it. So a failure
// is recorded in status.edgeActions and raised as a Warning Event, and Ready
// stays as the target reconciliation left it.
func (r *AutomationReconciler) runEdgeActions(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	matching bool,
) []reactorv1alpha1.EdgeExecutionStatus {
	edges := edgeActionsOf(automation, matching)
	if len(edges) == 0 {
		return nil
	}

	log := logf.FromContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, edgeActionBudget)
	defer cancel()

	data := templateContext(automation, matching)
	results := make([]reactorv1alpha1.EdgeExecutionStatus, 0, len(edges))
	for _, action := range edges {
		entry := reactorv1alpha1.EdgeExecutionStatus{Type: action.Type, OnExit: !matching}
		if ctx.Err() != nil {
			entry.Status = executionSkipped
			entry.Reason = "ran out of time for the remaining actions on this transition"
			metrics.ActionSkipped(action.Type, metrics.KindEdge, !matching)
		} else {
			started := time.Now()
			result, err := r.runEdgeAction(ctx, automation, action, data)
			// Counted as an edge action, which is what keeps a failure here from
			// reading as an Automation that could not do its job: this did not
			// happen, and the desired-state action beside it still did.
			metrics.ActionExecuted(action.Type, metrics.KindEdge, !matching, err, time.Since(started))
			entry.Destination = result.Origin
			entry.Attempts = result.Attempts
			entry.Status = executionSuccess
			if err != nil {
				entry.Status = executionFailed
				entry.Reason = err.Error()
			}
		}
		entry.Time = metav1.Now()
		r.reportEdgeAction(log, automation, entry)
		results = append(results, entry)
	}
	return results
}

// reportEdgeAction logs the outcome and raises an Event for it, so that a
// reaction is visible to someone reading kubectl describe rather than only to
// someone reading controller logs.
func (r *AutomationReconciler) reportEdgeAction(
	log logr.Logger,
	automation *reactorv1alpha1.Automation,
	entry reactorv1alpha1.EdgeExecutionStatus,
) {
	done, failed := edgeVerbs(entry.Type)
	switch entry.Status {
	case executionSuccess:
		log.Info("edge action sent", "automation", claimantOf(automation),
			"action", entry.Type, "destination", entry.Destination, "attempts", entry.Attempts)
		r.event(automation, corev1.EventTypeNormal, reasonEdgeActionSent, actionEdge,
			"%s %s %s after %d attempt(s)", entry.Type, done, entry.Destination, entry.Attempts)
	case executionFailed:
		log.Info("edge action failed", "automation", claimantOf(automation),
			"action", entry.Type, "destination", entry.Destination, "reason", entry.Reason)
		r.event(automation, corev1.EventTypeWarning, reasonEdgeActionFailed, actionEdge,
			"%s %s: %s", entry.Type, failed, entry.Reason)
	default:
		r.event(automation, corev1.EventTypeWarning, reasonEdgeActionSkipped, actionEdge,
			"%s did not run: %s", entry.Type, entry.Reason)
	}
}

// edgeVerbs says what an edge action did in words that fit what it acted on.
//
// A notification is delivered to a destination; a restart is applied to an
// object, and reading "delivered to Deployment/media/sonarr" at 3am would make
// an operator wonder what was delivered. The named integrations sit between the
// two — they command a service at an address rather than announcing anything to
// it, so "delivered" would read as if a message had gone out.
func edgeVerbs(actionType string) (done, failed string) {
	switch {
	case isKubernetesAction(actionType):
		return verbApplied, "was not applied"
	case actionType == actions.TypeHomeAssistant,
		actionType == actions.TypeQBittorrentPause,
		actionType == actions.TypeQBittorrentResume:
		return verbCommanded, "was not applied at"
	}
	return verbDelivered, "was not delivered"
}

// runEdgeAction performs one edge action.
//
// The two families are separated here rather than inside the outbound client,
// because a kubernetes.* edge action goes to the API server this operator is
// already authenticated to — there is no destination to allow, no credential to
// resolve, and no allowlist that could sensibly apply. An install that has
// deliberately allowed no outbound destination can still restart a workload.
func (r *AutomationReconciler) runEdgeAction(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
	data actions.Context,
) (actions.Result, error) {
	if isKubernetesAction(action.Type) {
		return r.runClusterAction(ctx, automation, action)
	}
	if r.Outbound == nil || !r.Outbound.Enabled() {
		return actions.Result{}, errors.New(
			"outbound actions are disabled on this install: no destination is allowed")
	}
	request, err := r.buildRequest(ctx, automation, action, data)
	if err != nil {
		return actions.Result{}, err
	}
	return r.Outbound.Do(ctx, request)
}

// templateContext is what an edge action's templates may read. It is built from
// the status just written, so what a notification says and what the Automation
// reports cannot disagree.
func templateContext(automation *reactorv1alpha1.Automation, matching bool) actions.Context {
	data := actions.Context{
		Automation: claimantOf(automation),
		Namespace:  automation.Namespace,
		Name:       automation.Name,
		Matching:   matching,
		State:      automation.Status.ObservedState,
		Time:       metav1.Now().UTC().Format(time.RFC3339),
	}
	if automation.Spec.When != nil {
		data.Provider = automation.Spec.When.Provider
	}
	if transition := automation.Status.LastTransition; transition != nil {
		data.Key, data.From, data.To = transition.Key, transition.From, transition.To
		if !transition.Time.IsZero() {
			data.Time = transition.Time.UTC().Format(time.RFC3339)
		}
	}
	if data.State == nil {
		data.State = map[string]string{}
	}
	return data
}

// buildRequest turns one action into a fully resolved request: credentials
// merged in from the Secret, body rendered, retry policy decided.
func (r *AutomationReconciler) buildRequest(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
	data actions.Context,
) (actions.Request, error) {
	timeout := actions.DefaultTimeout
	if action.TimeoutSeconds != nil {
		timeout = time.Duration(*action.TimeoutSeconds) * time.Second
	}
	switch action.Type {
	case actions.TypeHTTPRequest:
		return r.buildHTTPRequest(ctx, automation, action, data, timeout)
	case actions.TypeHomeAssistant:
		return r.buildHomeAssistantRequest(ctx, automation, action, data, timeout)
	case actions.TypeQBittorrentPause, actions.TypeQBittorrentResume:
		return r.buildQBittorrentRequest(ctx, automation, action, timeout)
	default:
		return r.buildNotification(ctx, automation, action, data, timeout)
	}
}

// destinationFrom resolves where an action sends, from the field on the
// Automation or the url key of its Secret.
//
// Exactly one of the two must supply it, and having both is an error rather
// than a precedence rule: someone who has written a URL in two places is
// telling Reactor two things, and quietly picking one is how a request ends up
// somewhere nobody meant.
//
// actionType and field are both named because the two callers spell the field
// differently, and an error that says "set the url field" without saying which
// action's is not worth reading at 3am.
func destinationFrom(actionType, field, fromAutomation, fromSecret, secretName string) (string, error) {
	switch {
	case fromAutomation != "" && fromSecret != "":
		return "", fmt.Errorf(
			"%s is set and secret %q also holds a %q key; exactly one must supply the destination",
			field, secretName, actions.SecretKeyURL)
	case fromAutomation != "":
		return fromAutomation, nil
	case fromSecret != "":
		return fromSecret, nil
	}
	return "", fmt.Errorf("%s has no destination: set %s, or a %q key in the referenced secret",
		actionType, field, actions.SecretKeyURL)
}

func (r *AutomationReconciler) buildHTTPRequest(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
	data actions.Context,
	timeout time.Duration,
) (actions.Request, error) {
	spec := action.Request
	if spec == nil {
		return actions.Request{}, errors.New("http.request needs a request block")
	}

	credentials, err := r.credentialsFor(ctx, automation, secretNameOf(spec.SecretRef))
	if err != nil {
		return actions.Request{}, err
	}

	target, err := destinationFrom(
		actions.TypeHTTPRequest, "request.url", spec.URL, credentials.URL, secretNameOf(spec.SecretRef))
	if err != nil {
		return actions.Request{}, err
	}

	header := credentials.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	for _, entry := range spec.Headers {
		if http.CanonicalHeaderKey(entry.Name) == "Authorization" {
			return actions.Request{}, errors.New(
				"an Authorization header belongs in the referenced secret, not in the Automation")
		}
		header.Set(entry.Name, entry.Value)
	}

	// The body is the only part of the request a template may reach. The URL
	// and the headers stay literal on purpose: the destination is what the
	// allowlist decided, and letting observed state edit it would hand back the
	// choice the allowlist exists to take away.
	body, err := actions.Render(spec.Body, data)
	if err != nil {
		return actions.Request{}, fmt.Errorf("request.body: %w", err)
	}

	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}
	return actions.Request{
		Method:    method,
		URL:       target,
		Header:    header,
		Body:      []byte(body),
		Retryable: retryable(method, spec.Idempotent),
		Timeout:   timeout,
	}, nil
}

// buildHomeAssistantRequest turns one homeassistant.service action into the
// POST that calls it.
//
// It goes through the same outbound client as everything else here, which is
// the whole design: the destination allowlist, the address floor in the dialer,
// the refusal to follow redirects and the origin-only reporting are properties
// of that client, and an integration that wanted its own transport would be
// opting out of all four. What is specific to Home Assistant is two things —
// the path is built rather than written, and the token is a bearer credential
// that has to be present before anything is sent.
func (r *AutomationReconciler) buildHomeAssistantRequest(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
	data actions.Context,
	timeout time.Duration,
) (actions.Request, error) {
	spec := action.HomeAssistant
	if spec == nil {
		return actions.Request{}, errors.New("homeassistant.service needs a homeAssistant block")
	}

	credentials, err := r.credentialsFor(ctx, automation, spec.SecretRef.Name)
	if err != nil {
		return actions.Request{}, err
	}
	if credentials.Header.Get("Authorization") == "" {
		// Checked before anything is sent, because Home Assistant answers an
		// unauthenticated call with a 401 that says nothing about which
		// Automation produced it or what it was missing.
		return actions.Request{}, fmt.Errorf(
			"secret %q has no %q key; a Home Assistant long-lived access token travels as %q: Bearer <token>",
			spec.SecretRef.Name, actions.SecretKeyAuthorization, actions.SecretKeyAuthorization)
	}

	base, err := destinationFrom(
		actions.TypeHomeAssistant, "homeAssistant.url", spec.URL, credentials.URL, spec.SecretRef.Name)
	if err != nil {
		return actions.Request{}, err
	}
	target, err := actions.HomeAssistantURL(base, spec.Domain, spec.Service)
	if err != nil {
		return actions.Request{}, err
	}

	// The service data is the only part a template may reach, for the same
	// reason the body is on http.request: the domain and the service decide
	// what is being called, and observed state does not get to choose that.
	rendered, err := actions.Render(spec.Data, data)
	if err != nil {
		return actions.Request{}, fmt.Errorf("homeAssistant.data: %w", err)
	}
	payload, err := actions.HomeAssistantPayload(rendered)
	if err != nil {
		return actions.Request{}, err
	}

	header := credentials.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	maps.Copy(header, payload.Header)

	return actions.Request{
		Method: http.MethodPost,
		URL:    target,
		Header: header,
		Body:   payload.Body,
		// At most once unless the author says otherwise. See Idempotent on the
		// API type: a service call is a command, and Reactor cannot tell
		// light.turn_on from script.run_the_thing.
		Retryable: spec.Idempotent != nil && *spec.Idempotent,
		Timeout:   timeout,
	}, nil
}

// buildQBittorrentRequest turns one qbittorrent.pause or qbittorrent.resume
// into the login-act-logout exchange it needs.
//
// Nothing is templated here, and there is nothing to template: the action takes
// no message and no body, and its only parameters are where the instance is and
// who to log in as. That is a smaller surface than http.request, which is the
// trade an integration is supposed to make.
func (r *AutomationReconciler) buildQBittorrentRequest(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
	timeout time.Duration,
) (actions.Request, error) {
	spec := action.QBittorrent
	if spec == nil {
		return actions.Request{}, fmt.Errorf("%s needs a qbittorrent block", action.Type)
	}

	credentials, err := r.credentialsFor(ctx, automation, spec.SecretRef.Name)
	if err != nil {
		return actions.Request{}, err
	}
	base, err := destinationFrom(
		action.Type, "qbittorrent.url", spec.URL, credentials.URL, spec.SecretRef.Name)
	if err != nil {
		return actions.Request{}, err
	}
	if credentials.Username == "" || credentials.Password == "" {
		return actions.Request{}, fmt.Errorf(
			"secret %q needs both a %q and a %q key: qBittorrent issues a session rather than accepting a token",
			spec.SecretRef.Name, actions.SecretKeyUsername, actions.SecretKeyPassword)
	}

	return actions.QBittorrentRequest(
		action.Type, base, credentials.Username, credentials.Password, timeout)
}

// retryable decides whether a failed request may be sent again.
//
// This is the per-type at-most-once decision #33 asks for, made from the only
// evidence available: a generic HTTP endpoint might be a webhook or it might
// create an order, and Reactor cannot tell. RFC 9110 says GET and PUT are
// idempotent, so those retry; POST and PATCH are attempted exactly once unless
// the author says otherwise, because a duplicate side effect is worse than a
// missed one when nobody knows what the side effect is.
func retryable(method string, idempotent *bool) bool {
	if idempotent != nil {
		return *idempotent
	}
	return method == http.MethodGet || method == http.MethodPut
}

func (r *AutomationReconciler) buildNotification(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	action reactorv1alpha1.Action,
	data actions.Context,
	timeout time.Duration,
) (actions.Request, error) {
	spec := action.Notification
	if spec == nil {
		return actions.Request{}, fmt.Errorf("%s needs a notification block", action.Type)
	}

	credentials, err := r.credentialsFor(ctx, automation, spec.SecretRef.Name)
	if err != nil {
		return actions.Request{}, err
	}
	if credentials.URL == "" {
		return actions.Request{}, fmt.Errorf(
			"secret %q has no %q key; a notification destination is a credential and lives only in the secret",
			spec.SecretRef.Name, actions.SecretKeyURL)
	}

	title, err := actions.Render(spec.Title, data)
	if err != nil {
		return actions.Request{}, fmt.Errorf("notification.title: %w", err)
	}
	message, err := actions.Render(spec.Message, data)
	if err != nil {
		return actions.Request{}, fmt.Errorf("notification.message: %w", err)
	}

	payload, err := actions.NotificationPayload(action.Type, title, message)
	if err != nil {
		return actions.Request{}, err
	}
	header := credentials.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	maps.Copy(header, payload.Header)

	return actions.Request{
		Method: http.MethodPost,
		URL:    credentials.URL,
		Header: header,
		Body:   payload.Body,
		// Notifications retry, unlike an unknown POST: a duplicate is noise on
		// a phone, and a missed one is the failure this feature exists to stop.
		// Every transport shipped is a publish, not a command.
		Retryable: true,
		Timeout:   timeout,
	}, nil
}

func secretNameOf(ref *reactorv1alpha1.SecretReference) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

// credentialsFor reads one action's credential Secret.
//
// It reads from the Automation's own namespace and no other: anyone who can
// write an Automation can already write a Secret beside it, so nothing is lost,
// while a cross-namespace read would let them borrow Reactor's cluster-wide
// reach to fetch a credential they cannot read themselves.
//
// The read goes through SecretReader — the manager's uncached API reader —
// rather than the cached client, because a cached Get would start an informer
// and hold every Secret in the cluster in the operator's memory for the rest of
// the process's life.
func (r *AutomationReconciler) credentialsFor(
	ctx context.Context,
	automation *reactorv1alpha1.Automation,
	name string,
) (actions.Credentials, error) {
	if name == "" {
		return actions.Credentials{}, nil
	}
	reader := r.SecretReader
	if reader == nil {
		reader = r.Client
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: automation.Namespace, Name: name}
	if err := reader.Get(ctx, key, &secret); err != nil {
		// Safe to surface: a Get error names the resource asked for, never its
		// contents, and this text ends up in status where the operator needs to
		// tell "no such Secret" from "no permission to read it".
		return actions.Credentials{}, fmt.Errorf("reading secret %q in %s: %w",
			name, automation.Namespace, err)
	}
	return actions.CredentialsFrom(name, secret.Data)
}
