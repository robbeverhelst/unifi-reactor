---
title: "Suspend a Kubernetes CronJob during an outage"
description: "The nightly backup starts at 02:00 while the rack is on battery. Suspend the CronJob from a UniFi state key instead, and unsuspend it when power or the uplink comes back."
---

Scaling cannot express "do not start the nightly backup tonight". A CronJob has
no replica count to hold at zero — it has a schedule, and at 02:00 it will
create a Job whatever the rest of the cluster is doing.

That is the single highest-value thing to stop during a power cut or on a
metered uplink, and it is one field: `spec.suspend`.

## What this assumes

- Reactor is installed and `Ready` — [Install](/start/install/).
- The CronJob exists. Reactor never creates one.
- It is in a namespace Reactor can reach. A target in a different namespace
  from the Automation needs `rbac.clusterWide` (the default); with it false,
  an Automation can only target its own namespace.
- The state key you match on is published — `ups*` only with a UniFi UPS
  adopted, `wan`/`internet` only with a gateway. See [State keys](/state-keys/).

## Stop the backup while the rack is on battery

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: hold-backups-on-battery
  namespace: velero
spec:
  when:
    provider: unifi
    state:
      ups: on-battery

  actions:
    - type: kubernetes.cronjob.suspend
      target: { kind: CronJob, name: velero-backup, namespace: velero }
```

That is complete. `suspended` defaults to `true` — it is what the action is
named after — and with `onExit` omitted the reversal policy is `Baseline`:
Reactor recorded whether the CronJob was suspended **before** it first claimed
it, in `reactor.robbeverhelst.com/baseline-suspend` on the CronJob itself, and
restores exactly that.

That default matters here more than it looks. If you had suspended the backup by
hand last Tuesday and forgotten, the baseline is `true` and Reactor leaves it
suspended when the power comes back, rather than quietly starting a job you had
switched off.

If you would rather say it out loud:

```yaml
  onExit:
    - type: kubernetes.cronjob.suspend
      target: { kind: CronJob, name: velero-backup, namespace: velero }
      suspended: false
```

Now the CronJob is unsuspended on the way out regardless of what it was — which
is the right choice when the schedule matters more than the manual override, and
the wrong one if anybody ever pauses it by hand.

## For an offsite backup, match the uplink instead

A backup that ships to S3 has a different failure mode: the rack has power, the
uplink is the metered one, and a 400GB sync is about to leave over 5G.

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: hold-offsite-sync-on-backup-wan
  namespace: backup
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: kubernetes.cronjob.suspend
      target: { kind: CronJob, name: restic-offsite }
```

Omitting `namespace` on the target means the Automation's own namespace, which
is the common case and needs no cluster-wide RBAC.

## Suspending stops new Jobs, and nothing else

**A Job already running keeps running.** Suspending sets `spec.suspend` on the
CronJob, which stops the controller creating more Jobs; it does not touch, evict
or delete the one that started at 01:59.

That is deliberate, and it is enforced rather than promised: Reactor is granted
no permission over `jobs` at all, so it could not delete one if it wanted to.
Declining to start more work is a very different act from killing work in
flight, and killing work in flight is not a decision an outage should take on
your behalf. If a running backup is what needs stopping, stop it yourself.

