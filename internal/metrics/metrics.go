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

// Package metrics publishes what Reactor observed, what matched, what it did,
// and how fast — the decision layer, which is the only thing Reactor knows that
// nothing else in a cluster does. Raw vendor telemetry is deliberately not
// re-exported; a vendor exporter already covers that ground better.
//
// Everything registers on controller-runtime's own registry, so the series
// appear on the metrics endpoint the manager already serves. There is no second
// server, no second port, and no second auth posture to reason about.
//
// # What is a label here, and what is not
//
// Every label on this page is a deliberate decision, because a label whose
// value set is open turns one metric into an unbounded number of time series
// that a Prometheus instance keeps for its whole retention period.
//
//   - provider, key: bounded by what is compiled in, plus whatever an operator
//     has explicitly opted into. A handful of each.
//
//     This is where per-entity keys landed. device.<name> is the first key
//     whose NAME comes from the outside world rather than from this repository,
//     and the rule this comment set before it existed was that such keys have
//     to become opt-in rather than that this paragraph gets revisited — so they
//     did: the UniFi provider publishes the aggregate devices key always and
//     the per-device keys only when asked, which is what keeps a forty-device
//     fleet from silently becoming forty series. client.<name> takes the same
//     shape when it lands. What is bounded here is therefore still bounded at
//     compile time by default, and bounded by a deliberate choice otherwise.
//
//   - value: bounded ONLY for keys whose provider declares a closed value set,
//     via SetVocabulary. A key with an open value set — isp, whose values are
//     carrier names derived from whatever public address the gateway holds —
//     is never labelled by value, because one such key is enough to blow up an
//     instance. Its transitions are still counted, and its current value is
//     still in the Automation's status and in a Kubernetes Event. A key with an
//     open NAME is left out of SetVocabulary for the same reason from the other
//     direction: device.<name>'s two values are closed, and the set of keys
//     holding them is not, so it gets no reactor_state_info series at all.
//
//   - namespace, name of an Automation: unbounded in principle, self-limiting
//     in practice — a new series appears only when a human writes another
//     policy object, never on its own. What makes that safe is ForgetAutomation:
//     a deleted Automation's series are dropped rather than left reporting
//     matching forever.
//
//   - A target reference, a claimant, a state VALUE for an open key, and any
//     error string are NOT labels. "How often" is this package's question;
//     "which one" is answered by status and by Events, which cost nothing to
//     keep and are attached to the object they describe.
//
// Reconcile counts, queue depth and reconcile latency are not defined here:
// controller-runtime already exports controller_runtime_reconcile_* on the same
// endpoint, and a second implementation would only be a second thing to trust.
package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label values, spelled once so a query written against one call site keeps
// working when another is added.
const (
	// ResultSuccess and ResultError are the outcome of an observation or an
	// action attempt; ResultSkipped is an action that never ran.
	ResultSuccess = "success"
	ResultError   = "error"
	ResultSkipped = "skipped"

	// KindDesiredState and KindEdge are the two shapes an action has, and they
	// fail differently enough that no alert should be written without saying
	// which it means.
	//
	// A desired-state action is what the Automation is for: it declares a level
	// for a target, it is arbitrated, it is retried across reconciles, and a
	// failure makes the Automation not Ready. An edge action reports an
	// occurrence that has already happened; it is attempted once per transition,
	// its retry policy is decided per type, and a failure is recorded without
	// making the Automation not Ready — a notification that did not go out did
	// not stop the workload being scaled.
	//
	// The label is redundant with type today, and deliberately so: type is an
	// open list that grows, kind is the distinction an alert actually means, and
	// an alert written as type!~"kubernetes.*" silently breaks the day a second
	// desired-state type lands.
	KindDesiredState = "desired_state"
	KindEdge         = "edge"

	// OutcomeClaimed is a target held at the value this Automation asked for,
	// OutcomeDeferred one held at a peer's more restrictive value, and
	// OutcomeReleased one handed back because nothing claims it any more.
	OutcomeClaimed  = "claimed"
	OutcomeDeferred = "deferred"
	OutcomeReleased = "released"
	// OutcomeDeclined is a target Reactor refused to arbitrate at all, because
	// a controller it cannot fold into the arbitration already drives the same
	// field. A rising count is a configuration to fix rather than an incident:
	// somebody has pointed an Automation at a workload something else owns.
	OutcomeDeclined = "declined"
	// OutcomeWithheld is a target that was arbitrated and then not written,
	// because the install is running as a dry run. It is the one series that
	// answers "is this install actually doing anything?" without reading a
	// single Automation: a dry run publishes only these, and a live one
	// publishes none.
	OutcomeWithheld = "withheld"

	// DeliveryAccepted is a delivery that caused a re-observation,
	// DeliveryCoalesced one that arrived while another was already pending, and
	// DeliveryRejected one that presented no valid token.
	DeliveryAccepted  = "accepted"
	DeliveryCoalesced = "coalesced"
	DeliveryRejected  = "rejected"
)

