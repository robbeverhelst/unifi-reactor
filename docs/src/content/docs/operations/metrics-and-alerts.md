---
title: "Metrics, alerts and the dashboard"
description: "Reactor fails silently if it stops observing. One metric answers that, and the shipped PrometheusRule and Grafana dashboard are built around it. Plus the cardinality rules behind every label."
---

## Knowing it is working

Reactor's worst failure is **silent and fails open**. If it stops observing — an expired API key, a rebooted console, a network partition — every automation quietly stops reacting. Nothing in the cluster notices, and the next real outage simply does not get handled. There is no error to find, because nothing errored.

One metric answers that, and it is the reason the rest exist:

```promql
time() - reactor_last_observation_timestamp_seconds
```

Metrics are **off by default** and register on the endpoint controller-runtime already serves — there is no second server and no second auth posture:

```sh
helm upgrade reactor ... \
  --set metrics.enabled=true \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.rules.enabled=true \
  --set metrics.dashboard.enabled=true
```

| Metric | Type | Answers |
| --- | --- | --- |
| `reactor_last_observation_timestamp_seconds` | gauge | is Reactor still seeing anything |
| `reactor_observations_total` | counter | how often polling succeeds and fails |
| `reactor_state_info` | gauge 0/1 | what each state key holds right now |
| `reactor_state_transitions_total` | counter | how often failover actually happens |
| `reactor_automation_matching` / `_ready` | gauge 0/1 | which policies are in force, and which are broken |
| `reactor_arbitrations_total` | counter | claimed, deferred to a peer, or released |
| `reactor_actions_total` | counter | action outcomes, by type and **kind** |
| `reactor_action_duration_seconds` | histogram | slow or hanging actions |
| `reactor_reaction_latency_seconds` | histogram | observation → action, end to end |
| `reactor_webhook_deliveries_total` | counter | fast-path deliveries accepted, coalesced, refused |
| `reactor_provider_signal_disagreements_total` | counter | two independent signals for one fact disagreeing |

Reconcile counts, queue depth and reconcile latency are controller-runtime's own `controller_runtime_*` series on the same endpoint. Reactor does not reimplement them. It also deliberately **does not re-export UniFi telemetry** — a UniFi exporter covers that better, and Reactor's unique vantage point is the decision layer.

