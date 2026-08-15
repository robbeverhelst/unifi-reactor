---
title: "Shut down Kubernetes when the UPS goes on battery"
description: "A power cut buys you minutes. Three Automations that shed load the moment a UniFi UPS goes on battery, escalate as its remaining runtime drops, and close a node before it runs out."
---

The power goes at 02:40. The UPS carries the rack for eleven minutes. Nothing in
the cluster knows, so all of it keeps running at full draw until the battery
ends and every node loses power mid-write.

Reactor sees the UPS because your UniFi console already does. This guide builds
the escalation ladder: shed the cheap load immediately, close a node when the
runtime gets short, and stop the things that need a clean stop when it gets
critical.

## What this assumes

- Reactor is installed and `Ready` — [Install](/start/install/).
- **A UniFi UPS is adopted by the same console Reactor polls.** The `ups`,
  `ups.battery`, `ups.runtime` and `ups.load` keys do not exist otherwise. An
  Automation matching a key nothing publishes holds its last known matching —
  `false`, for one you just created — and reports `Ready=False` with
  `StateKeyUnavailable` naming the key, so it fails visibly rather than
  silently. A third-party UPS over NUT is a different provider and is not this
  one.
- Your workloads are `Deployment`s or `StatefulSet`s in a namespace Reactor can
  reach. Cross-namespace targets need `rbac.clusterWide` (the default).

Check the keys are actually there before writing anything:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'key=ups'
# INFO state transition provider=unifi key=ups from= to=online
# INFO state transition provider=unifi key=ups.runtime from= to=ample
```

## 1. Shed the cheap load the moment mains goes

The first rung costs nothing to get wrong: workloads that can disappear for the
length of an outage without anyone minding.

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: shed-load-on-battery
  namespace: media
spec:
  when:
    provider: unifi
    state:
      ups: on-battery

  actions:
    - type: kubernetes.scale
      target: { kind: Deployment, name: qbittorrent }
      replicas: 0
    - type: kubernetes.scale
      target: { kind: Deployment, name: plex }
      replicas: 0
```

There is no `onExit`, and that is the point. With it omitted the reversal policy
is `Baseline`: Reactor records what each workload was set to before it first
claimed it — in the annotation
`reactor.robbeverhelst.com/baseline-replicas`, on the workload itself — and
restores exactly that when mains comes back. You do not have to remember that
Plex runs at 1 and something else runs at 3, and the annotation outlives both
the Automation and Reactor. See
[Reversal and baselines](/concepts/reversal-and-baselines/).

`ups` is debounced at 1 sample, so this fires on the first observation that says
`on-battery` — within one `pollInterval`, 30s by default. A UPS switching to
battery is a switch position, not a measurement, and it does not flap.

## 2. Escalate on remaining runtime, not on charge

`ups.battery` is the obvious escalation key and the wrong one. It reports charge
against a threshold, and charge ignores load: 30% at 300W and 30% at 900W are
the same value and completely different situations. `ups.runtime` is the UPS's
own estimate of how long it can carry **what is plugged into it right now**,
bucketed to `ample` / `short` / `critical` against thresholds you set.

That is what a shutdown should match on, and it gets better as the ladder works:
every workload rung 1 scaled down raises the runtime the UPS reports for the
rest.

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: stop-database-on-critical-runtime
  namespace: data
spec:
  when:
    provider: unifi
    state:
      ups: on-battery
      ups.runtime: critical

  actions:
    - type: kubernetes.scale
      target: { kind: StatefulSet, name: postgres }
      replicas: 0

  onExit:
    - type: kubernetes.scale
      target: { kind: StatefulSet, name: postgres }
      replicas: 1
