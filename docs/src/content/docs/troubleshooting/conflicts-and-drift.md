---
title: "Something else is fighting Reactor over a workload"
description: "Two Automations claiming one target, a workload stuck down after a deletion, GitOps reverting Reactor’s writes, and a HorizontalPodAutoscaler that wants the same replica count."
---

## 7. Two Automations fighting over one target

When more than one Automation names the same target, the desired value is resolved by a fold over every Automation claiming it, not by whichever reconciled last. For `kubernetes.scale` the fold is `min` — the most restrictive claim wins, so shedding beats restoring.

Two Automations, `power/shed-on-battery` and `net/pause-on-backup-wan`, both scaling `media/qbittorrent` to 0. Power returns while the WAN is still on backup: the first stops matching and wants 1, the second still matches and wants 0, and `min(1, 0) = 0`. The workload correctly stays down. It comes back when the last claim releases.

This is visible rather than mysterious — each Automation reports what it wanted and what it got:

```yaml
status:
  matching: false
  targets:
    - ref: media/qbittorrent
      desired: 1        # what this Automation alone wants
      effective: 0      # what the arbiter resolved
      deferredBy:
        - power/shed-on-battery
```

`deferredBy` names who outvoted you. And the target itself explains its own state:

```sh
kubectl -n media get deploy qbittorrent -o jsonpath='{.metadata.annotations}'
# reactor.robbeverhelst.com/baseline-replicas: "1"   # or baseline-suspend on a CronJob
# reactor.robbeverhelst.com/claimed-by: "power/shed-on-battery,net/pause-on-backup-wan"
# reactor.robbeverhelst.com/claimed-at: "..."
```

`baseline-replicas` is what Reactor found before it first claimed the target, and it is what a reversal restores. `claimed-by` and `claimed-at` are advisory — refreshed each reconcile, never read back as truth — and exist so `kubectl describe deploy` explains the zero to a human at 3am.

**A scale-*up* Automation loses to any scale-down claim on the same target**, because `min` encodes "most restrictive wins". `status.targets[].effective` makes it visible instead of silent. If you need the opposite, that is a design conversation, not a misconfiguration.