`kind` on `reactor_actions_total` is `desired_state` or `edge`, and no alert should be written without it. A failed `kubernetes.scale` means the cluster is not in the state you asked for. A failed notification means nobody was told — [the workload was still scaled](/actions/notifications-and-http/#when-a-notification-fails), and the automation is still `Ready`. The shipped rules alert on those separately for exactly that reason.

### Cardinality, on purpose

`isp` is the first state key whose values are an **open set** — a carrier slug derived from whatever public address your gateway currently holds. So `reactor_state_info` is published only for keys whose provider declares a closed value set, and `isp` is deliberately not one of them. The transition counter is not labelled by `from`/`to` for the same reason. What a key currently holds is always in `status.observedState` and in an `Event`; what Prometheus keeps is bounded at compile time.

Declaring the vocabulary is also what lets the gauge report `0` for the values a key does *not* hold. Without that, the series for a value it used to hold goes stale at `1` rather than dropping, and every graph built on it lies. All values `0` means the key is not currently observable — the metric side of [`StateKeyUnavailable`](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state).

`device.<name>` is the other side of the same coin, and the reason [it is opt-in](/state-keys/fleet-and-devices/#the-fleet-devices-and-why-devicename-is-opt-in). Its *values* are closed — two of them — but its **key name** comes from your network, so the set of keys is open. It is therefore never in `reactor_state_info` at all, and turning the keys on adds one `reactor_state_transitions_total` series per adopted device: a bounded number, chosen by you, rather than one this repository can promise. `devices` is one series regardless of fleet size, which is why it is the one that ships on.

`outlet.<n>` is out of `reactor_state_info` too, and it is worth saying why it is **not** opt-in as well, because the two halves of `device.<name>`'s argument come apart here.

The cardinality half does not carry over. Eight outlets are bounded by a chassis, not by a rack: nobody adds outlets to a UPS, and most installs have no outlet-bearing device at all. So these keys ship on, beside the other UPS keys, rather than being the one UPS key you have to ask for.

The enumerability half does, and it is what keeps them out of the gauge. `StateVocabulary` is handed over once at startup, before any console has been polled, so it cannot contain a key whose name is an outlet somebody has not named yet — and hardcoding `outlet.1` … `outlet.8` would write one UPS's chassis into this repository and silently leave a larger PDU's ninth outlet outside the metric. Declaring `outlet.*` was considered and rejected for a sharper reason: the gauge reports `0` for the values a key does **not** hold, which requires enumerating the values of a key that is missing from the observation, and a prefix can only match keys that are present. An outlet key that vanished with its UPS would sit at `1` forever — the exact staleness declaring a vocabulary exists to prevent.

They are still counted in `reactor_state_transitions_total` (one series each), they are in `status.observedState` and in Events, and the provider logs every change with its relay group in words. That last one is what the [relay-group experiment](/state-keys/outlets/) actually reads.

### Alerts and the dashboard

`metrics.rules.enabled` ships a `PrometheusRule` — `ReactorObservationStale` first, then failing observations, failing actions, edge actions failing separately, automations stuck not-ready, and reactions getting slow. `ReactorUPSOnBattery` and `ReactorWANOnBackup` are informational: they let your existing alerting learn what your network already knows.

`metrics.dashboard.enabled` ships a grafana-operator `GrafanaDashboard`. It pins no datasource — you pick one from a variable when you open it — so the same JSON works in any Grafana, and it is a plain file at [`charts/reactor/dashboards/reactor.json`](https://github.com/robbeverhelst/unifi-reactor/blob/main/charts/reactor/dashboards/reactor.json) if you would rather import it by hand.

Both need their operator's CRDs, and both refuse to render without `metrics.enabled` rather than quietly querying series nothing is publishing.

## The auth posture, and the alerts that ship

The endpoint serves **HTTPS on `:8443` behind the API server's authn/authz
filter**, which is what the kubebuilder scaffold has always done and what
`install.yaml` ships. A scraper must present a bearer token belonging to a
ServiceAccount that is allowed to `get` the `/metrics` non-resource URL.

Enabling metrics therefore grants the operator `create` on `tokenreviews` and
`subjectaccessreviews` — that is how it asks the API server to authenticate and
authorize each scrape. Both are cluster-scoped, so those rules are a ClusterRole
in both `rbac.clusterWide` modes.

Both reviews are cluster-scoped resources with no namespaced form, so those
rules are a **ClusterRole even under `rbac.clusterWide: false`** — the one place
a namespace-scoped install still creates cluster-scoped RBAC. With metrics off,
or with `metrics.secure: false`, a namespace-scoped install creates nothing
cluster-scoped at all. What is granted is delegation — the right to ask the API
server whether a caller is allowed in — not access to anything that caller
holds.

The chart creates a `<release>-metrics-reader` ClusterRole and **deliberately
does not bind it**. It cannot know which ServiceAccount your Prometheus scrapes
with, and a binding to the wrong one looks granted and is not:

```sh
kubectl create clusterrolebinding reactor-metrics-reader \
  --clusterrole=reactor-metrics-reader \
  --serviceaccount=monitoring:prometheus-k8s
```

The controller generates a self-signed certificate for the endpoint unless
`--metrics-cert-path` points at a real one, so the ServiceMonitor ships with
`insecureSkipVerify: true`. Issue a certificate and set
`metrics.serviceMonitor.serverName` to turn verification on. Setting
`metrics.secure: false` serves plain HTTP instead, which only makes sense when
something else already controls who can reach the pod.

`metrics.serviceMonitor.labels` is optional on purpose: a Prometheus whose
`serviceMonitorSelector` is `{}` scrapes every ServiceMonitor and needs none of
them.

### Alert rules

`metrics.rules.enabled` ships a `PrometheusRule` (needs the Prometheus Operator
CRD). `metrics.rules.observationStaleSeconds` defaults to `90`, three times the
default `unifi.pollInterval` — **raise it if you raise that**, or a slower poll
will page you.

| Alert | Severity | Means |
| --- | --- | --- |
| `ReactorObservationStale` | critical | Reactor has gone blind; every Automation has silently stopped reacting |
| `ReactorObservationAbsent` | critical | it is publishing no observations at all, or is not running |
| `ReactorObservationFailing` | warning | sustained poll errors — the warning before the two above |
| `ReactorActionFailing` | warning | an Automation matched but could not act on its target |
| `ReactorEdgeActionFailing` | warning | a notification or HTTP call did not go out; the Automation is unaffected |
| `ReactorAutomationNotReady` | warning | invalid config, a missing state key, or a failed action |
| `ReactorReactionSlow` | warning | p95 observation-to-action above `metrics.rules.reactionLatencySeconds` |
| `ReactorUPSOnBattery` | info | the UPS is running on battery |
| `ReactorWANOnBackup` | info | the gateway is on its backup uplink |

The last two fire on observed state rather than on a fault: they are how your
existing alerting learns what your network already knows. Set
`metrics.rules.informational: false` to leave them out.

### Dashboard

`metrics.dashboard.enabled` ships a grafana-operator `GrafanaDashboard`. It pins
no datasource — the dashboard carries a `datasource` variable you pick when you
open it — and no folder or tag specific to any organisation, so the same JSON
imports into any Grafana unedited. It is a plain file in the chart at
`dashboards/reactor.json` if you would rather import it by hand.

`metrics.dashboard.instanceSelector` decides which Grafana instances
grafana-operator installs it into; it defaults to
`matchLabels: {dashboards: grafana}`, the operator's own convention.

All three of `serviceMonitor`, `rules` and `dashboard` refuse to render without
`metrics.enabled`, rather than shipping something that queries series nothing is
publishing — which fails as silence rather than as an error.
