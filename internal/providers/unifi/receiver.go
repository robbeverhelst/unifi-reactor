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
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Defaults for the webhook fast path.
const (
	// DefaultWebhookBindAddress is a port of its own, deliberately not the
	// metrics or health port: those are exposed to the cluster on different
	// terms than a port a network appliance is invited to talk to.
	DefaultWebhookBindAddress = ":9090"

	// DefaultWebhookPath is the path the design spec names.
	DefaultWebhookPath = "/webhooks/unifi"

	// DefaultMinObserveInterval floors how often a delivery may force an
	// observation. Deliveries arrive in bursts — a real outage fires several
	// triggers at once, and a retrying console fires the same one repeatedly —
	// and the console should not be asked about its own state once per
	// delivery. It is well under a second so the fast path still is one.
	DefaultMinObserveInterval = 500 * time.Millisecond
)

const (
	// maxDeliveryBytes bounds what the receiver will read from a delivery
	// before discarding it. Nothing is parsed, so this only exists to stop an
	// unbounded or slow body from occupying the handler.
	maxDeliveryBytes = 64 << 10

	authorizationHeader = "Authorization"
	bearerPrefix        = "Bearer "

	// tokenHeader is an alternative to Authorization, for a rule configured by
	// hand in the UniFi UI using the Alarm Manager's custom-headers list.
	tokenHeader = "X-Reactor-Token"
)

// Receiver is the UniFi webhook fast path: an HTTP endpoint the console's
// Alarm Manager posts to, whose entire job is to ask the poller to observe
// again, now, instead of at the next tick.
//
// It never reads the delivery body. That is the whole design: polling is the
// mechanism of record, a delivery is only a hint that something may have
// changed, and the poll decides what actually did. Because no state is derived
// from a payload, a delivery that is dropped, duplicated, replayed, delayed or
// forged costs at most one extra observation of the console, and can never move
// the cluster into a state the console does not actually report.
type Receiver struct {
	// Addr is the address to listen on, e.g. ":9090".
	Addr string
	// Path is the URL path deliveries are accepted on.
	Path string
	// Token is the shared secret every delivery must present, as either
	// "Authorization: Bearer <token>" or "X-Reactor-Token: <token>". An empty
	// token rejects everything rather than accepting everything.
	Token string

	requests chan struct{}
	log      logr.Logger
}

// NewReceiver builds a receiver from the provider configuration. Call
// Validate on the configuration first; a receiver built from an invalid one
// simply refuses every delivery.
func NewReceiver(cfg WebhookConfig) *Receiver {
	addr := cfg.BindAddress
	if addr == "" {
		addr = DefaultWebhookBindAddress
	}
	path := cfg.Path
	if path == "" {
		path = DefaultWebhookPath
	}
	return &Receiver{
		Addr: addr,
		Path: path,
		// A single slot is the point: see request.
		requests: make(chan struct{}, 1),
		Token:    cfg.Token,
		log:      logf.Log.WithName("unifi-webhook"),
	}
}

// Requests emits one value per pending re-observation request. Reading from it
// is how the poller learns a delivery arrived; a nil Receiver's channel is
// never nil, so the poller never has to special-case the fast path being off.
func (r *Receiver) Requests() <-chan struct{} { return r.requests }

// request asks for one re-observation. The channel holds a single slot and the
// send never blocks, so a burst — a retry storm, a duplicate, an outage firing
// three triggers at once, or a flood from whoever found the endpoint —
// collapses into one pending observation rather than one per delivery.
func (r *Receiver) request() bool {
	select {
	case r.requests <- struct{}{}:
		return true
	default:
		return false
	}
}

// Handler is the receiver's routing, exported so tests can exercise the
// endpoint without binding a port.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	// Method-qualified: anything but POST to this path is a 405, and any other
	// path is a 404, without the handler having to decide.
	mux.HandleFunc(http.MethodPost+" "+r.Path, r.handleDelivery)
	return mux
}

func (r *Receiver) handleDelivery(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(req) {
		// The response says nothing about which part was wrong, and nothing
		// about the console or the cluster behind it.
		r.log.V(1).Info("Rejected a webhook delivery presenting no valid token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Read and discard, bounded. Not parsing the payload is what makes a forged
	// delivery harmless: there is nothing in it Reactor can be told.
	read, _ := io.Copy(io.Discard, http.MaxBytesReader(w, req.Body, maxDeliveryBytes))

	if r.request() {
		r.log.V(1).Info("Accepted a webhook delivery; re-observing early", "bytes", read)
	} else {
		r.log.V(1).Info("Accepted a webhook delivery; a re-observation is already pending", "bytes", read)
	}

	// Accepted, not OK: the work this delivery asks for has not happened yet,
	// and the console is never made to wait for it.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"accepted":true}`)
}

// authorized checks the shared secret in constant time. UniFi's Alarm Manager
// offers no payload signature, so a shared secret is the strongest thing the
// console can actually be asked to present.
func (r *Receiver) authorized(req *http.Request) bool {
	if r.Token == "" {
		return false
	}
	presented := req.Header.Get(tokenHeader)
	if presented == "" {
		if after, found := strings.CutPrefix(req.Header.Get(authorizationHeader), bearerPrefix); found {
			presented = after
		}
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(r.Token)) == 1
}

// Start implements manager.Runnable; the manager cancels ctx on shutdown.
//
// It always returns nil. A fast path that cannot listen is a lost optimization,
// and returning an error here would stop the manager and take the poller — the
// mechanism of record — down with it.
func (r *Receiver) Start(ctx context.Context) error {
	r.log = logf.FromContext(ctx).WithName("unifi-webhook")

	server := &http.Server{
		Addr:    r.Addr,
		Handler: r.Handler(),
		// A network appliance is not a trusted client: bound how long it may
		// hold a connection open before completing a request.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	r.log.Info("Webhook fast path listening", "address", r.Addr, "path", r.Path)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.log.Error(err, "Webhook fast path could not serve; continuing on the poll interval alone",
			"address", r.Addr)
		return nil
	}
	<-stopped
	return nil
}

// NeedLeaderElection reports false so the endpoint answers on every replica.
// Only the leader polls, so a delivery to a standby is accepted and dropped —
// which is exactly what a missed delivery already costs: nothing but latency.
func (r *Receiver) NeedLeaderElection() bool { return false }
