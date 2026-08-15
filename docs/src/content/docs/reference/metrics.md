---
title: "Metrics"
description: "Every Prometheus series Reactor publishes: its type, its labels, and why it exists."
editUrl: false
tableOfContents:
  maxHeadingLevel: 2
---

:::note[Generated from source]
This page is generated from `internal/metrics/metrics.go` by `make docs`. Change it there — CI fails when this file and its source disagree.
:::

Every series Reactor publishes, on the metrics endpoint the manager already serves. Turn it on with `metrics.enabled`; [Metrics, alerts and dashboard](/operations/metrics-and-alerts/) covers the auth posture and the alert rules the chart ships.

| Metric | Type | Labels |
| --- | --- | --- |
| [`reactor_last_observation_timestamp_seconds`](#reactor_last_observation_timestamp_seconds) | gauge | `provider` |
| [`reactor_observations_total`](#reactor_observations_total) | counter | `provider`, `result` |
| [`reactor_state_info`](#reactor_state_info) | gauge | `provider`, `key`, `value` |
| [`reactor_state_transitions_total`](#reactor_state_transitions_total) | counter | `provider`, `key` |
| [`reactor_automation_matching`](#reactor_automation_matching) | gauge | `namespace`, `name` |
| [`reactor_automation_ready`](#reactor_automation_ready) | gauge | `namespace`, `name` |
| [`reactor_arbitrations_total`](#reactor_arbitrations_total) | counter | `outcome` |
| [`reactor_actions_total`](#reactor_actions_total) | counter | `type`, `kind`, `result`, `on_exit` |
| [`reactor_action_duration_seconds`](#reactor_action_duration_seconds) | histogram | `type` |
| [`reactor_provider_signal_disagreements_total`](#reactor_provider_signal_disagreements_total) | counter | `provider`, `signal` |
| [`reactor_reversal_disagreements_total`](#reactor_reversal_disagreements_total) | counter | none |
| [`reactor_stale_decisions_total`](#reactor_stale_decisions_total) | counter | `provider` |
| [`reactor_reaction_latency_seconds`](#reactor_reaction_latency_seconds) | histogram | `provider` |
| [`reactor_webhook_deliveries_total`](#reactor_webhook_deliveries_total) | counter | `result` |

## About these series

Package metrics publishes what Reactor observed, what matched, what it did,
and how fast — the decision layer, which is the only thing Reactor knows that
nothing else in a cluster does. Raw vendor telemetry is deliberately not
re-exported; a vendor exporter already covers that ground better.

Everything registers on controller-runtime's own registry, so the series
appear on the metrics endpoint the manager already serves. There is no second
server, no second port, and no second auth posture to reason about.

### What is a label here, and what is not

Every label on this page is a deliberate decision, because a label whose
value set is open turns one metric into an unbounded number of time series
that a Prometheus instance keeps for its whole retention period.

- provider, key: bounded by what is compiled in, plus whatever an operator
  has explicitly opted into. A handful of each.

  This is where per-entity keys landed. device.&lt;name> is the first key
  whose NAME comes from the outside world rather than from this repository,
  and the rule this comment set before it existed was that such keys have
  to become opt-in rather than that this paragraph gets revisited — so they
  did: the UniFi provider publishes the aggregate devices key always and
  the per-device keys only when asked, which is what keeps a forty-device
  fleet from silently becoming forty series. client.&lt;name> takes the same
  shape when it lands. What is bounded here is therefore still bounded at
  compile time by default, and bounded by a deliberate choice otherwise.

- value: bounded ONLY for keys whose provider declares a closed value set,
  via SetVocabulary. A key with an open value set — isp, whose values are
  carrier names derived from whatever public address the gateway holds —
  is never labelled by value, because one such key is enough to blow up an
  instance. Its transitions are still counted, and its current value is
  still in the Automation's status and in a Kubernetes Event. A key with an
  open NAME is left out of SetVocabulary for the same reason from the other
  direction: device.&lt;name>'s two values are closed, and the set of keys
  holding them is not, so it gets no reactor_state_info series at all.

- namespace, name of an Automation: unbounded in principle, self-limiting
  in practice — a new series appears only when a human writes another
  policy object, never on its own. What makes that safe is ForgetAutomation:
  a deleted Automation's series are dropped rather than left reporting
  matching forever.

- A target reference, a claimant, a state VALUE for an open key, and any
  error string are NOT labels. "How often" is this package's question;
  "which one" is answered by status and by Events, which cost nothing to
  keep and are attached to the object they describe.

Reconcile counts, queue depth and reconcile latency are not defined here:
controller-runtime already exports controller_runtime_reconcile_* on the same
endpoint, and a second implementation would only be a second thing to trust.

## `reactor_last_observation_timestamp_seconds`

**Type:** gauge &nbsp;·&nbsp; **Labels:** `provider`

Unix timestamp of the last successful observation, per provider. time() minus this value is how long Reactor has been unable to see.

The highest-value series here: `time() - this` is the
only signal that Reactor has gone blind, which is the failure mode the
whole design is otherwise silent about.

## `reactor_observations_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `provider`, `result`

Observations attempted, by provider and outcome.

## `reactor_state_info`

**Type:** gauge &nbsp;·&nbsp; **Labels:** `provider`, `key`, `value`

1 for the value a state key currently holds, 0 for every other value its provider declares. All values 0 means the key is not currently observable. Keys with an open value set are deliberately absent.

Published only for keys whose value set the provider
declares closed. See the package comment.

## `reactor_state_transitions_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `provider`, `key`

State transitions reported by the store, by provider and key.

Not labelled by from/to: one key with an open value set
would make from x to unbounded, and which values a key moved between is
already recorded in the Automation's status and in a Kubernetes Event.

## `reactor_automation_matching`

**Type:** gauge &nbsp;·&nbsp; **Labels:** `namespace`, `name`

1 while an Automation's condition holds, 0 while it does not.

## `reactor_automation_ready`

**Type:** gauge &nbsp;·&nbsp; **Labels:** `namespace`, `name`

1 while an Automation reports Ready=True, 0 otherwise. An Automation that is outvoted on a target is still Ready.

## `reactor_arbitrations_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `outcome`

Arbitrated target outcomes: claimed, deferred to a more restrictive peer, released, declined because another controller already drives the target, or withheld because the install runs as a dry run.

## `reactor_actions_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `type`, `kind`, `result`, `on_exit`

Action executions, by type and kind, outcome, and whether the Automation was reversing rather than claiming. A failed edge action does not make its Automation unhealthy.

## `reactor_action_duration_seconds`

**Type:** histogram &nbsp;·&nbsp; **Labels:** `type`

**Buckets:** `0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30`

Time one action attempt took, by type. Bounded above by the action's timeoutSeconds. For an edge action this covers every retry it was allowed.

## `reactor_provider_signal_disagreements_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `provider`, `signal`

Times two independent signals for the same fact disagreed. Nothing stops; the log line carries the detail, and a rising count is how a wrong derivation announces itself.

The moments a provider's independent signals fail
to agree. The signal label is a closed set the provider spells out, never
the values that disagreed: those come from the outside world.

## `reactor_reversal_disagreements_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** none

Times the Automations sharing one target declared different reversal levels for it, so they disagree about that workload's normal size. Reactor still takes the most restrictive; the Automation's status and its Event name both claimants and both levels.

The arbitration sibling of `reactor_provider_signal_disagreements_total` above:
two sources of one fact that do not agree, counted rather than resolved.
There the fact is what the network is doing; here it is what a workload's
normal size is, and the disagreeing sources are two Automations.

Unlabelled on purpose. Which target and which claimants is exactly the
unbounded half — a target reference and a claimant are not labels here —
and both are already in status.targets[].reversalDisagreement and in the
Event, attached to the objects they describe.

## `reactor_stale_decisions_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `provider`

Reconciles that acted on provider state older than the age this install allows. Reactor deliberately keeps acting — going blind must not release a claim mid-incident — so this counts decisions taken against state nothing has confirmed since.

The other half of `reactor_last_observation_timestamp_seconds`, and the half that is
attributable. The gauge says Reactor has gone blind; this says automations
were still deciding while it was, which is the part that reaches a
workload. It is published only by an install that set a bound, so on every
other install it is absent rather than zero.

## `reactor_reaction_latency_seconds`

**Type:** histogram &nbsp;·&nbsp; **Labels:** `provider`

**Buckets:** `0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120`

Time from the observation that changed a condition to the action it caused completing.

The metric that would have caught the v0.3.0 latency
bug the week it shipped, instead of by hand-reading log timestamps.

## `reactor_webhook_deliveries_total`

**Type:** counter &nbsp;·&nbsp; **Labels:** `result`

Webhook fast-path deliveries, by outcome. Losing these costs reaction latency and nothing else.
