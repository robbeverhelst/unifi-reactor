---
title: "Events and status"
description: "kubectl describe automation tells the story of a failover without log access. Which Events are raised, why being outvoted is Normal, and why nothing is raised from a steady condition."
---

## The other direction: `kubectl describe`

Metrics answer *how often*, across everything. Events answer *what happened*, to this one resource — and they need no Prometheus, no port, and no cluster-admin log access:

```sh
kubectl -n media describe automation pause-downloads-on-backup-wan
```
```text
Type     Reason                     Age    From        Message
----     ------                     ----   ----        -------
Normal   StateEntered               3m12s  automation  wan moved from "primary" to "backup", so the condition started holding
Normal   TargetHeld                 3m12s  automation  Deployment/media/qbittorrent held at 0 replicas
Normal   EdgeActionSent             3m11s  automation  notification.ntfy delivered to https://ntfy.example.com:443 after 1 attempt(s)
Normal   DeferredToOtherAutomation  2m40s  automation  a more restrictive claim is in effect: Deployment/media/qbittorrent held by power/shed-on-battery
Warning  StateKeyUnavailable        1m02s  automation  provider "unifi" stopped reporting ups; holding the last known state rather than treating lost sight of it as the condition ending
Normal   StateExited                18s    automation  wan moved from "backup" to "primary", so the condition stopped holding
Normal   TargetReleased             18s    automation  Deployment/media/qbittorrent released; no automation claims it any more
```

That is the whole failover, in order, including the part where it deliberately did nothing.

`Normal` and `Warning` are used deliberately rather than as a severity dial. Entering a state, scaling a target, releasing one, and **being outvoted by a more restrictive claim** are all `Normal` — the last one especially, because it is how two automations sharing a workload are meant to behave, and reporting it as a fault would train you to ignore Warnings here. `Warning` is reserved for something you have to act on: a held state, a failed action, a retry budget spent, a notification that did not go out.

Volume is bounded by the same rule everywhere: **Events fire on edges, not on states.** A reconcile happens at least every 15s, so anything raised from a steady condition would be an API write every 15 seconds per automation, forever. A target already at the right value produces nothing. A condition that keeps reporting the same reason produces nothing after the first. `ActionFailed` stops at the retry budget, and `RetryBudgetExhausted` replaces it exactly once.

Every reason Reactor raises, whether it is `Normal` or `Warning`, and what each one means is in the [Events and condition reasons reference](/reference/events/) — generated from the controller, so it cannot drift from what the operator actually emits.

Events are where a state key with an **open value set** is reported: `isp` is not a metric label, so `isp moved from "carrier-a" to "carrier-b"` lives here and in `status.observedState`. The two halves are complementary on purpose — Prometheus keeps what is bounded, Kubernetes keeps what is specific.

> Events are written to the `events.k8s.io/v1` API and expire on your cluster's retention (an hour by default). They are for the incident you are in, not the audit trail — `status` is the durable record.

## The Events RBAC changed: `events.k8s.io/v1`

**The RBAC changed in this release.** Reactor records through the
**`events.k8s.io/v1`** API, not the deprecated core one. They share storage but
are separate API groups for authorization, so the previous rule — which named
only `apiGroups: [""]` — was refused on every emission. The refusal is logged by
the event broadcaster inside the controller and surfaced nowhere else, so the
symptom was an Automation with no Events at all, which reads as nothing having
happened. Upgrading fixes it; hand-written RBAC copied from an older chart needs
the same change:

```sh
kubectl auth can-i create events.events.k8s.io --namespace media \
  --as system:serviceaccount:reactor-system:reactor
```