// Label names, spelled once. Repeating them is where a typo turns into a
// series that simply never appears.
const (
	labelProvider  = "provider"
	labelKey       = "key"
	labelValue     = "value"
	labelResult    = "result"
	labelType      = "type"
	labelKind      = "kind"
	labelOnExit    = "on_exit"
	labelOutcome   = "outcome"
	labelNamespace = "namespace"
	labelName      = "name"
	labelSignal    = "signal"
)

var (
	// lastObservation is the highest-value series here: `time() - this` is the
	// only signal that Reactor has gone blind, which is the failure mode the
	// whole design is otherwise silent about.
	lastObservation = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reactor_last_observation_timestamp_seconds",
		Help: "Unix timestamp of the last successful observation, per provider. " +
			"time() minus this value is how long Reactor has been unable to see.",
	}, []string{labelProvider})

	observations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_observations_total",
		Help: "Observations attempted, by provider and outcome.",
	}, []string{labelProvider, labelResult})

	// stateInfo is published only for keys whose value set the provider
	// declares closed. See the package comment.
	stateInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reactor_state_info",
		Help: "1 for the value a state key currently holds, 0 for every other value its provider declares. " +
			"All values 0 means the key is not currently observable. " +
			"Keys with an open value set are deliberately absent.",
	}, []string{labelProvider, labelKey, labelValue})

	// transitions is not labelled by from/to: one key with an open value set
	// would make from x to unbounded, and which values a key moved between is
	// already recorded in the Automation's status and in a Kubernetes Event.
	transitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_state_transitions_total",
		Help: "State transitions reported by the store, by provider and key.",
	}, []string{labelProvider, labelKey})

	automationMatching = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reactor_automation_matching",
		Help: "1 while an Automation's condition holds, 0 while it does not.",
	}, []string{labelNamespace, labelName})

	automationReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reactor_automation_ready",
		Help: "1 while an Automation reports Ready=True, 0 otherwise. " +
			"An Automation that is outvoted on a target is still Ready.",
	}, []string{labelNamespace, labelName})

	arbitrations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_arbitrations_total",
		Help: "Arbitrated target outcomes: claimed, deferred to a more restrictive peer, released, " +
			"declined because another controller already drives the target, or withheld because the " +
			"install runs as a dry run.",
	}, []string{labelOutcome})

	actions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_actions_total",
		Help: "Action executions, by type and kind, outcome, and whether the Automation was reversing " +
			"rather than claiming. A failed edge action does not make its Automation unhealthy.",
	}, []string{labelType, labelKind, labelResult, labelOnExit})

	actionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "reactor_action_duration_seconds",
		Help: "Time one action attempt took, by type. Bounded above by the action's timeoutSeconds. " +
			"For an edge action this covers every retry it was allowed.",
		// Spans a fast local patch to the default 30s action timeout.
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{labelType})

	// providerSignals counts the moments a provider's independent signals fail
	// to agree. The signal label is a closed set the provider spells out, never
	// the values that disagreed: those come from the outside world.
	providerSignals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_provider_signal_disagreements_total",
		Help: "Times two independent signals for the same fact disagreed. Nothing stops; the log line " +
			"carries the detail, and a rising count is how a wrong derivation announces itself.",
	}, []string{labelProvider, labelSignal})

	// reversalDisagreements is the arbitration sibling of providerSignals above:
	// two sources of one fact that do not agree, counted rather than resolved.
	// There the fact is what the network is doing; here it is what a workload's
	// normal size is, and the disagreeing sources are two Automations.
	//
	// Unlabelled on purpose. Which target and which claimants is exactly the
	// unbounded half — a target reference and a claimant are not labels here —
	// and both are already in status.targets[].reversalDisagreement and in the
	// Event, attached to the objects they describe.
	reversalDisagreements = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "reactor_reversal_disagreements_total",
		Help: "Times the Automations sharing one target declared different reversal levels for it, so they " +
			"disagree about that workload's normal size. Reactor still takes the most restrictive; the " +
			"Automation's status and its Event name both claimants and both levels.",
	})

	// staleDecisions is the other half of lastObservation, and the half that is
	// attributable. The gauge says Reactor has gone blind; this says automations
	// were still deciding while it was, which is the part that reaches a
	// workload. It is published only by an install that set a bound, so on every
	// other install it is absent rather than zero.
	staleDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_stale_decisions_total",
		Help: "Reconciles that acted on provider state older than the age this install allows. " +
			"Reactor deliberately keeps acting — going blind must not release a claim mid-incident — " +
			"so this counts decisions taken against state nothing has confirmed since.",
	}, []string{labelProvider})

	// reactionLatency is the metric that would have caught the v0.3.0 latency
	// bug the week it shipped, instead of by hand-reading log timestamps.
	reactionLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "reactor_reaction_latency_seconds",
		Help: "Time from the observation that changed a condition to the action it caused completing.",
		// Spans the webhook fast path (sub-second) to several poll intervals.
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{labelProvider})

	webhookDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reactor_webhook_deliveries_total",
		Help: "Webhook fast-path deliveries, by outcome. Losing these costs reaction latency and nothing else.",
	}, []string{labelResult})
)

