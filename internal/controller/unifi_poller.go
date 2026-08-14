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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
	"github.com/robbeverhelst/unifi-reactor/internal/providers/unifi"
)

// UniFiPoller periodically observes UniFi state into the shared StateStore.
// Polling is the source of truth: it makes the system self-healing after
// restarts and immune to missed webhook deliveries. The webhook fast path can
// only make an observation happen sooner, never make one happen differently.
type UniFiPoller struct {
	Client   *unifi.Client
	Store    *engine.StateStore
	Interval time.Duration

	// Reader lists the Automations to wake when state changes. Optional; when
	// nil, Automations are only re-evaluated on their periodic requeue.
	Reader client.Reader
	// Events wakes the controller as soon as a transition is observed, instead
	// of leaving it to notice on its next periodic re-evaluation.
	Events chan<- event.GenericEvent

	// Nudge carries out-of-band re-observation requests from the webhook fast
	// path. A nudge says only "look now"; the observation below is what decides
	// what changed, so a dropped, duplicated, replayed or forged delivery can
	// cost an extra poll and nothing else. Receiving from a nil channel blocks
	// forever, so leaving this unset simply means poll-only.
	Nudge <-chan struct{}
	// MinObserveInterval floors the time between two observations, so a burst
	// of deliveries cannot become a burst of requests to the console.
	MinObserveInterval time.Duration
}

// Start implements manager.Runnable; the manager cancels ctx on shutdown.
func (p *UniFiPoller) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("unifi-poller")
	ctx = logf.IntoContext(ctx, log)
	interval := p.Interval
	if interval <= 0 {
		interval = unifi.DefaultPollInterval
	}
	minInterval := p.MinObserveInterval
	if minInterval <= 0 {
		minInterval = unifi.DefaultMinObserveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		p.observe(ctx)
		// Restart the interval from the observation that just happened, so a
		// nudge-driven poll does not leave a periodic one due immediately after.
		ticker.Reset(interval)

		if !p.awaitNextObservation(ctx, ticker.C, minInterval, time.Now()) {
			return nil
		}
	}
}

// awaitNextObservation blocks until it is time to observe again, reporting
// false if the context ended first.
//
// A webhook delivery brings the next observation forward, under two limits.
// MinObserveInterval floors how often that can happen, so a burst of
// deliveries cannot become a burst of requests to the console. And while a
// value is still proving itself against the store's debounce threshold, a
// delivery is ignored outright: those samples stay on the poll cadence.
//
// The second limit is the important one. A delivery may make Reactor look
// sooner, but it must never supply the evidence that promotes a value —
// otherwise anyone who can reach the endpoint could fast-forward a debounced
// key straight through the settling time its operator asked for, which is the
// one thing debounce exists to prevent.
func (p *UniFiPoller) awaitNextObservation(ctx context.Context, tick <-chan time.Time,
	minInterval time.Duration, observedAt time.Time) bool {
	log := logf.FromContext(ctx)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-tick:
			return true
		case <-p.Nudge:
			if p.Store != nil && p.Store.Proving(unifi.ProviderName) {
				log.V(1).Info("ignoring a webhook delivery while a value is still settling")
				continue
			}
			log.V(1).Info("re-observing early on a webhook delivery")
			return waitFor(ctx, minInterval-time.Since(observedAt))
		}
	}
}

// observe performs one observation and records the transitions it found. Errors
// are logged and dropped: the next observation is the recovery mechanism.
func (p *UniFiPoller) observe(ctx context.Context) {
	log := logf.FromContext(ctx)
	state, err := p.Client.Observe(ctx)
	if err != nil {
		metrics.ObservationFailed(unifi.ProviderName)
		log.Error(err, "state observation failed")
		return
	}
	observedAt := time.Now()
	observation := events.Observation{Provider: unifi.ProviderName, State: state, ObservedAt: observedAt}
	transitions := p.Store.Observe(observation)
	// Exported from the store rather than from the reading above, so what is
	// graphed is what Automations act on: a value still proving itself against
	// its debounce threshold has been reported to nobody.
	reported, _ := p.Store.Get(unifi.ProviderName)
	metrics.ObservationSucceeded(unifi.ProviderName, reported.State, observedAt)
	log.V(1).Info("state observed", "state", state)
	for _, t := range transitions {
		metrics.TransitionObserved(t.Provider, t.Key)
		log.Info("state transition", "provider", t.Provider, "key", t.Key, "from", t.From, "to", t.To)
	}
	if len(transitions) > 0 {
		p.wake(ctx)
	}
}

// waitFor sleeps out d, reporting false if the context ended first. A
// non-positive duration returns immediately.
func waitFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// wake enqueues every Automation driven by this provider for immediate
// reconciliation. Without it, the only path back into the reconciler after a
// state change is the periodic requeue, which adds up to one re-evaluation
// interval of latency to every reaction.
func (p *UniFiPoller) wake(ctx context.Context) {
	log := logf.FromContext(ctx)
	if p.Events == nil || p.Reader == nil {
		return
	}
	var list reactorv1alpha1.AutomationList
	if err := p.Reader.List(ctx, &list); err != nil {
		log.Error(err, "listing automations to wake")
		return
	}
	for i := range list.Items {
		automation := &list.Items[i]
		if automation.Spec.When == nil || automation.Spec.When.Provider != unifi.ProviderName {
			continue
		}
		select {
		case p.Events <- event.GenericEvent{Object: automation}:
		case <-ctx.Done():
			return
		default:
			// Never let a saturated queue stall observation: the periodic
			// re-evaluation still picks this Automation up.
			log.V(1).Info("wake queue full, leaving it to periodic re-evaluation",
				"automation", client.ObjectKeyFromObject(automation))
		}
	}
}

// NeedLeaderElection ensures only the active manager polls and drives state.
func (p *UniFiPoller) NeedLeaderElection() bool { return true }