```

Both keys must match — every entry in a `state` block is an AND. Here `onExit`
is written out rather than left to the baseline, because a database is the one
workload where "whatever it was before" is worth stating explicitly instead of
inferring.

**Where the thresholds come from**, and why they are set together rather than
one at a time:

| Value | Default | What it does |
| --- | --- | --- |
| `unifi.ups.shortRuntimeSeconds` | `600` | at or below this, `ups.runtime` is `short` |
| `unifi.ups.criticalRuntimeSeconds` | `180` | at or below this, `ups.runtime` is `critical` |
| `unifi.pollInterval` | `30s` | how often the console is polled |
| `unifi.debounce.keys[ups.runtime]` | `2` | consecutive samples before a change is believed |

Two samples at a 30s poll is 60 seconds between the UPS saying `critical` and
Reactor believing it. Against the default 180s threshold that leaves two minutes
to get the database down. Lower `criticalRuntimeSeconds` and that headroom goes
with it. Every value is in the [chart values reference](/reference/values/).

### Why two Automations rather than one escalating key

Because a single enum that dropped from `on-battery` to `low-battery` would
**leave** the matching state, and leaving a matching state runs the reversal —
scaling your workloads back **up** in the middle of a power failure, at the
worst possible moment.

So `ups`, `ups.battery`, `ups.runtime` and `ups.load` are four independent axes,
and escalation is expressed by matching more of them rather than by one key
moving. The first Automation stays matched for the whole outage while the second
comes and goes underneath it. [Power and UPS state keys](/state-keys/power-and-ups/)
has the full argument.

If two rungs ever claim the **same** workload, that is fine and needs no
coordination: a shared target sits at the most restrictive level any Automation
asked for and comes back only when none of them want it down. See
[Arbitration](/concepts/arbitration/).

## 3. Close a node before the battery ends

The endgame of a power cut is a graceful shutdown, not a hard cut. Cordoning a
worker on the failing UPS stops new pods landing there, so replacements come up
on a node that is still on mains:

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: cordon-worker-on-short-runtime
  namespace: reactor-system
spec:
  when:
    provider: unifi
    state:
      ups: on-battery
      ups.runtime: short

  actions:
    - type: kubernetes.cordon
      target: { kind: Node, name: worker-03 }
```

A `Node` is cluster-scoped, so the target takes **no** `namespace` — the CRD
rejects one at admission rather than looking somewhere that cannot succeed.

Three things to know before adding this rung:

- **It is only worth anything if some nodes are not on the failing UPS.** In a
  homelab where the whole rack is on one UPS, cordoning buys nothing: there is
  nowhere else for a pod to go.
- **It needs a permission Reactor does not otherwise ask for.**
  `--set rbac.allowNodeActions=true` creates a `ClusterRole` even in a
  namespace-scoped install, and grants `get` and `patch` on nodes — and
  Kubernetes cannot narrow `patch` to one field. Without it, the Automation
  reports the node as unreachable and names the value to set. The plain manifest
  bundle does not offer it at all.