func init() {
	crmetrics.Registry.MustRegister(
		lastObservation,
		observations,
		stateInfo,
		transitions,
		automationMatching,
		automationReady,
		arbitrations,
		actions,
		actionDuration,
		providerSignals,
		reversalDisagreements,
		staleDecisions,
		reactionLatency,
		webhookDeliveries,
	)
}

// vocabulary holds the closed value sets providers have declared, and which of
// their keys have been observed at least once.
//
// Values are published lazily, per key, on first observation. A provider that
// declares ups but runs on a site with no UPS adopted therefore publishes no
// ups series at all — the same "omit what you cannot see" rule the state model
// itself follows — rather than a row of zeroes that reads like an observation.
var vocabulary = struct {
	sync.Mutex
	declared map[string]map[string][]string
	seen     map[string]map[string]bool
}{
	declared: map[string]map[string][]string{},
	seen:     map[string]map[string]bool{},
}

// SetVocabulary declares a provider's closed value sets, which is what lets
// reactor_state_info report 0 for the values a key does not currently hold
// rather than leaving stale series behind at 1.
//
// The map is opaque data: this package never learns what a key or a value
// means, only how many of them there can be. A key the provider cannot
// enumerate the values of is simply left out, and is then never labelled by
// value anywhere.
func SetVocabulary(provider string, declared map[string][]string) {
	vocabulary.Lock()
	defer vocabulary.Unlock()
	vocabulary.declared[provider] = declared
	if _, ok := vocabulary.seen[provider]; !ok {
		vocabulary.seen[provider] = map[string]bool{}
	}
}

// ObservationSucceeded records one successful observation and the state it
// reported. Pass the store's reported state rather than the raw reading, so
// what is graphed is what Automations actually acted on — a value still proving
// itself against a debounce threshold has not been reported to anyone.
func ObservationSucceeded(provider string, state map[string]string, at time.Time) {
	observations.WithLabelValues(provider, ResultSuccess).Inc()
	lastObservation.WithLabelValues(provider).Set(float64(at.Unix()))
	publishState(provider, state)
}

