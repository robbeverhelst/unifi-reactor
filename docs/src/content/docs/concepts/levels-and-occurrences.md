---
title: "Levels vs occurrences"
description: "Desired-state actions declare a level that is arbitrated across every Automation sharing a target; edge actions declare an occurrence and own nothing. What decides which is which."
---

## The two shapes an action has

| | Declares | Arbitrated? | Types |
| --- | --- | --- | --- |
| **Desired-state** | a *level* — what a target should be | yes, continuously across every automation sharing the target | `kubernetes.scale`, `kubernetes.cronjob.suspend`, `kubernetes.cordon` |
| **Edge** | an *occurrence* | no — fires on this automation's own transition and owns nothing | `kubernetes.restart`, `http.request`, `notification.*`, `homeassistant.service`, `qbittorrent.*`, `unifi.wlan.*`, `unifi.poe.cycle`, `unifi.outlet.*` |

`kubernetes.scale` works through the [scale subresource](https://kubernetes.io/docs/reference/using-api/api-concepts/#subresources), so `kind: Deployment` and `kind: StatefulSet` take the same path and Reactor never has to know where a kind keeps its replicas. `target.kind` is still a closed list, on purpose: a kind is only reachable if the chart granted RBAC for it, and RBAC has to name resources explicitly — so an open field would turn a typo into a `Forbidden` discovered *during* the outage, instead of a rejected write at admission.

A level is ordered and nothing else: **lower is more restrictive, and a shared target resolves to the lowest anyone asked for.** For `kubernetes.scale` that is the replica count, so shedding wins. For `kubernetes.cronjob.suspend` and `kubernetes.cordon` it is a switch, ordered so that *suspended* and *cordoned* are the restrictive answers — which means suspended wins over running for exactly the same reason 0 replicas wins over 3, and with no new rule to learn. `status.targets[].level` says which in words.

**What decides which column an action lands in is not whether it expresses a level.** [Pausing torrents](/actions/external-services/#qbittorrent) plainly does, and so does [a WLAN being enabled](/actions/unifi-console/#switching-a-wireless-network-off), and both are edge actions anyway. It is whether there is somewhere to record the value the target held *before* Reactor claimed it — because without that, release cannot put it back, and an automation that cannot hand a target back has no business claiming it. For a Kubernetes object that place is an annotation on the object. For anything else there is no answer yet, which is why every desired-state action so far is a `kubernetes.*` one, and why the actions that reach outside the cluster are named as verbs.

The [HPA decline path](/concepts/arbitration/#when-something-else-already-owns-the-workload) is the same rule seen from the other side: a scalable target another controller drives can be refused *and handed back to what it was*, and it can be handed back precisely because the baseline is on the object. Nothing outside the cluster has that, so nothing outside the cluster can be declined back to anything.
