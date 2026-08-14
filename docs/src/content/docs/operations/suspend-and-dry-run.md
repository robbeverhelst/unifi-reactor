---
title: "Suspending an Automation, and dry runs"
description: "Take one Automation out of force without deleting it, ask what it would do before letting it do it, or bring up a whole install that writes nothing and is held to that by RBAC."
---

## Pausing an automation

`spec.suspend: true` takes an automation out of force without deleting it — during an incident, while testing, or when one is misbehaving:

```sh
kubectl -n media patch automation shed-on-battery --type=merge -p '{"spec":{"suspend":true}}'

kubectl -n media get automation
# NAME                    PROVIDER   MATCHING   SUSPENDED   READY   AGE
# pause-on-backup-wan     unifi      false      false       True    3h
# shed-on-battery         unifi      true       true        True    3h
```

**Suspending is a reversible delete, not a freeze.** A suspended automation keeps observing state and reporting `matching`, `observedState` and `lastTransition` — that is what makes it worth leaving in place while you debug — and stops claiming its targets entirely. Each target is arbitrated as if the automation were not there, so it goes back to whatever the other automations claiming it want, or to this one's [`reversal`](/concepts/reversal-and-baselines/#what-coming-back-means) if none do. It reports `Ready=True`, `Applied=False` with reason `Suspended`.

Deletion gives the same answer, on purpose: "pause this" and "remove this" should not mean different things to a workload one of them is holding down. Two consequences worth knowing:

- **A suspended automation cannot strand a workload**, because it is not holding one. Deleting one is equally uneventful — its finalizer has nothing left to release.
- **It never fights you.** A suspended automation writes nothing. If it was the only claimant, Reactor's annotations come off the target as it lets go and you can scale that workload by hand; if another automation still claims it, that one is still in charge and `claimed-by` names it.

Resuming re-evaluates against current state and replays nothing: an automation whose condition still holds re-claims its targets on the next reconcile, recording a fresh baseline from whatever the workload is at then.

If what you wanted was "leave the workload exactly where it is", say that explicitly — with nothing else claiming the target, this pauses the automation *and* stops Reactor asserting a value for it:

```sh
kubectl -n media patch automation shed-on-battery --type=merge \
  -p '{"spec":{"suspend":true,"reversal":"None"}}'
```

## Asking what an automation would do

Writing an automation means deciding what should happen to somebody's production workload during an incident you cannot rehearse. `spec.dryRun: true` lets you apply one and be told the answer instead of finding out:

```sh
kubectl -n media get automation shed-on-battery \
  -o jsonpath='{.status.targets[0]}' | jq
# {"ref": "Deployment/media/qbittorrent",
#  "effective": 1,                                  # what it is held at now, by somebody else
#  "preview": {
#    "desired": 0,                                  # what this automation would ask for
#    "effective": 0, "level": "0 replicas",         # what the arbitration would then resolve to
#    "wouldDefer": ["media/pause-on-backup-wan"],   # who would stop getting what they want
#    "onExit": "3 replicas"                         # what it would hand back afterwards
#  }}
```

A dry run is **out of force**, exactly as [suspending](#pausing-an-automation) one is: it claims nothing, writes nothing, and — the part that makes it safe to apply next to policies that are live — cannot change what any other automation's targets resolve to. What it adds is `preview`, which is the same fold run once more with its claim in it. Turning `dryRun` off is the only change needed to make it real.

It answers the question **whether or not the condition currently holds**, because the automation you most want to check is the one for a power cut and you are writing it on a Tuesday afternoon. And it says what it would do at the moment it would have done it:

```sh
kubectl -n media describe automation shed-on-battery | tail -3
# Normal  DryRun  dry run: nothing was written. In force, this automation would
#                 hold Deployment/media/qbittorrent at 0 replicas, outvoting media/pause-on-backup-wan
```

**What a preview cannot promise.** It is computed from the peers, the observed state and the target as they are at that moment, and all three can differ by the time the condition actually holds — another automation may have been written, the workload may have been scaled by hand, the baseline it would restore may not be the one it eventually records. It also says nothing about whether the write would *succeed*: RBAC, an admission webhook, a target that has since been deleted, and [a controller that already owns the field](/concepts/arbitration/#when-something-else-already-owns-the-workload) are all outside what arbitration can know. A preview is a fact about a moment, not a forecast.

For a **whole install** that has never acted — a first rollout into a cluster — there is `safety.dryRun: true`, and it is a different thing on purpose:

| | `spec.dryRun` on one automation | `safety.dryRun` on the install |
| --- | --- | --- |
| What it is for | trying one policy on a working install | bringing up an install that has never acted |
| Arbitration | this automation is out of force, so it perturbs nothing | everything stays in force and resolves normally |
| Reported as | `status.targets[].preview` | `status.targets[].effective` — the real fold, unwritten |
| Edge actions | not fired; it is out of force | recorded as `Skipped`, so you can see what would have been sent |
| Enforced by | the operator | the operator **and** the chart, which withholds every permission that could write to a target |

That last row is the point of the install-wide switch: `--dry-run` is Reactor promising not to write, and the missing `patch` and `update` grants are the API server holding it to that. Turning it on for an install that is *already* holding workloads down freezes them where they are, because releasing a claim is a write too — suspend or delete those automations first.

## The install-wide dry run, in the chart

`safety.dryRun: true` is the mode to bring a new install up in. Every Automation
is observed, evaluated and arbitrated exactly as it otherwise would be, and
nothing is written:

```sh
helm install reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system \
  --set unifi.url=https://192.0.2.1 \
  --set safety.dryRun=true
```

Each Automation reports what its targets *would* be held at in
`status.targets[].effective` — the real fold, just unwritten — says
`Applied=False` with reason `DryRun`, and records every edge action as `Skipped`
rather than sending it. `reactor_arbitrations_total{outcome="withheld"}` is the
only arbitration outcome such an install publishes, which is how a dashboard
tells a dry run from a live install that has nothing to do.

**Two locks, not one.** `--dry-run` is the operator promising not to write. The
chart holds it to that by withholding every verb that could: with `dryRun` on,
the manager's rules carry `get` on the workload kinds and their `/scale`
subresources and nothing else, and no `patch` on nodes even when
`rbac.allowNodeActions` is set. "It cannot touch your workloads" is enforced by
the API server rather than promised by a flag.

Two things worth knowing before turning it on:

- **It is not the same as `spec.dryRun` on one Automation.** That takes a single
  Automation out of force so it can be applied beside policies that are live
  without perturbing them, and reports the counterfactual in
  `status.targets[].preview`. This one stops the whole operator writing. Use
  `spec.dryRun` to try one policy on a working install, and this to bring up an
  install that has never acted.
- **Turning it on for an install that is already holding workloads down freezes
  them there**, because releasing a claim is a write too. Suspend or delete
  those Automations first, or uninstall with the pre-delete hook, which hands
  every target back. That hook is not rendered for a dry-run install — it would
  have nothing to release and no permission to release it with.
