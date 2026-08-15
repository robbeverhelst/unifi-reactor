---
title: "Scaling down a Deployment that has a HorizontalPodAutoscaler"
description: "Reactor scales to 0 to shed load, the HPA scales it back from metrics, and the Deployment oscillates forever. What Reactor does about it, and what to write instead."
---

You add a power-cut Automation that scales `api` to zero. `api` has a
HorizontalPodAutoscaler. Fifteen seconds later it is back at three replicas, and
fifteen seconds after that it is at zero again, and this continues until one of
you is removed.

Nobody is malfunctioning. Both controllers write `spec.replicas` through the same
`/scale` subresource, both believe they own it, and neither has any way to see
the other. This guide is what Reactor does about that and what to write instead.

## Why it cannot be arbitrated away

Reactor's answer to two Automations wanting the same workload is
[arbitration](/concepts/arbitration/): every claim on a target is folded to the
most restrictive one, so two Automations pausing a workload for unrelated reasons
resolve to a single claim. That works because Reactor can see every claimant —
they are all `Automation` objects it already lists.

An HPA is not one. There is nothing to fold it into, so writing anyway is a fight
rather than a resolution, and it is a fight that got *louder* in v1.0: claims are
re-asserted on every reconcile rather than once per transition. A reconcile
happens at least every 15 seconds. Same disagreement, forever, on a timer.

## Turn detection on

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set safety.detectHPA=true
```

It is **off by default** — it changes what an install already in that fight does,
and it costs a permission nothing else needs — but there is no Automation it
makes worse, so turning it on is the recommendation.

With it on, Reactor lists the HorizontalPodAutoscalers in a target's namespace
before claiming it. If one names the target, it writes **nothing**: not the
replica count, and not the baseline annotation either, because a baseline
captured from a value the HPA is actively changing would restore a meaningless
number later.

**The permission this grants** is `list` on
`autoscaling/horizontalpodautoscalers`, in whatever scope the manager already
has, and it is rendered only when the value is set. That is a read of an
autoscaling *policy* — including metric thresholds and replica bounds. It grants
no write to an HPA, so Reactor cannot suspend one to win, and nothing over the
workloads an HPA manages. There is no `get` (a namespace is listed, not a name
looked up) and no `watch` (HPAs are read uncached, so nothing starts an
informer).

If the permission is missing while detection is on, a claim **fails** rather than
proceeding blind, and the error names the fix.

## What a declined target looks like

Take the Automation that started this:

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
      target: { kind: Deployment, name: api }       # an HPA drives this one
      replicas: 0
    - type: kubernetes.scale
      target: { kind: Deployment, name: qbittorrent }
      replicas: 0
```

```sh
kubectl -n media get automation shed-load-on-battery -o jsonpath='{.status.targets[0]}'
# {"ref":"Deployment/media/api","desired":0,
#  "managedBy":"HorizontalPodAutoscaler/media/api-hpa"}
```

```sh
kubectl -n media describe automation shed-load-on-battery
```
```text
Type     Reason              Age   From        Message
----     ------              ----  ----        -------
Normal   StateEntered        20s   automation  ups moved from "online" to "on-battery", so the condition started holding
Warning  TargetManagedByHPA  20s   automation  not claiming Deployment/media/api is driven by HorizontalPodAutoscaler/media/api-hpa: arbitration cannot resolve a claimant it cannot see, and writing anyway would oscillate rather than win
Normal   TargetHeld          20s   automation  Deployment/media/qbittorrent held at 0 replicas
```

Four things to read out of that:

- **`Ready` stays `True`.** The Automation is correctly configured; it simply
  cannot act on that one target. `Applied` is `False` with reason
  `TargetManagedByHPA`.
- **Its other targets are unaffected.** `qbittorrent` was claimed normally in
  the same reconcile.
- **It is a `Warning`, unlike being outvoted by a peer.** Losing an arbitration
  is the design working and is reported `Normal`. Being unable to arbitrate at
  all is an Automation that cannot do its job, and no amount of waiting fixes
  it — somebody has to decide which controller owns the workload.