// ObservationFailed records an observation that could not be made. The
// timestamp gauge is deliberately left where it was: how long Reactor has been
// blind is the question, and a failed attempt does not answer it.
func ObservationFailed(provider string) {
	observations.WithLabelValues(provider, ResultError).Inc()
}

func publishState(provider string, state map[string]string) {
	vocabulary.Lock()
	defer vocabulary.Unlock()

	declared, ok := vocabulary.declared[provider]
	if !ok {
		return
	}
	seen := vocabulary.seen[provider]
	for key, values := range declared {
		got, present := state[key]
		if !present && !seen[key] {
			// Never observed, so nothing is claimed about it either way.
			continue
		}
		seen[key] = true
		for _, value := range values {
			active := 0.0
			if present && value == got {
				active = 1
			}
			stateInfo.WithLabelValues(provider, key, value).Set(active)
		}
	}
}

// TransitionObserved records one key changing value.
func TransitionObserved(provider, key string) {
	transitions.WithLabelValues(provider, key).Inc()
}

// AutomationEvaluated publishes an Automation's current condition. It is called
// wherever status is written, so the two can never disagree.
func AutomationEvaluated(namespace, name string, matching, ready bool) {
	automationMatching.WithLabelValues(namespace, name).Set(boolValue(matching))
	automationReady.WithLabelValues(namespace, name).Set(boolValue(ready))
}

// ForgetAutomation drops a deleted Automation's series. Without it a deleted
// policy would keep reporting matching until the process restarts, and the
// namespace/name labels would be an unbounded set rather than a self-limiting
// one.
func ForgetAutomation(namespace, name string) {
	automationMatching.DeleteLabelValues(namespace, name)
	automationReady.DeleteLabelValues(namespace, name)
}

// ArbitrationResolved records what happened to one target this reconcile.
func ArbitrationResolved(outcome string) {
	arbitrations.WithLabelValues(outcome).Inc()
}

// ActionExecuted records one action attempt that ran: its outcome, how long it
// took, and whether the Automation was reversing rather than claiming.
func ActionExecuted(actionType, kind string, onExit bool, err error, took time.Duration) {
	result := ResultSuccess
	if err != nil {
		result = ResultError
	}
	actions.WithLabelValues(actionType, kind, result, strconv.FormatBool(onExit)).Inc()
	actionDuration.WithLabelValues(actionType).Observe(took.Seconds())
}

// ActionSkipped records an action that never ran, so it is neither a success
// nor a failure and contributes no duration to reason about.
func ActionSkipped(actionType, kind string, onExit bool) {
	actions.WithLabelValues(actionType, kind, ResultSkipped, strconv.FormatBool(onExit)).Inc()
}

// SignalsDisagreed records two independent signals for the same fact failing to
// agree. signal names which comparison, from a closed set the provider spells
// out; the values that disagreed stay in the log line, because they come from
// the outside world and would be unbounded here.
func SignalsDisagreed(provider, signal string) {
	providerSignals.WithLabelValues(provider, signal).Inc()
}

// ReversalDisagreement records one target whose Automations do not agree on
// what its normal level is. Like ArbitrationResolved it is called per reconcile
// that resolved the target, so the series describes how long a contradiction
// has been standing rather than how many times somebody noticed it.
func ReversalDisagreement() {
	reversalDisagreements.Inc()
}

// StaleDecision records one reconcile taken against state older than the age
// this install allows. It is called per reconcile rather than per transition
// because that is the question: not how often Reactor went blind, but how much
// deciding it did while it was.
func StaleDecision(provider string) {
	staleDecisions.WithLabelValues(provider).Inc()
}

// ReactionCompleted records how long it took to get from the observation that
// changed a condition to the action it caused. A zero observedAt is ignored:
// nothing was observed, so there is no latency to attribute.
func ReactionCompleted(provider string, observedAt time.Time) {
	if observedAt.IsZero() {
		return
	}
	reactionLatency.WithLabelValues(provider).Observe(time.Since(observedAt).Seconds())
}

// WebhookDelivery records one fast-path delivery.
func WebhookDelivery(result string) {
	webhookDeliveries.WithLabelValues(result).Inc()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
