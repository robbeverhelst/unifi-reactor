---
title: "Arbitration: when two Automations share a target"
description: "A shared workload sits at the most restrictive level any Automation asked for, and comes back only when none of them want it down — plus what happens when an HPA owns the same field."
---

## When two automations share a workload

qBittorrent genuinely should pause for *both* a metered uplink and a power cut. Point both automations at it and nothing has to be coordinated by hand:

```sh
kubectl -n media get automation
# NAME                    PROVIDER   MATCHING   SUSPENDED   READY   AGE
# pause-on-backup-wan     unifi      false      false       True    3h
# shed-on-battery         unifi      true       false       True    3h
```

While *any* automation's condition holds, the workload stays at the **most restrictive** level asked for. The WAN recovering above does not bring qBittorrent back, because the UPS automation still wants it down — and the automation that lost says so plainly:

```sh
kubectl -n media get automation pause-on-backup-wan -o jsonpath='{.status.targets[0]}'
# {"ref":"Deployment/media/qbittorrent","desired":1,"effective":0,
#  "deferredBy":["media/shed-on-battery"]}
```

The workload comes back only once **no** automation wants it down.

## When an action fails

Each action is bounded by `timeoutSeconds` (default 30), so a target that has stopped answering fails and is retried rather than occupying the reconciler. Retries back off exponentially from 2s to a 1-minute cap and stop after five consecutive failures — at which point the automation says so and waits for the next state change instead of retrying forever:

```sh
kubectl -n media get automation pause-on-backup-wan -o jsonpath='{.status.conditions[?(@.type=="Applied")]}'
# {"type":"Applied","status":"False","reason":"RetryBudgetExhausted",
#  "message":"giving up after 5 attempts, will try again on the next state change: ..."}
```

`Ready` tells you whether an automation is healthy; `Applied` tells you whether what it wants is what its targets have. An automation that is outvoted by a more restrictive claim is `Ready=True, Applied=False` — working exactly as intended.

## When something else already owns the workload

Arbitration works because Reactor can see every claimant. A HorizontalPodAutoscaler writes the same `spec.replicas` and is not an automation, so there is nothing to fold it into: Reactor scales to 0 to shed load, the HPA computes a count from metrics and scales it back, and fifteen seconds later Reactor scales it to 0 again. Neither is wrong; they both believe they own the field.

`safety.detectHPA: true` makes Reactor look before it claims, and decline:

```sh
kubectl -n media get automation shed-on-battery -o jsonpath='{.status.targets[0]}'
# {"ref":"Deployment/media/api","desired":0,
#  "managedBy":"HorizontalPodAutoscaler/media/api-hpa"}

kubectl -n media describe automation shed-on-battery | tail -2
# Warning  TargetManagedByHPA  not claiming Deployment/media/api is driven by
#                              HorizontalPodAutoscaler/media/api-hpa: arbitration cannot resolve
#                              a claimant it cannot see, and writing anyway would oscillate rather than win
```

Nothing is written to that target — not the replica count, and not the baseline annotation, because a baseline captured from a value the HPA is actively changing would mean nothing when a later reversal restored it. The automation stays `Ready=True`: it is correctly configured, it simply cannot act there. Its other targets are unaffected.

**A workload Reactor is already holding is handed back** when an HPA appears over it, to the baseline, and then let go. That case is the one worth getting right: an HPA will not scale a workload up from zero, so going quiet while holding it at 0 would leave it there with neither controller willing to move it.

**There is deliberately no `force`.** Overriding would mean writing `spec.replicas` harder, which is the oscillation, not a way out of it. The thing that would actually work is suspending the HPA — patching its `minReplicas`/`maxReplicas` — and that needs *write* access to somebody's autoscaling policy, which is a much larger permission and a separate decision. If you genuinely want Reactor to win during an outage, remove or suspend the HPA, or point the automation at something else: `kubernetes.cronjob.suspend` and `kubernetes.cordon` shed real load and nothing autoscales them.

Detection is **off by default**, because turning it on changes what an install already in that fight does and costs a permission — `list` on `autoscaling/horizontalpodautoscalers`, granted only when the value is set. That is a read of an autoscaling *policy*: Reactor gets no write to an HPA and nothing over the workloads one manages. With detection off the behaviour is unchanged, which is to say Reactor writes and is written over.

And an honest limit: the general problem is not solvable by detection. KEDA, a GitOps controller correcting drift, and a cron job running `kubectl scale` own `spec.replicas` just as hard, and none of them is discoverable through a stable API. An HPA is the common case and the one that can be seen. An empty `managedBy` means nothing was found, not that the field is uncontested.

### Turning HPA detection on

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set safety.detectHPA=true
```

With this on, Reactor lists the HPAs in a target's namespace before claiming it.
If one names the target it writes **nothing** — not the replica count, and not
the baseline annotation, because a baseline captured from a value the HPA is
actively changing would restore a meaningless number later. `status.targets[]`
gains `managedBy: HorizontalPodAutoscaler/<ns>/<name>`, a Warning Event says so,
`reactor_arbitrations_total{outcome="declined"}` counts it, and the Automation
stays `Ready=True` — it is correctly configured, it just cannot act there. Its
other targets are unaffected.

A workload Reactor was **already** holding is handed back to its baseline when
an HPA appears over it, and then let go. An HPA does not scale a workload up
from zero, so going quiet while holding it at 0 would strand it.

**The RBAC this implies.** `list` on `autoscaling/horizontalpodautoscalers`, in
whatever scope the manager already has — rendered only when this value is set.
Stated plainly, it lets the operator read every HorizontalPodAutoscaler it can
see, including their metric thresholds and replica bounds, which are policy
rather than payload. It grants **no write** to an HPA, so Reactor cannot suspend
one to win, and nothing over the workloads an HPA manages beyond what the target
rules already allow. No `get` (a namespace is listed, not a name looked up) and
no `watch` (HPAs are read uncached, so nothing starts an informer). If the
permission is missing while detection is on, a claim **fails** rather than
proceeding blind, and the error names the fix.

Off by default because turning it on changes what an install already in that
fight does, and because it costs a permission nothing else here needs. There is
no Automation it makes worse, so turning it on is the recommendation.