- **It is counted**, as `reactor_arbitrations_total{outcome="declined"}`, so it
  is alertable rather than only discoverable by describing a resource.

### A target Reactor was already holding

If the HPA appears *over* a workload Reactor is already holding at zero, going
quiet would strand it: an HPA does not scale a workload up from zero, so neither
controller would ever move it. So Reactor hands it back to its recorded baseline
first, and then lets go:

```text
Warning  TargetManagedByHPA  3s  automation  Deployment/media/api is now driven by HorizontalPodAutoscaler/media/api-hpa; handed it back at 3 replicas and stopped claiming it
```

## What to do instead

Detection stops the oscillation. It does not shed the load you wanted shed, and
nothing Reactor could do would — so pick one of these deliberately.

**1. Shed load somewhere nothing autoscales.** Usually the best answer, because
the workloads with an HPA are typically the ones that should survive an outage
anyway. Nothing autoscales a CronJob or a Node:

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
    - type: kubernetes.cronjob.suspend
      target: { kind: CronJob, name: nightly-reindex }
    - type: kubernetes.scale
      target: { kind: Deployment, name: qbittorrent }
      replicas: 0
    - type: notification.ntfy
      notification:
        secretRef: { name: ntfy-credentials }
        title: "On battery"
        message: "{{ .Key }} is {{ .To }}. api is HPA-driven and was not touched — scale it by hand if this lasts."
```

That is a complete, working replacement: a
[suspended CronJob](/guides/suspend-cronjobs-during-an-outage/), a scaled
workload nothing else claims, and a
[notification](/guides/get-notified-when-the-wan-fails-over/) telling a human
about the part Reactor deliberately would not do. The notification needs
`actions.allowedDestinations` and a Secret first — drop that action if you have
not set those up.

**2. Let the HPA do it.** During a real outage the traffic that justified those
replicas usually goes away too, and the HPA will scale down on its own metrics.
That is not shedding on *your* schedule, but it is not nothing.

**3. Remove or suspend the HPA for the workloads that must shed.** Reactor will
not do this for you at any setting. Suspending an HPA means patching somebody's
`minReplicas`/`maxReplicas`, which needs **write** access to an autoscaling
policy — a much larger permission and a separate decision, taken by a person
rather than by an operator during an incident.

**4. Cordon instead of scaling.** [`kubernetes.cordon`](/actions/kubernetes/#closing-a-node-to-new-work)
closes a node to new work, and no autoscaler contests `spec.unschedulable`. It
needs `rbac.allowNodeActions`, and it moves nothing already running.

## There is deliberately no `force`

Overriding would mean writing `spec.replicas` harder, which *is* the
oscillation — not a way out of it. The only thing that actually resolves the
disagreement is deciding which controller owns the field, and that decision is
not Reactor's to take.

## What detection does not cover

- **Only HorizontalPodAutoscalers.** KEDA, a GitOps controller correcting drift,
  and a cron job running `kubectl scale` own `spec.replicas` just as hard, and
  none of them is discoverable through a stable API. **An empty `managedBy` means
  nothing was found, not that the field is uncontested.**
- **Only scalable kinds.** A CronJob's `spec.suspend` and a Node's
  `spec.unschedulable` are not fields an HPA writes, so detection does not apply
  to them and does not need to.
- **The HPA's version is not compared, its group is.** A `scaleTargetRef` written
  years ago against `apps/v1beta2` still drives the same Deployment today, and
  reading a version mismatch as "not managed" would put Reactor straight back in
  the fight.
- **It does not suspend, pause or edit the HPA.** No write permission exists for
  that, under any setting.
- **It does not tell the HPA anything.** The two controllers remain unaware of
  each other; one of them has simply stopped writing.

## Where to go next

- [Arbitration](/concepts/arbitration/) — the model this is the exception to.
- [Kubernetes actions](/actions/kubernetes/) — the actions nothing else contests.
- [Events and condition reasons](/reference/events/) — every reason `Ready` and `Applied` can carry.
- [Automation API reference](/reference/automation/) — `status.targets[].managedBy` and the rest of status.
- [Chart values reference](/reference/values/) — `safety.detectHPA` and the RBAC it renders.
