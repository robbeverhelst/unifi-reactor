---
title: "Nothing happens when the state changes"
description: "The state moved and no workload did: the checklist for an Automation that is not matching, a Reactor that is running but not reacting, and one that is telling you it is in dry run."
---

## 1. Nothing happens when the state changes

Work down this list; it is ordered by likelihood.

**It is suspended.** The cheapest check, and it survives restarts and upgrades because it is spec, not state:

```sh
kubectl -n media get automation
# NAME                           PROVIDER   MATCHING   SUSPENDED   READY   AGE
# pause-downloads-on-backup-wan  unifi      false      true        True    3h
```

A suspended Automation observes and reports state but claims nothing, so its targets sit wherever the automations still in force put them — see [pausing an automation](/operations/suspend-and-dry-run/#pausing-an-automation). `kubectl patch automation <name> --type=merge -p '{"spec":{"suspend":false}}'` puts it back in force, and it re-claims on the next reconcile if its condition still holds.

**The provider is not enabled.** Without `unifi.url` set, the provider never starts and every state Automation sits at `ProviderStateUnavailable` forever. The startup log says so plainly:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'UniFi provider'
# INFO UniFi provider disabled (UNIFI_URL not set); state triggers will stay pending
```

Fix with `--set unifi.url=https://192.0.2.10` on `helm upgrade`.

**The console is not reachable, or the key is wrong.** See [§3](/troubleshooting/credentials-and-reachability/#3-credentials-and-reachability).

**Your condition does not match the observed vocabulary.** All keys in a `state` block must match, exactly, case-sensitively. Compare what you wrote against what the provider actually publishes:

```sh
kubectl -n media get automation pause-downloads-on-backup-wan -o jsonpath='{.spec.when.state}'
kubectl -n reactor-system logs deploy/reactor | grep 'state observed' | tail -1
```

The published keys and values are listed in the [README state-key table](/state-keys/). `wan: Backup`, `ups: battery`, and `ups.battery: LOW` all silently never match.

**It already matched.** Actions run on *transitions*. If `status.matching` was already `true` when you deployed the workload, nothing fires until the state leaves and re-enters. `status.lastTransition` tells you when it last flipped. To force a re-run during testing, flip the state at the source (`make dev-mock` exposes `POST /flip` and `POST /ups`) rather than editing the resource.

**You are inside the latency window.** Reaction time is bounded by the poll interval (`unifi.pollInterval`, default 30s). A transition wakes affected Automations immediately, but if that wake queue is saturated the Automation falls back to a periodic re-evaluation of ~15s. That falling-back is a debug line, so you only see it with `log.level=debug`:

```text
DEBUG wake queue full, leaving it to periodic re-evaluation automation=media/pause-downloads-on-backup-wan
```

Occasional lines are harmless by design — the wake is an optimization, the poll is the mechanism. Continuous ones mean the reconciler is not keeping up.

**Only the leader polls.** With `replicaCount > 1`, the non-leader replicas observe nothing; that is correct. Check the logs of the pod holding the lease, not an arbitrary one.

---

## 13. Reactor is running but nothing is reacting

The failure this whole page cannot help with: Reactor is up, healthy, logging
nothing unusual, and has silently stopped observing. Every Automation holds its
last known state, no error is raised, and the next real outage is simply not
handled. There is nothing to grep for, because nothing went wrong loudly.

One number answers it, and it is why `metrics.enabled` exists:

```promql
time() - reactor_last_observation_timestamp_seconds
```

Above three poll intervals, Reactor is blind. `ReactorObservationStale` is that
query with a threshold on it, and it is the alert to wire up before any other.

Without metrics enabled, the same question by hand — the last line is the
answer, and its timestamp is what matters:

```sh
kubectl -n reactor-system logs deploy/reactor --timestamps   | grep -E 'state (observed|transition)' | tail -1
```

`state observed` needs `log.level=debug`; `state transition` only appears when
something changed, so on a quiet network it can be hours old and still healthy.
That ambiguity is exactly what the metric removes.

### Turning the endpoint on

```sh
helm upgrade reactor ... --set metrics.enabled=true
```

It serves HTTPS on `:8443` behind the API server's authn/authz filter, so a
scrape needs a token. Check it by hand from inside the cluster:

```sh
kubectl -n reactor-system exec deploy/reactor --   wget -qO- --no-check-certificate https://127.0.0.1:8443/metrics
# 401 — expected: the endpoint authenticates every scrape, including this one
```

| What you see | Cause |
| --- | --- |
| `connection refused` | `metrics.enabled` is not set, so the binary is not listening at all |
| Prometheus reports the target as `down` with a 401 or 403 | its ServiceAccount is not bound to the `<release>-metrics-reader` ClusterRole. The chart creates that role and deliberately does not bind it |
| target `down` with a TLS error | the endpoint's certificate is self-signed unless you issue one; the shipped ServiceMonitor sets `insecureSkipVerify` |
| the target is not discovered at all | `metrics.serviceMonitor.enabled` is off, or your Prometheus selects on labels the ServiceMonitor does not carry — see `metrics.serviceMonitor.labels` |
| the target scrapes but there are no series | a `networkPolicy` with the default `ingress: []` denies the scrape; the chart does not widen it for you |

### A state key you expected has no series

`reactor_state_info` is published **only for keys whose value set is closed**.
`isp` is deliberately absent: its values are carrier names, an open set, and one
series per carrier ever seen is how a Prometheus instance gets hurt. Read `isp`
off the Automation's `status.observedState` or its Events instead.

A key that *is* declared but shows `0` for every value has not been observed —
no UPS adopted, or the hardware dropped off. That is the metric side of
[`StateKeyUnavailable`](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state), and it is a
different statement from "observed, and it is not that value".

### `reactor_provider_signal_disagreements_total` is climbing

Two independent signals for the same fact stopped agreeing. Nothing stops and
no state is withheld — see
[§10](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover) for what each `signal` label
means and why these are reported rather than resolved. The counter exists so a
wrong `wan` derivation announces itself on a graph instead of waiting for
somebody to read the logs during an outage.

---

## 14. An automation is not acting, and is telling you what it would do

`Ready=True` with reason `DryRun` is not a fault. Something asked this automation to describe itself rather than run, and there are two separate things that could have:

```sh
# Is it this automation?
kubectl -n media get automation shed-on-battery -o jsonpath='{.spec.dryRun}'

# Or the whole install?
kubectl -n reactor-system get deploy reactor \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -o '\-\-dry-run'
```

They report differently, and the difference tells you which one you are looking at:

| | `spec.dryRun` on the automation | `safety.dryRun` on the install |
| --- | --- | --- |
| Where the answer is | `status.targets[].preview` | `status.targets[].effective`, unwritten |
| `Applied` message | "a dry run claims no target" | "this install runs as a dry run" |
| Effect on peers | none — it is out of force, so it is arbitrated as if absent | none — everything is in force and nothing is written |
| Metrics | — | `reactor_arbitrations_total{outcome="withheld"}` is the only outcome published |

`increase(reactor_arbitrations_total{outcome="withheld"}[1h])` is the fleet-wide version of the same question: a live install publishes none of these, and an install that thinks it is live but is not publishes nothing else.

**Reading a preview.** `preview.effective` is what the target would be held at with this automation's claim folded in; `preview.deferredBy` is who would still outvote it, `preview.wouldDefer` is who it would outvote, and `preview.onExit` is what it would hand back when its condition ended. It is computed whether or not the condition currently holds, on purpose — the automation you most want to check is the one for an outage.

It is not a forecast. The peers, the observed state and the target can all change before the condition holds, and nothing in a fold can predict whether the write would be *accepted*: RBAC, an admission webhook, a deleted target, and [a HorizontalPodAutoscaler already driving the field](/troubleshooting/conflicts-and-drift/#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment) are all outside what it knows.

**Nothing was written, but the workload is still down.** Turning the install-wide dry run on does not release what Reactor was already holding, because releasing is a write too. Those workloads freeze where they are with their annotations intact — [§8](/troubleshooting/conflicts-and-drift/#8-a-workload-is-stuck-down-after-an-automation-was-deleted) is how to get them back, and the fix is to suspend or delete those automations before enabling `safety.dryRun`, not after.

---