**Unsuspending does not run what was missed.** The CronJob controller does not
catch up on schedules that passed while it was suspended (subject to the
CronJob's own `startingDeadlineSeconds`). A four-hour outage means the 02:00
backup did not happen, not that it happens at 06:00.

## What you will see when it fires

```sh
kubectl -n velero describe automation hold-backups-on-battery
```
```text
Type    Reason        Age   From        Message
----    ------        ----  ----        -------
Normal  StateEntered  15s   automation  ups moved from "online" to "on-battery", so the condition started holding
Normal  TargetHeld    15s   automation  CronJob/velero/velero-backup held at suspended
```

The level is reported in words rather than as a number, because a bare `0` stops
explaining itself once a target's level is a switch rather than a count:

```sh
kubectl -n velero get automation hold-backups-on-battery -o jsonpath='{.status.targets[0]}'
# {"ref":"CronJob/velero/velero-backup","desired":0,"effective":0,"level":"suspended"}
```

Underneath, it is an ordinary `kubectl` change with an ordinary audit trail:

```sh
kubectl -n velero get cronjob velero-backup
# NAME            SCHEDULE     TIMEZONE   SUSPEND   ACTIVE   LAST SCHEDULE   AGE
# velero-backup   0 2 * * *    <none>     True      0        14h             97d

kubectl -n velero get cronjob velero-backup -o jsonpath='{.metadata.annotations}'
# {"reactor.robbeverhelst.com/baseline-suspend":"false",
#  "reactor.robbeverhelst.com/claimed-by":"velero/hold-backups-on-battery",
#  "reactor.robbeverhelst.com/claimed-at":"2026-08-15T02:40:11Z"}
```

And on the way back:

```text
Normal  StateExited     6s   automation  ups moved from "on-battery" to "online", so the condition stopped holding
Normal  TargetReleased  6s   automation  CronJob/velero/velero-backup released; no automation claims it any more
```

## Two Automations, one CronJob

Suspending is a **desired-state** action, so it is arbitrated exactly as scaling
is. Point one Automation at the backup for `ups: on-battery` and another at the
same backup for `wan: backup`, and the CronJob stays suspended while **either**
condition holds, coming back only when neither does. Nothing has to be
coordinated, and the one that is currently outvoted says so:

```sh
kubectl -n velero get automation hold-backups-on-battery -o jsonpath='{.status.targets[0]}'
# {"ref":"CronJob/velero/velero-backup","desired":1,"effective":0,"level":"suspended",
#  "deferredBy":["backup/hold-offsite-sync-on-backup-wan"]}
```

`Applied=False` with reason `DeferredToOtherAutomation` is **not** a fault, and
the Event for it is `Normal`. Being outvoted by a more restrictive claim is how
two Automations sharing a target are meant to behave.
[Arbitration](/concepts/arbitration/) has the whole model.

One thing to avoid: if both Automations write an explicit `onExit`, make them
agree. Two Automations declaring *different* levels for one target once nothing
claims it cannot both be right — a CronJob has one normal setting — so Reactor
takes the most restrictive, and raises a `ReversalDisagreement` Warning naming
both specs so somebody can fix one. It is raised while the disagreement exists
rather than at release, because the point of knowing is finding out before the
outage ends. [Reversal and baselines](/concepts/reversal-and-baselines/).

## What this does not cover

- **Not Jobs.** No permission, on purpose. See above.
- **Not the workloads the job talks to.** Suspending the backup CronJob does not
  scale down the database it dumps —
  [that is a separate action](/guides/shut-down-kubernetes-when-the-ups-is-on-battery/).
- **Not schedules outside Kubernetes.** A cron entry on a NAS is invisible to
  Reactor. A [`notification.*`](/guides/get-notified-when-the-wan-fails-over/)
  or an [`http.request`](/actions/notifications-and-http/#httprequest) is the
  reach it has there.
- **Not a Job that is already `Pending` for other reasons.** Suspension is about
  creation, not scheduling.
- **No `kubernetes.drain`.** Deliberately not implemented — an eviction cannot
  be un-evicted, so it has no level to arbitrate and no reversal to declare.
  [The four reasons](/actions/kubernetes/#why-there-is-no-kubernetesdrain).

## Where to go next

- [Kubernetes actions](/actions/kubernetes/) — the rest of what Reactor can do to a Kubernetes object.
- [Levels vs occurrences](/concepts/levels-and-occurrences/) — why suspending arbitrates and restarting does not.
- [Shut down Kubernetes when the UPS goes on battery](/guides/shut-down-kubernetes-when-the-ups-is-on-battery/) — the rung this one belongs on.
- [Automation API reference](/reference/automation/) — `suspended`, `target`, `reversal` and everything else.
- [Chart values reference](/reference/values/) — `rbac.clusterWide` and the rest of the install surface.
