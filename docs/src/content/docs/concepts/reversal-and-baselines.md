---
title: "Reversal and baselines"
description: "What an Automation wants once nothing claims its target: the values in onExit, the baseline recorded on the object before Reactor first touched it, or nothing at all."
---

## What "coming back" means

`onExit` declares the level an automation wants once nothing is holding the workload down. Omit it and Reactor restores the **baseline** — what the target was set to before it first claimed it, recorded on the target itself:

```sh
kubectl -n media get deploy qbittorrent -o jsonpath='{.metadata.annotations}'
# {"reactor.robbeverhelst.com/baseline-replicas":"1",
#  "reactor.robbeverhelst.com/claimed-by":"media/shed-on-battery",
#  "reactor.robbeverhelst.com/claimed-at":"2026-08-13T02:41:07Z"}
```

The baseline annotation is named for what it records, so a CronJob carries `baseline-suspend: "false"` rather than a replica count that would mean nothing there — `baseline-replicas` keeps meaning exactly one replica count, forever. Those annotations are how a workload explains itself at 3am, and they are removed the moment nothing claims it — after which Reactor asserts nothing and you can scale it by hand freely.

| `spec.reversal` | What the automation wants once nothing claims the target | Default when |
| --- | --- | --- |
| `Declared` | the values in `onExit` | `onExit` is set |
| `Baseline` | whatever the target was before Reactor first claimed it | `onExit` is omitted |
| `None` | nothing — leave it wherever it was left | never; opt in explicitly |

> **Upgrading from v0.3.0:** an automation with no `onExit` used to leave its workload scaled down permanently. It now restores the baseline instead. Set `reversal: None` to keep the old behaviour.

> **GitOps:** Reactor writes `spec.replicas` and the three annotations above onto target Deployments. If Flux or Argo CD manages those Deployments it will report drift and revert them. Exclude the fields on any workload you let Reactor act on — Argo CD `ignoreDifferences` on `/spec/replicas` and the `reactor.robbeverhelst.com` annotations, or a Flux `patch` with the same exclusions.

## When they disagree about coming back

Two automations can share a workload and still not agree on what its normal size is:

```yaml
# shed-a                          # shed-b
onExit:                           onExit:
  - type: kubernetes.scale          - type: kubernetes.scale
    target: {kind: Deployment, name: qbittorrent}
    replicas: 1                       replicas: 3
```

While either matches, the workload sits at 0 and everything above applies. When both stop matching, the reversals are folded the same way live claims are — `min(1, 3) = 1` — and the workload comes back at 1 and stays there.

**Reactor keeps taking `min`, and does not try to resolve this.** It cannot know which number was meant, and picking the more restrictive one is defensible, documented and order-independent, exactly as it is for a live claim.

**What it will not do is resolve it silently.** Two automations declaring different reversal levels for one target is a contradiction visible in the specs themselves — no intent has to be guessed to see it — so it is reported from the moment it exists, not at the moment the workload comes back at the wrong number:

```sh
kubectl -n media get automation shed-a -o jsonpath='{.status.targets[0].reversalDisagreement}'
# [{"claimant":"media/shed-a","desired":1,"level":"1 replicas"},
#  {"claimant":"media/shed-b","desired":3,"level":"3 replicas"}]
```

```sh
kubectl -n media describe automation shed-a | tail -3
# Warning  ReversalDisagreement  Deployment/media/qbittorrent: media/shed-a wants 1 replicas,
#          media/shed-b wants 3 replicas. They cannot both be its normal level — Reactor takes
#          the most restrictive, 1 replicas, and changing one of the specs is the only thing
#          that resolves it
```

and `reactor_reversal_disagreements_total` for the fleet-wide version.

It is a **Warning**, unlike being outvoted on a live claim, and the difference is not severity for its own sake. Two automations wanting a workload down for different reasons are both right, and arbitration between them is the design working — that is `Normal`. Two automations declaring different normal sizes for one workload cannot both be right; nothing Reactor does resolves it, and the number it picks is only a tie-break. Somebody has to change one of the specs.

`reversal: None` contributes no level at all, so it is never part of a disagreement. Two automations both on `Baseline` agree by construction — they resolve to the same recorded baseline — so the cases this catches are `Declared` against `Declared`, and `Declared` against `Baseline`.
