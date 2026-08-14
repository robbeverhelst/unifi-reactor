---
title: "Kubernetes actions"
description: "Scaling is only the start: suspend a CronJob so the nightly backup does not run, cordon a node on a dying battery, restart a wedged workload — and why there is no drain."
---

The first Automation in [your first Automation](/start/first-automation/) scales a Deployment. These are the rest of what Reactor can do to a Kubernetes object, and the reasoning behind the one it deliberately cannot.

## Stopping scheduled work

Scaling cannot express "do not start the nightly backup tonight" — that is `spec.suspend` on a CronJob, and it is the single highest-value thing to stop during an outage or on a metered uplink:

```yaml
  actions:
    - type: kubernetes.cronjob.suspend
      target: { kind: CronJob, name: velero-backup, namespace: velero }
```

`suspended` defaults to `true`, which is what the action is named after; write `suspended: false` in `onExit` to ask for it back explicitly, or omit `onExit` and Reactor restores whatever the CronJob was set to before it claimed it.

**Suspending stops new Jobs being created and does nothing to a Job already running.** That is deliberate, and Reactor is not granted any permission over Jobs at all, so it could not delete one if it wanted to: declining to start more work is a very different act from killing work in flight, and killing work in flight is not a decision an outage should make on your behalf. If a running backup is what you need stopped, stop it yourself.

## Closing a node to new work

The endgame of a power cut is a graceful shutdown, not a hard cut. Cordoning a worker running on a dying battery stops new pods landing there, so replacements come up on the node still on mains:

```yaml
  actions:
    - type: kubernetes.cordon
      target: { kind: Node, name: worker-03 }
```

Cordoning is desired-state, like scaling: `spec.unschedulable` is a level, `cordoned` wins over `schedulable` in a fold, and applying it twice is applying it once. `cordoned` defaults to `true`; write `cordoned: false` in `onExit` to reopen explicitly, or omit `onExit` and Reactor restores what it found — **including leaving a node cordoned that you had already cordoned by hand.**

> **It is opt-in, and it is the only permission Reactor asks for that reaches outside the workloads you installed it to manage.** Nodes are cluster-scoped, so `--set rbac.allowNodeActions=true` creates a `ClusterRole` *even in a namespace-scoped install*. It grants `get` and `patch` on nodes; Kubernetes cannot narrow `patch` to one field, so that also permits writing node labels and annotations. Decide whether that is worth it before turning it on. Without it, an automation using `kubernetes.cordon` reports the node as unreachable and names the value to set. The manifest bundle (`install.yaml`) does not offer it at all.

### Why there is no `kubernetes.drain`

Draining was proposed alongside cordoning and is **deliberately not implemented** — not deferred, not behind a flag. Four reasons, and the first is the one that decides it:

1. **An eviction cannot be un-evicted.** Every other action here declares a *level* that is a pure function of which conditions currently hold, which is what makes the outcome independent of ordering and a controller restart harmless. A drain has no such value: there is no state a node can be held at that means "drained", `onExit` cannot express undoing it, and a flapping key would empty the node again on every flap with nothing to correct it.
2. **In a small cluster it makes things worse.** Draining assumes somewhere else to go. In a three-node homelab on one UPS, the evicted pods do not reschedule — they go `Pending`, so you lose the workload *before* the battery runs out instead of when it does. Cordoning gets the actual benefit here without that cost.
3. **It can evict the operator.** If Reactor's own pod is on the node being drained, the action kills the thing performing and reporting it, mid-action. Nothing else Reactor does can do that.
4. **It hangs, by design.** Eviction respects PodDisruptionBudgets, and a single-replica workload with `minAvailable: 1` blocks forever. That is a bounded-timeout problem on paper and an unbounded blast-radius problem in practice.

So the RBAC that would make it possible is not granted under any setting: `rbac.allowNodeActions` gives access to `nodes` and nothing to `pods` or `pods/eviction`. If your outage plan genuinely needs a drain, `kubernetes.cordon` plus a `notification.*` telling you to run `kubectl drain` yourself is the honest shape — a human is the right thing to make an irreversible cluster-wide decision at 3am.

## Restarting a workload

The standard remedy for something wedged — a service that needs to re-resolve DNS or re-establish upstream connections once connectivity returns:

```yaml
  onExit:
    - type: kubernetes.restart
      target: { kind: Deployment, name: sonarr }
```

It stamps `kubectl.kubernetes.io/restartedAt` on the pod template, exactly as `kubectl rollout restart` does, so the workload controller rolls the pods under whatever update strategy and disruption budget the workload already declares. Reactor never deletes a pod.

Restart is an **edge** action: there is no value a workload can be held at that means "restarted", so it owns nothing, arbitrates with nothing, and fires on this automation's own transition. Put it in `onExit` when you want it on recovery, as above, and in `actions` when you want it on the way in.

> **It is at-most-once, and it has to be.** Every execution rolls the workload, so a retry after an ambiguous failure would be a second outage rather than a correction. Reactor attempts it exactly once per transition, records the outcome in `status.edgeActions`, and never tries again — the failures that actually happen here (a conflict, a `Forbidden`) are not ones a retry fixes. A restart that did not happen is reported as a `Warning` and leaves the automation `Ready`.

### Restart is why debounce matters

Everything else Reactor does is safe to repeat: scaling to 0 twice is one scale, suspending a suspended CronJob is nothing. **A restart is not.** The engine only acts on transitions, so a steady condition never restarts anything twice — but a *flapping* state key is a stream of transitions, and each one is a real rollout. A `wan` key oscillating every poll would roll the workload every poll.

The engine's answer is [debounce](/concepts/settling-a-noisy-signal/), and with `kubernetes.restart` it stops being an optimization:

```yaml
unifi:
  debounce:
    default: 1
    keys:
      wan: 3      # if wan drives a restart, make it prove itself first
```

The shipped default is `1` — react on the first observation — because `wan` and `ups` are switch positions that do not flap, and a failover deserves an immediate reaction. That default is chosen for `kubernetes.scale`. **If a key drives a restart, raise its debounce**, and accept the cost: each extra sample is one `pollInterval` of extra reaction time. Before adding a restart to an automation, ask what the key does when the hardware behind it is halfway broken rather than cleanly up or down — that is the state a restart loop is born in.

The same paragraph applies word for word to [`unifi.poe.cycle`](/actions/unifi-console/#power-cycling-a-poe-port), only more so: there the repeated act is a power cut to a physical device.