**None of this reaches a claimant that is not an Automation.** The fold is over what Reactor can see, so a HorizontalPodAutoscaler on the same Deployment is not resolved — it is fought, unless detection is on. That is [§15](#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment), and it looks quite different: a workload that flaps rather than one that settles on a value somebody else asked for.

> The fold, `status.targets[]`, and the target annotations land with the target-ownership change. On v0.3.0 the outcome of two Automations on one target depends on reconcile order.

---

## 8. A workload is stuck down after an Automation was deleted

The worst failure mode in the system, because the cause and the symptom are a week apart. Reactor scaled something to 0, the Automation was deleted, and nothing is left to scale it back up.

**Find everything Reactor is currently holding down:**

```sh
kubectl get deploy -A \
  -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,REPLICAS:.spec.replicas,BASELINE:.metadata.annotations.reactor\.robbeverhelst\.com/baseline-replicas' \
  | grep -v '<none>'
```

Any row with a baseline annotation is or was claimed. A row where `REPLICAS` is 0 and `BASELINE` is not is a stranded workload, and the annotation is its restore instruction:

```sh
kubectl -n media scale deploy/qbittorrent --replicas=1
kubectl -n media annotate deploy/qbittorrent \
  reactor.robbeverhelst.com/baseline-replicas- \
  reactor.robbeverhelst.com/claimed-by- \
  reactor.robbeverhelst.com/claimed-at-
```

This works with Reactor uninstalled, which is the point of putting the baseline on the target rather than anywhere else.

**Why it should not happen from v1.** Automations holding a claim carry a `reactor.robbeverhelst.com/release-claims` finalizer. On delete, Reactor recomputes the fold without the deleted Automation, patches the target, clears the annotations if it was the last claimant, and removes the finalizer. Deleting an Automation mid-outage brings the workload back even though the condition still holds — removing the automation removes the policy.

**Three cases the finalizer does not cover:**

*`kubectl delete` while the operator is down.* The resource sits in `Terminating` until the operator returns and processes the finalizer. Start it back up if you can. If the operator is gone for good and you need the resource gone:

```sh
kubectl -n media patch automation pause-downloads-on-backup-wan \
  --type=merge -p '{"metadata":{"finalizers":[]}}'
```

That strands whatever it was holding. Restore the target by hand using the baseline annotation above.

*`helm uninstall`.* The CRD carries `helm.sh/resource-policy: keep`, so both it and every Automation stored under it *survive* the uninstall — deliberately, because losing people's resources to an uninstall is worse. They simply stop reconciling, and workloads freeze wherever Reactor last put them. No finalizer ever fires, because nothing is being deleted. The chart ships a pre-delete hook that releases every claim before the controller goes away, gated by `uninstall.releaseClaims` (default `true`); if you disabled it, or the hook failed, sweep the annotations manually before removing the chart.

The hook stops the operator before it releases anything. Helm removes the release's own resources only once its pre-delete hooks have finished, so a controller left running would re-claim every workload the hook had just released — and re-add the finalizer, which by then has nothing left to service it. If an uninstall is interrupted after the hook has run but before Helm finishes, the operator is left scaled to zero; `helm upgrade` or `helm rollback` puts it back.

Deleting the CRD afterwards is a deliberate, separate act, and it takes every Automation with it:

```sh
kubectl delete crd automations.reactor.robbeverhelst.com
```

*Deleting the CRD while Automations still carry finalizers.* Nothing is left to remove them and deletion hangs forever. Clear the finalizers first (the patch above, over every Automation), then delete the CRD.

**What is explicitly not covered:** the controller being deleted outright, permanently evicted, or the cluster rebuilt. Reactor does not supervise its own absence. The baseline annotation on the target is the answer in those cases, and it is the reason it lives there.

> The finalizer, the pre-delete hook, and `uninstall.releaseClaims` land with the target-ownership change. On v0.3.0 there is no finalizer and no release-on-delete: deleting an Automation strands its target, and the manual restore above is the only route.

---

## 9. GitOps: Reactor's changes look like drift

If your targets are managed by Flux or Argo CD, Reactor and your GitOps controller will fight over the same Deployments. Reactor writes:

- `spec.replicas` — already true today, and the entire point of the operator
- `metadata.annotations.reactor.robbeverhelst.com/*` — the baseline and claim record

Both must be excluded from drift detection on any target an Automation names, or your GitOps controller will restore the replica count Reactor just changed, in a loop.

Argo CD, on the target Application:

```yaml
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      name: qbittorrent
      jsonPointers:
        - /spec/replicas
        - /metadata/annotations/reactor.robbeverhelst.com~1baseline-replicas
```

If you would rather exclude by field manager than by path, check what Reactor's patches are actually recorded under before writing the name into config:

```sh
kubectl -n media get deploy qbittorrent -o jsonpath='{.metadata.managedFields[*].manager}'
```

Flux, on the target Kustomization, via `spec.patches` or by omitting `replicas` from the source manifest entirely. The general rule: whatever field an Automation controls should not be specified in Git.

---

## 15. Reactor and a HorizontalPodAutoscaler want the same Deployment

The symptom is a workload flapping on a fifteen-second cycle during an outage, or `Applied=False` with reason `TargetManagedByHPA` and no flapping at all — which of the two you get depends on whether detection is on.

**What is happening.** Reactor writes `spec.replicas`; an HPA computes one from metrics and writes it back. [§7](#7-two-automations-fighting-over-one-target) does not apply: arbitration resolves claims *between Automations*, because Reactor can see all of them, and an HPA is a claimant it cannot see. There is nothing to fold it into.

It is worth knowing this got *louder* rather than quieter with target ownership. Claims are re-asserted on every reconcile rather than once per transition, so what used to be a one-off flap is now a sustained oscillation. Same bug, more visible.

**Confirm it:**

```sh
kubectl -n media get hpa -o custom-columns=\
'NAME:.metadata.name,KIND:.spec.scaleTargetRef.kind,TARGET:.spec.scaleTargetRef.name'
```

Anything whose `TARGET` an Automation also names is in this fight.

**The fix is to turn detection on**, and it is worth doing before you need it:

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set safety.detectHPA=true
```

Reactor then lists the HPAs in a target's namespace before claiming it, and if one points at the target it writes nothing at all — not the replica count and not the baseline annotation, because a baseline captured from a value the HPA is actively changing would restore a meaningless number later. `status.targets[].managedBy` names the HPA, a Warning Event says so, `reactor_arbitrations_total{outcome="declined"}` counts it, and the Automation stays `Ready=True`. Its other targets are unaffected.

**A workload Reactor was already holding is handed back** to its baseline when an HPA appears over it, and then let go. That is deliberate and it is the case worth getting right: an HPA does not scale a workload up from zero, so a Reactor that simply went quiet while holding it at 0 would leave it there with neither controller willing to move it.

**Detection is on but every claim now fails.** The permission is missing:

```sh
kubectl auth can-i list horizontalpodautoscalers.autoscaling \
  --namespace media --as system:serviceaccount:reactor-system:reactor
```

Reactor fails closed here rather than writing blind, because an install that turned detection on has said it cares. `safety.detectHPA=true` grants it; a hand-written Role copied from an older chart will not have it.

**"I want Reactor to win during the outage."** There is no `force`, on purpose. Overriding means writing `spec.replicas` harder, which is the oscillation rather than a way out of it — the HPA syncs again in seconds. What would actually work is suspending the HPA by patching its `minReplicas`/`maxReplicas`, and that is *write* access to somebody's autoscaling policy, which is a much bigger permission and a separate decision. Today the answers are: remove or suspend the HPA, or shed load somewhere it does not reach — `kubernetes.cronjob.suspend` and `kubernetes.cordon` are unaffected by any of this.

**An empty `managedBy` is not a promise.** KEDA, a GitOps controller correcting drift, and a cron job running `kubectl scale` own `spec.replicas` just as hard, and none of them is discoverable through a stable API. An HPA is the common case and the one that can be seen; the general problem is not solvable by detection. If a workload flaps and no HPA names it, look for those next, and see [§9](#9-gitops-reactors-changes-look-like-drift).

---
