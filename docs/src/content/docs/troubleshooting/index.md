---
title: "Troubleshooting"
description: "Four commands that identify most problems, and where to go from what they say. Start here before reading any of the failure modes."
---

Reactor observes state and reconciles against it, so almost every problem is one of three things: it cannot see the state, it disagrees with you about what the state means, or it cannot act on the target. This page is ordered by how often each turns out to be the answer.

> Examples below use documentation addresses (`192.0.2.10`) and placeholder names. Substitute your own.

## Start here

Four questions answer most problems, and the first one is the one nothing else can tell you:

```sh
# 0. Is it still observing at all? (needs metrics.enabled — see §13)
#    time() - reactor_last_observation_timestamp_seconds

# 1. What does Reactor think is true?
kubectl -n reactor-system logs deploy/reactor | grep -E 'state (observed|transition)'
# INFO state transition provider=unifi key=wan from=primary to=backup

# 2. What does this Automation think?
kubectl -n media get automation pause-downloads-on-backup-wan -o yaml

# 3. Why is it saying that?
kubectl -n media describe automation pause-downloads-on-backup-wan
```

`state observed` is a debug line and the chart defaults to `info`, so it is usually absent. Turn it on for as long as you are debugging:

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set log.level=debug
```

At the default level you still get every change, because `state transition` logs at INFO whenever a key moves:

```text
INFO state transition provider=unifi key=wan from=primary to=backup
```

`log.format: json` switches the encoder if you are reading these through a log collector rather than by eye.

The status block is the single most useful artifact, and it is what a bug report needs:

```yaml
status:
  matching: true
  observedState:
    wan: backup
  lastTransition: {key: wan, from: primary, to: backup, time: "..."}
  lastExecution: {status: Success, onExit: false, time: "..."}
  conditions:
    - type: Ready
      status: "True"
      reason: Reconciled
```
