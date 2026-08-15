---
title: "Your first Automation"
description: "One Automation resource that pauses downloads while the WAN is on the backup uplink and resumes them when it recovers — both directions, in twenty lines of YAML."
---

## Your first automation

Pause downloads while the WAN is on the backup uplink, and resume them when it recovers — one resource covers both directions:

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: pause-downloads-on-backup-wan
  namespace: media
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: kubernetes.scale
      target: { kind: Deployment, name: qbittorrent }
      replicas: 0

  onExit:
    - type: kubernetes.scale
      target: { kind: Deployment, name: qbittorrent }
      replicas: 1
```

```sh
kubectl -n media get automation
# NAME                           PROVIDER   MATCHING   SUSPENDED   READY   AGE
# pause-downloads-on-backup-wan  unifi      false      false       True    12s
```

Shedding load during a power cut is the same shape, matching `ups: on-battery` instead.

## Where to go next

- [Guides](/guides/shut-down-kubernetes-when-the-ups-is-on-battery/) — the same shape applied to a real job: shedding load on battery, pausing downloads on a metered uplink, suspending a CronJob, getting told about a failover.
- [The two shapes an action has](/concepts/levels-and-occurrences/) — why some actions are arbitrated and some just fire.
- [Kubernetes actions](/actions/kubernetes/) — suspending a CronJob, cordoning a node, restarting a workload.
- [State keys](/state-keys/) — everything else Reactor can match on.
- [Suspend and dry run](/operations/suspend-and-dry-run/) — how to ask an Automation what it would do before letting it do it.
- [Automation API reference](/reference/automation/) — every field of `spec` and `status`, its type, and what it accepts.