- **Cordoning moves nothing that is already running.** There is deliberately no
  `kubernetes.drain`: an eviction cannot be un-evicted, so it has no level to
  arbitrate and no reversal to declare, and in a three-node cluster the evicted
  pods go `Pending` rather than anywhere useful. [Why there is no drain](/actions/kubernetes/#why-there-is-no-kubernetesdrain).

Omitting `onExit` here restores the baseline, which includes **leaving a node
cordoned that you had already cordoned by hand** — recorded in
`reactor.robbeverhelst.com/baseline-unschedulable` before Reactor touched it.

## What you will see when the power goes

```sh
kubectl -n media get automation
# NAME                   PROVIDER   MATCHING   SUSPENDED   READY   AGE
# shed-load-on-battery   unifi      true       false       True    6d
```

```sh
kubectl -n media describe automation shed-load-on-battery
```
```text
Type    Reason        Age   From        Message
----    ------        ----  ----        -------
Normal  StateEntered  42s   automation  ups moved from "online" to "on-battery", so the condition started holding
Normal  TargetHeld    42s   automation  Deployment/media/qbittorrent held at 0 replicas
Normal  TargetHeld    42s   automation  Deployment/media/plex held at 0 replicas
```

The workload itself says who is holding it down and what it was:

```sh
kubectl -n media get deploy qbittorrent -o jsonpath='{.metadata.annotations}'
# {"reactor.robbeverhelst.com/baseline-replicas":"1",
#  "reactor.robbeverhelst.com/claimed-by":"media/shed-load-on-battery",
#  "reactor.robbeverhelst.com/claimed-at":"2026-08-15T02:40:11Z"}
```

And when mains returns:

```text
Normal  StateExited     8s   automation  ups moved from "on-battery" to "online", so the condition stopped holding
Normal  TargetReleased  8s   automation  Deployment/media/qbittorrent released; no automation claims it any more
```

`TargetHeld` and `TargetReleased` are raised only when a write actually
happened. A target already at the right value produces nothing, so a long outage
is three Events and not one every fifteen seconds.

### If the console goes down with the power

Likely, if your gateway is on the same UPS. Reactor holds the last known state
and **keeps acting on it** — it does not treat losing sight of the UPS as the
outage ending, because that would scale everything back up in the middle of one.
You get `Ready=False` with `StateKeyUnavailable` (a key vanished) or
`ObservationStale` (the console stopped answering entirely, on an install that
set `unifi.maxObservationAge`), and the claims stay exactly where they were.
[When Reactor cannot see](/concepts/when-reactor-cannot-see/).

## What this does not cover

- **It does not shut down machines.** Nothing here powers off a node or a NAS.
  Cordoning closes a node to new work; the shutdown itself is still yours, and a
  `notification.*` action telling you to run it is the honest shape — see
  [Get told when the WAN fails over](/guides/get-notified-when-the-wan-fails-over/)
  for the setup, which is identical for a UPS message.
- **It does not stop a running Job.** Suspending a CronJob is a separate rung
  and stops only *new* Jobs — [Suspend scheduled work during an outage](/guides/suspend-cronjobs-during-an-outage/).
- **It does not switch a UPS outlet.** Reactor reads `outlet.<n>` and never
  writes one, so "cut power to outlet 3" is not part of any plan you can write
  today. (Toggling a single outlet by hand on a UniFi UPS 2U moved only that
  outlet, and `relay_group` turns out to separate battery-backed outlets from
  surge-only ones rather than naming a switching bank — but no write action has
  shipped, and [#23](https://github.com/robbeverhelst/unifi-reactor/issues/23)
  is where that is decided.)
- **It cannot help a workload an HPA drives.** Reactor declines those rather
  than fighting — [Run alongside a HorizontalPodAutoscaler](/guides/run-alongside-an-hpa/).

## Before you trust it with a shutdown

`ups.runtime` is derived from the UPS's `timeToRemain` field, and **its unit is
inferred to be seconds** from a single observation on a UPS that was not
discharging. Nothing in Reactor depended on it before this key existed. The
`ups` key itself is verified against a real capture; the runtime *thresholds*
are not verified against a real discharge.

So: put the ladder in with [`spec.dryRun`](/operations/suspend-and-dry-run/)
first, pull the mains lead, and watch what `ups.runtime` actually reports as the
battery drains against a clock.
[#7](https://github.com/robbeverhelst/unifi-reactor/issues/7) has the procedure,
and confirming it is fifteen minutes that would settle the key for everybody.

## Where to go next

- [Power and UPS state keys](/state-keys/power-and-ups/) — the four keys and why they are four.
- [Kubernetes actions](/actions/kubernetes/) — cordon, suspend, restart, and the drain that is deliberately missing.
- [Arbitration](/concepts/arbitration/) — what happens when two rungs claim one workload.
- [Automation API reference](/reference/automation/) — every field of `spec`, generated from the types.
- [Chart values reference](/reference/values/) — every threshold, debounce and RBAC switch named above.
