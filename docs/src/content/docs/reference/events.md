---
title: "Events and condition reasons"
description: "Every Event reason Reactor raises, whether it is Normal or Warning, and every reason an Automation reports on its Ready and Applied conditions."
editUrl: false
tableOfContents:
  maxHeadingLevel: 2
---

:::note[Generated from source]
This page is generated from `internal/controller` by `make docs`. Change it there — CI fails when this file and its source disagree.
:::

What `kubectl describe automation` and `kubectl get events` will say, and what each `status.conditions[].reason` means. [Events and status](/operations/events/) covers how to read them; this is the whole list.

Warning is reserved for something an operator has to act on. A held state, a deferred claim and a reversal are all Normal: they are the design working.

## Event reasons

| Reason | Type | Action |
| --- | --- | --- |
| [`ActionFailed`](#actionfailed) | Warning | `Execute` |
| [`DeferredToOtherAutomation`](#deferredtootherautomation) | Normal | `Execute` |
| [`DryRun`](#dryrun) | Normal | `Execute` |
| [`EdgeActionFailed`](#edgeactionfailed) | Warning | `EdgeAction` |
| [`EdgeActionSent`](#edgeactionsent) | Normal | `EdgeAction` |
| [`EdgeActionSkipped`](#edgeactionskipped) | Warning | `EdgeAction` |
| [`EventTriggerRemoved`](#eventtriggerremoved) | Warning | `Reconcile` |
| [`ObservationStale`](#observationstale) | Warning | `Evaluate` |
| [`ReleaseFailed`](#releasefailed) | Warning | `Release` |
| [`RetryBudgetExhausted`](#retrybudgetexhausted) | Warning | `Execute` |
| [`ReversalDisagreement`](#reversaldisagreement) | Warning | `Execute` |
| [`StateEntered`](#stateentered) | Normal | `Evaluate` |
| [`StateExited`](#stateexited) | Normal | `Evaluate` |
| [`StateKeyUnavailable`](#statekeyunavailable) | Warning | `Evaluate` |
| [`TargetHeld`](#targetheld) | Normal | `Execute` |
| [`TargetManagedByHPA`](#targetmanagedbyhpa) | Warning | `Execute` / `Release` |
| [`TargetReleased`](#targetreleased) | Normal | `Release` |
| [`TemplateWillNotRender`](#templatewillnotrender) | Warning | `Evaluate` |

### `ActionFailed`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Execute`

Reported when a desired-state action could not be
applied to its target.

### `DeferredToOtherAutomation`

**Type:** Normal &nbsp;·&nbsp; **Action:** `Execute`

Normal on purpose. Being outvoted by a more restrictive
claim is how two Automations sharing a workload are meant to behave, and
reporting it as a fault would train people to ignore it.

Message:

- a more restrictive claim is in effect: %s

### `DryRun`

**Type:** Normal &nbsp;·&nbsp; **Action:** `Execute`

Normal, and is the whole output of a mode whose output is
the point. It is raised on the transition a dry run would have acted on,
because that is the moment worth telling somebody about and the one thing
status cannot tell them: status is a poll, and nobody polls a resource at
the second it becomes interesting.

Message:

- this install runs as a dry run: every target was arbitrated and nothing was written
- dry run: nothing was written. In force, this automation would %s

### `EdgeActionFailed`

**Type:** Warning &nbsp;·&nbsp; **Action:** `EdgeAction`

`EdgeActionSent`, `EdgeActionFailed` and `EdgeActionSkipped`
are the Event reasons an operator greps for. A reaction Reactor performed
should be visible without reading controller logs, which is the whole
point of having these actions at all.

### `EdgeActionSent`

**Type:** Normal &nbsp;·&nbsp; **Action:** `EdgeAction`

`EdgeActionSent`, `EdgeActionFailed` and `EdgeActionSkipped`
are the Event reasons an operator greps for. A reaction Reactor performed
should be visible without reading controller logs, which is the whole
point of having these actions at all.

### `EdgeActionSkipped`

**Type:** Warning &nbsp;·&nbsp; **Action:** `EdgeAction`

`EdgeActionSent`, `EdgeActionFailed` and `EdgeActionSkipped`
are the Event reasons an operator greps for. A reaction Reactor performed
should be visible without reading controller logs, which is the whole
point of having these actions at all.

### `EventTriggerRemoved`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Reconcile`

Reported for an Automation left over from when
the API had a second, never-implemented trigger kind.

Message:

- spec.trigger was removed from v1alpha1 and was never implemented; this automation does nothing, delete it

### `ObservationStale`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Evaluate`

The adjacent case, and a Warning for the same
reason. There the console answered and left a key out; here it has
stopped answering, so every key is as old as the last reply. Both hold
the last known state and go on acting on it, because losing sight of the
world is not evidence about the world — and both have to say so, or the
only difference between Reactor working and Reactor blind is a graph.

It is raised only on an install that set unifi.maxObservationAge. Without
a bound there is no such thing as too old, and inventing one would make
an upgrade start reporting a fault that was always there.

### `ReleaseFailed`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Release`

Raised when deletion gives up handing targets back.

Message:

- could not hand targets back after %d attempts, deleting anyway: %v

### `RetryBudgetExhausted`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Execute`

Raised once, when Reactor stops retrying a
target and starts waiting for the next state change instead.

Message:

- gave up after %d attempts; waiting for the next state change rather than retrying forever: %v

### `ReversalDisagreement`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Execute`

A Warning, and it is the one place that
distinction is worth arguing next to `DeferredToOtherAutomation` above.

Being outvoted on a live claim is Normal because both Automations are
right: two conditions hold, both want the workload down, and the fold is
how they were always meant to compose. Two Automations declaring
different levels for one target once NOTHING claims it cannot both be
right — a workload has one normal size — so nothing Reactor does resolves
this, and min is a tie-break rather than an answer. Somebody has to
change one of the specs, which is precisely the line this file draws
Warning at.

The failure it predicts is also the quiet kind: the workload comes back
at the wrong number after the incident, when nobody is watching, and the
cause is in a spec written weeks earlier.

Message:

- %s: %s. They cannot both be its normal level — Reactor takes the most restrictive, %s, and changing one of the specs is the only thing that resolves it

### `StateEntered`

**Type:** Normal &nbsp;·&nbsp; **Action:** `Evaluate`

`StateEntered` and `StateExited` bracket the interesting part of
every incident: the moment this Automation's condition started holding,
and the moment it stopped.

Message:

- the condition %s against observed %s state
- %s moved from %q to %q, so the condition %s

### `StateExited`

**Type:** Normal &nbsp;·&nbsp; **Action:** `Evaluate`

`StateEntered` and `StateExited` bracket the interesting part of
every incident: the moment this Automation's condition started holding,
and the moment it stopped.

Message:

- the condition %s against observed %s state
- %s moved from %q to %q, so the condition %s

### `StateKeyUnavailable`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Evaluate`

A Warning because holding state indefinitely
is not a resting place: the hardware publishing a key has gone, and
somebody has to decide whether it is coming back.

Message:

- provider %q stopped reporting %s; holding the last known state rather than treating lost sight of it as the condition ending

### `TargetHeld`

**Type:** Normal &nbsp;·&nbsp; **Action:** `Execute`

`TargetHeld` and `TargetReleased` record a write that actually
happened. Reconciling a target that is already where it should be is the
common case and produces nothing.

One reason covers every desired-state action rather than one per kind:
what happened is that arbitration moved a target to its resolved level,
and the note says in words which level that was. A per-kind reason would
make `kubectl get events --field-selector reason=...` an incomplete
question that silently stops matching each time a kind is added.

### `TargetManagedByHPA`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Execute` / `Release`

A Warning, and the one place that distinction
carries real weight. Being outvoted by a peer is the arbitration working
and is reported Normal; being unable to arbitrate at all is an automation
that cannot do its job, and no amount of waiting fixes it. Somebody has to
decide which controller owns the workload.

Message:

- not claiming %s: arbitration cannot resolve a claimant it cannot see, and writing anyway would oscillate rather than win
- %s is now driven by %s; handed it back at %s and stopped claiming it

### `TargetReleased`

**Type:** Normal &nbsp;·&nbsp; **Action:** `Release`

`TargetHeld` and `TargetReleased` record a write that actually
happened. Reconciling a target that is already where it should be is the
common case and produces nothing.

One reason covers every desired-state action rather than one per kind:
what happened is that arbitration moved a target to its resolved level,
and the note says in words which level that was. A per-kind reason would
make `kubectl get events --field-selector reason=...` an incomplete
question that silently stops matching each time a kind is added.

Message:

- %s released; no automation claims it any more

### `TemplateWillNotRender`

**Type:** Warning &nbsp;·&nbsp; **Action:** `Evaluate`

A Warning, and it is raised at write time
for something that would otherwise only be discovered at send time.

A notification's message and an http.request's body are templates whose
.State carries the keys in spec.when.state and nothing else. A template
reading a key this Automation did not match on is accepted by the API
server, reports Ready and matches — and then fails to render on the
transition it was written for, which for a WAN failover might be weeks
later and is exactly the moment somebody wanted the message. Both halves
of that question are in the spec, so it is answered when the object is
written instead.

It never stops the Automation acting. The message is the report of a
reaction rather than the reaction, so the desired-state actions beside it
still run — the same rule a delivery that fails already follows.

Message:

- a template on this automation can never render, so the action holding it would fail at send time rather than here: %s

## Condition reasons

What an Automation reports in `status.conditions[].reason`. `Ready` is whether it is valid and reconciling; `Applied` is whether what it wants is what its targets have. The two are separate because an Automation can be perfectly healthy and still not be the one deciding a target's value.

| Reason | Reported on |
| --- | --- |
| [`ActionFailed`](#actionfailed-1) | `Ready=False` / `Applied=False` |
| [`DeferredToOtherAutomation`](#deferredtootherautomation-1) | `Applied=False` |
| [`DryRun`](#dryrun-1) | `Applied=False` / `Ready=True` |
| [`InEffect`](#ineffect) | `Applied=True` |
| [`NoTargets`](#notargets) | `Applied=True` |
| [`ObservationStale`](#observationstale-1) | `Ready=False` |
| [`ProviderStateUnavailable`](#providerstateunavailable) | `Ready=False` |
| [`Reconciled`](#reconciled) | `Ready=True` |
| [`ReleaseFailed`](#releasefailed-1) | `Applied=False` |
| [`RetryBudgetExhausted`](#retrybudgetexhausted-1) | `Applied=False` |
| [`StateKeyUnavailable`](#statekeyunavailable-1) | `Ready=False` |
| [`Suspended`](#suspended) | `Ready=True` / `Applied=False` |
| [`TargetManagedByHPA`](#targetmanagedbyhpa-1) | `Applied=False` |
| [`TemplateWillNotRender`](#templatewillnotrender-1) | `Ready=False` |

### `ActionFailed`

**Reported on:** `Ready=False` / `Applied=False`

Reported when a desired-state action could not be
applied to its target.

### `DeferredToOtherAutomation`

**Reported on:** `Applied=False`

Normal on purpose. Being outvoted by a more restrictive
claim is how two Automations sharing a workload are meant to behave, and
reporting it as a fault would train people to ignore it.

### `DryRun`

**Reported on:** `Applied=False` / `Ready=True`

Normal, and is the whole output of a mode whose output is
the point. It is raised on the transition a dry run would have acted on,
because that is the moment worth telling somebody about and the one thing
status cannot tell them: status is a poll, and nobody polls a resource at
the second it becomes interesting.

Message:

- this install runs as a dry run: status.targets[].effective is what each target would be held at, and nothing was written
- spec.dryRun is true: state is observed and evaluated, and nothing is written
- a dry run claims no target; status.targets[].preview reports what this would do in force

### `InEffect`

**Reported on:** `Applied=True`

Message:

- target state matches what this automation wants

### `NoTargets`

**Reported on:** `Applied=True`

Message:

- this automation only has edge actions, so it holds no target

### `ObservationStale`

**Reported on:** `Ready=False`

The adjacent case, and a Warning for the same
reason. There the console answered and left a key out; here it has
stopped answering, so every key is as old as the last reply. Both hold
the last known state and go on acting on it, because losing sight of the
world is not evidence about the world — and both have to say so, or the
only difference between Reactor working and Reactor blind is a graph.

It is raised only on an install that set unifi.maxObservationAge. Without
a bound there is no such thing as too old, and inventing one would make
an upgrade start reporting a fault that was always there.

### `ProviderStateUnavailable`

**Reported on:** `Ready=False`

Message:

- no state observed yet for provider %q

### `Reconciled`

**Reported on:** `Ready=True`

Message:

- automation evaluated against observed state

### `ReleaseFailed`

**Reported on:** `Applied=False`

Raised when deletion gives up handing targets back.

### `RetryBudgetExhausted`

**Reported on:** `Applied=False`

Raised once, when Reactor stops retrying a
target and starts waiting for the next state change instead.

Message:

- giving up after %d attempts, will try again on the next state change: %v

### `StateKeyUnavailable`

**Reported on:** `Ready=False`

A Warning because holding state indefinitely
is not a resting place: the hardware publishing a key has gone, and
somebody has to decide whether it is coming back.

Message:

- provider %q is not reporting %s; holding last known state

### `Suspended`

**Reported on:** `Ready=True` / `Applied=False`

Reported while spec.suspend takes an Automation out
of force.

Message:

- spec.suspend is true: state is still observed, and no target is claimed
- suspended, so this automation's targets are arbitrated as if it did not exist

### `TargetManagedByHPA`

**Reported on:** `Applied=False`

A Warning, and the one place that distinction
carries real weight. Being outvoted by a peer is the arbitration working
and is reported Normal; being unable to arbitrate at all is an automation
that cannot do its job, and no amount of waiting fixes it. Somebody has to
decide which controller owns the workload.

### `TemplateWillNotRender`

**Reported on:** `Ready=False`

A Warning, and it is raised at write time
for something that would otherwise only be discovered at send time.

A notification's message and an http.request's body are templates whose
.State carries the keys in spec.when.state and nothing else. A template
reading a key this Automation did not match on is accepted by the API
server, reports Ready and matches — and then fails to render on the
transition it was written for, which for a WAN failover might be weeks
later and is exactly the moment somebody wanted the message. Both halves
of that question are in the spec, so it is answered when the object is
written instead.

It never stops the Automation acting. The message is the report of a
reaction rather than the reaction, so the desired-state actions beside it
still run — the same rule a delivery that fails already follows.
