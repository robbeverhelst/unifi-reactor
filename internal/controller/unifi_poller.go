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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/events"
	"github.com/robbeverhelst/unifi-reactor/internal/providers/unifi"
)

// UniFiPoller periodically observes UniFi WAN state into the shared
// StateStore. Polling is the source of truth: it makes the system
// self-healing after restarts and immune to missed webhook deliveries.
type UniFiPoller struct {
	Client   *unifi.Client
	Store    *engine.StateStore
	Interval time.Duration
}

// Start implements manager.Runnable; the manager cancels ctx on shutdown.
func (p *UniFiPoller) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("unifi-poller")
	interval := p.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		state, err := p.Client.ObserveWANState(ctx)
		if err != nil {
			log.Error(err, "state observation failed")
		} else {
			observation := events.Observation{Provider: providerUniFi, State: state, ObservedAt: time.Now()}
			transitions := p.Store.Observe(observation)
			log.V(1).Info("state observed", "state", state)
			for _, t := range transitions {
				log.Info("state transition", "provider", t.Provider, "key", t.Key, "from", t.From, "to", t.To)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection ensures only the active manager polls and drives state.
func (p *UniFiPoller) NeedLeaderElection() bool { return true }
