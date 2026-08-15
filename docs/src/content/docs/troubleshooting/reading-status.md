---
title: "Reading an Automation’s status and Events"
description: "What the Ready and Applied conditions actually mean, every reason string either can carry, and how to read the Event stream on a resource as the story of an incident."
---

## Reading the `Ready` condition

| Reason | Meaning | Where to look |
| --- | --- | --- |
| `Reconciled` | Normal. Evaluated against observed state. | — |
| `Suspended` | `spec.suspend: true`. State is still observed; no target is claimed. | [§1](/troubleshooting/nothing-is-happening/#1-nothing-happens-when-the-state-changes) |
| `DryRun` | `spec.dryRun: true`, or the whole install runs with `safety.dryRun`. Everything is evaluated; nothing is written. | [§14](/troubleshooting/nothing-is-happening/#14-an-automation-is-not-acting-and-is-telling-you-what-it-would-do) |
| `ProviderStateUnavailable` | No state has been observed yet for this provider. | [§1](/troubleshooting/nothing-is-happening/#1-nothing-happens-when-the-state-changes) |
| `StateKeyUnavailable` | A key this Automation needs vanished from the observation. Last known matching state is held. | [§2](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state) |

| `ObservationStale` | The console has stopped answering at all, and the last state it gave is older than `unifi.maxObservationAge`. Everything is held and still acted on. | [§2a](/troubleshooting/state-keys/#2a-observationstale-and-how-old-a-decision-is-allowed-to-be) |
| `ActionFailed` | An action returned an error. `status.lastExecution.reason` has the message. | [§5](/troubleshooting/rbac-and-crd/#5-rbac-refuses-a-cross-namespace-target), [§6](/troubleshooting/rbac-and-crd/#6-the-crd-invalid-ownership-metadata-or-a-stale-schema) |

`Applied` carries its own reasons, and two of them are not faults: `DeferredToOtherAutomation` is a peer's more restrictive claim winning, and `TargetManagedByHPA` is Reactor declining a target another controller drives — [§15](/troubleshooting/conflicts-and-drift/#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment).

An Automation left over from before `spec.trigger` was removed has no conditions at all: `spec.when` is now required, so the API server rejects any write to it, status included. It is reported once per reconcile in the operator log and as a Warning Event on the resource instead:

```sh
kubectl -n media describe automation notify-on-client-connect | tail -5
# Warning  EventTriggerRemoved  spec.trigger was removed from v1alpha1 and was never
#          implemented; this automation does nothing, delete it
```

Delete it. Event triggers never ran on any version, so nothing is lost, and nothing it referenced was ever claimed. See the README's [Stability](/design/stability/) section for when they come back.

> On v0.3.0, `ActionFailed` is reported with `status: "True"` — a bug where the condition status was not flipped alongside the reason. Read the *reason*, not the status, on that version. Fixed in the target-ownership batch.

---

## Reading the Event stream

`kubectl describe automation <name>` ends with an Event list, and for most
questions it is faster than anything else on this page — it needs no log
access, and it is already in chronological order:

```sh
kubectl -n media describe automation pause-downloads-on-backup-wan | tail -20
```

Each reason below links to the part of this guide that deals with it. The [Events and condition reasons reference](/reference/events/) is the same list generated from the controller, with the full note on each one and no troubleshooting.

| Reason | Type | Means |
| --- | --- | --- |
| `StateEntered` / `StateExited` | Normal | the condition started or stopped holding; the message names the key that moved |
| `TargetHeld` / `TargetReleased` | Normal | a write to a target actually happened; the message names the level in words ("0 replicas", "suspended") |
| `DeferredToOtherAutomation` | Normal | a peer's more restrictive claim won — [§7](/troubleshooting/conflicts-and-drift/#7-two-automations-fighting-over-one-target) |
| `EdgeActionSent` | Normal | an edge action ran: a notification or HTTP request delivered, or a restart applied |

| `ReversalDisagreement` | Warning | two Automations declared different `onExit` levels for one target, so they disagree about its normal size — [§7](/troubleshooting/conflicts-and-drift/#the-workload-came-back-at-the-wrong-number) |
| `StateKeyUnavailable` | Warning | a key vanished and state is being held — [§2](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state) |

| `ObservationStale` | Warning | the console has stopped answering and decisions are being taken against old state — [§2a](/troubleshooting/state-keys/#2a-observationstale-and-how-old-a-decision-is-allowed-to-be) |
| `ActionFailed` | Warning | a desired-state action could not be applied — [§5](/troubleshooting/rbac-and-crd/#5-rbac-refuses-a-cross-namespace-target) |
| `RetryBudgetExhausted` | Warning | Reactor stopped retrying and is waiting for the next state change |
| `EdgeActionFailed` / `EdgeActionSkipped` | Warning | an edge action did not happen — [§12](/troubleshooting/actions-and-targets/#12-an-edge-action-did-not-happen--or-happened-too-often) |
| `ReleaseFailed` | Warning | deletion could not hand a target back and let the object go anyway — [§8](/troubleshooting/conflicts-and-drift/#8-a-workload-is-stuck-down-after-an-automation-was-deleted) |
| `EventTriggerRemoved` | Warning | a leftover `spec.trigger` automation that does nothing; delete it |
| `DryRun` | Normal | a dry run reached the transition it would have acted on; the message says what it would have done — [§14](/troubleshooting/nothing-is-happening/#14-an-automation-is-not-acting-and-is-telling-you-what-it-would-do) |
| `TargetManagedByHPA` | Warning | a HorizontalPodAutoscaler already drives the target, so Reactor declined it rather than fight — [§15](/troubleshooting/conflicts-and-drift/#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment) |

**Being outvoted is `Normal`, not a Warning.** Two Automations sharing a
workload and one of them losing is the arbitration working as designed.
`ReversalDisagreement` is the Warning next to it, and the difference is not
severity for its own sake: two automations wanting a workload down for different
reasons are both right, while two declaring different normal sizes for it cannot
both be — nothing Reactor does resolves that, so somebody has to.

**Events fire on edges, not on states.** A condition that has been held for an
hour raised one Event when it started, not one every fifteen seconds — so an
old timestamp on a Warning means "still true since then", not "stale". Read the
Age column with that in mind, and read `status.conditions` for what is true
*now*.

**No Events at all** on an Automation that is clearly doing things has one
likely cause: the operator's RBAC does not grant `create` and `patch` on
`events` in the **`events.k8s.io`** API group. A rule naming only the core
group (`""`) is refused on every emission, and the refusal is logged by the
event broadcaster and surfaced nowhere else:

```sh
kubectl auth can-i create events.events.k8s.io \
  --namespace media \
  --as system:serviceaccount:reactor-system:reactor
```

Charts from this release on grant it. An operator installed from an older chart,
or with hand-written RBAC copied from one, will be silent.

**They expire.** Events live on your cluster's retention, an hour by default.
They are for the incident you are in; `status` is the durable record.

---
