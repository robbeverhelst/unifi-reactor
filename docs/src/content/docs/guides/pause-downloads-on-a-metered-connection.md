---
title: "Pause downloads when Kubernetes falls back to a metered connection"
description: "Your WAN fails over to 5G and qBittorrent keeps saturating it. Two ways to stop that from Kubernetes — scaling the Deployment, or pausing the torrents — and which one to actually pick."
---

The uplink drops, the gateway fails over to the 5G backup, and the download
client in your cluster carries on at 40Mbit against a metered SIM. Nothing in
Kubernetes knows the connection changed underneath it.

Reactor knows, because the console does. There are two ways to react and they
are not equally good — but the better one is not the one you should always
reach for, and the reason is worth two minutes.

## What this assumes

- Reactor is installed and `Ready` — [Install](/start/install/).
- Your gateway has a second uplink and reports `wan` — a console with one WAN
  publishes the key but it never moves off `primary`.
- qBittorrent runs in the cluster as a `Deployment`.

## The crude answer: scale the Deployment to zero

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
```

That is the whole thing. No Secret, no allowlist, no credential, no outbound
request — Reactor writes to the API server it is already authenticated to. With
`onExit` omitted the replica count is restored from
`reactor.robbeverhelst.com/baseline-replicas`, recorded on the Deployment before
the first claim.

It is also blunt: it kills the container, drops every in-progress peer
connection, and relies on qBittorrent recovering its session from disk when it
comes back.

## The better answer: pause the torrents

Pausing is what you actually wanted. Traffic stops, session state is preserved,
resume is instant:

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: pause-torrents-on-backup-wan
  namespace: media
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: qbittorrent.pause
      qbittorrent:
        url: http://qbittorrent.media.svc.cluster.local:8080
        secretRef: { name: qbittorrent-credentials }

  onExit:
    - type: qbittorrent.resume
      qbittorrent:
        url: http://qbittorrent.media.svc.cluster.local:8080
        secretRef: { name: qbittorrent-credentials }
```

It needs two things set up first, and both are install-level decisions rather
than Automation ones.

**1. Allow the destination.** Outbound actions are refused by default, including
to a Service inside your own cluster:

```yaml
# values.yaml
actions:
  allowedDestinations:
    - http://qbittorrent.media.svc.cluster.local:8080
```

Entries are matched on scheme, host and **port** only — a path is rejected, and
a non-default port has to be written out, as above. This is a chart value and
not an Automation field on purpose: anyone who can create an Automation in their
own namespace can ask Reactor to make a request, and it leaves with the
operator's network position rather than theirs.

**2. Put the credentials in a Secret**, in the Automation's own namespace:

```sh
kubectl -n media create secret generic qbittorrent-credentials \
  --from-literal=username=reactor \
  --from-literal=password='...'
```

Both are required. qBittorrent issues a session cookie rather than accepting a
static token, and that login round trip is the entire reason this is a named
action instead of an `http.request`. Reactor logs in inside the one action,
holds the cookie in a local variable, and logs out again — there is no session
cache.

## So which one

The pause is better at the job. The scale is better at composing with everything
else you will write, and that is usually what decides it.

| | `kubernetes.scale` | `qbittorrent.pause` |
| --- | --- | --- |
| Arbitrated across Automations | **yes** | no |
| Baseline recorded, restored on release | **yes** | no |
| Handed back when Reactor is uninstalled | **yes**, by the pre-delete sweep | no |
| Needs an allowlist entry and a Secret | no | yes |
| Keeps session state and peer connections | no | **yes** |

**Arbitration is the reason to keep the crude one.** Downloads genuinely should
stop for a metered uplink *and* for a power cut, and those are two Automations
with unrelated conditions. Point both at the same Deployment and nothing has to
be coordinated: the workload sits at the most restrictive level anyone asked for,
and comes back only when **neither** wants it down. The one that lost says so:

```sh
kubectl -n media get automation pause-downloads-on-backup-wan -o jsonpath='{.status.targets[0]}'
# {"ref":"Deployment/media/qbittorrent","desired":1,"effective":0,
#  "deferredBy":["media/shed-load-on-battery"]}
```

Two `qbittorrent.pause` Automations do not do that. Each fires on its own
transition and **whichever resumes first resumes everything** — including
torrents you had paused by hand before Reactor ever ran, because nothing
recorded which those were.

That is not an oversight, it is a stated limit. What makes arbitration possible
is not the fold: it is that the target is a Kubernetes object, so the value it
held before Reactor claimed it can be written as an annotation *on that object*,
where it outlives the Automation and Reactor itself. A qBittorrent instance
reached over HTTP has nowhere to put one. [The two shapes an action
has](/concepts/levels-and-occurrences/) is the general form of this;
[Home Assistant and qBittorrent actions](/actions/external-services/) has the
three alternatives that were considered and rejected.

**You can have both.** Pause on the way in for the graceful stop, and let a
`kubernetes.scale` claim the Deployment for the cases where the fold matters —
the scale still arbitrates normally, because a Deployment is still a Deployment.

## Matching the carrier instead of the port

`wan: backup` says which uplink is selected. If what you actually mean is "we
are on the metered SIM", `isp` says who is carrying the traffic:

```yaml
  when:
    provider: unifi
    state:
      isp: your-carrier-slug
```

`isp` is the one key whose values are not a closed set — it is the carrier your
console geolocated your public address to, lowercased with everything
non-alphanumeric turned into a hyphen. Look yours up before matching on it:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'key=isp'
# INFO state transition provider=unifi key=isp from= to=example-telecom
```

It is debounced at 2 samples, because a geolocation lookup on a freshly assigned
address can report `unknown` for a poll or two — which is exactly during a
failover.

## What you will see when it fires

```sh
kubectl -n media describe automation pause-torrents-on-backup-wan
```
```text
Type    Reason          Age   From        Message
----    ------          ----  ----        -------
Normal  StateEntered    12s   automation  wan moved from "primary" to "backup", so the condition started holding
Normal  EdgeActionSent  12s   automation  qbittorrent.pause applied at http://qbittorrent.media.svc.cluster.local:8080 after 1 attempt(s)
```

```sh
kubectl -n media get automation pause-torrents-on-backup-wan -o jsonpath='{.status.edgeActions}'
# [{"type":"qbittorrent.pause","status":"Success","attempts":1,
#   "destination":"http://qbittorrent.media.svc.cluster.local:8080","time":"..."}]
```

`Applied` reports `NoTargets` — "this automation only has edge actions, so it
holds no target" — and that is `Applied=True`, not a fault. The scaling version
instead reports `TargetHeld`, a `status.targets[]` entry, and the annotations on
the Deployment.

If the destination is not allowed, nothing is sent and the reason says exactly
what to add:

```sh
kubectl -n media get automation pause-torrents-on-backup-wan -o jsonpath='{.status.edgeActions[0].reason}'
# outbound actions are disabled on this install: no destination is allowed, so
# http://qbittorrent.media.svc.cluster.local:8080 was refused
```

## What this does not cover

- **It does not throttle.** There is no rate limit action; downloads are either
  running or not.
- **It is all torrents or none.** No category or tag filter, because narrowing
  would mean reading a response body back into Reactor, and the outbound client
  drains and discards every one — a response can echo a request back,
  credentials included.
- **It does not know your data cap.** `wan: backup` is a link state, not a
  quota. Reactor has no meter.
- **It does not cover other clients.** SABnzbd, a sync daemon, anything with an
  HTTP API: use [`http.request`](/actions/notifications-and-http/#httprequest),
  or scale it.
- **A resume does not un-pause selectively.** See above — there is no baseline.

## One honesty note about `wan`

**A genuine WAN failover has now been observed on real hardware**
([#34](https://github.com/robbeverhelst/unifi-reactor/issues/34), closed as
verified): on 2026-08-18 the primary uplink was unplugged for 75 seconds, the
console failed over to a cellular backup and back, and `wan` moved from
`primary` to `backup` and back to `primary`. One caveat survives it. That
failover was resolved by the gateway's own uplink interface name, because a
cellular uplink's record carries no `is_uplink` field at all — so whether
`is_uplink` moves cleanly when one *wired* port takes over from another is
still unobserved.

What that means for this guide, practically:

- Watch for the
  [disagreement warnings](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover)
  Reactor raises when `wan` and `isp` do not move together — a second console
  is not obliged to fail over the way this one did.
- `internet: down` answers a different question — whether the outside world is
  reachable at all — and comes from the console's own `www` health subsystem
  rather than from `is_uplink`. It is debounced at 3 samples, so about 90s at
  the default poll before an outage or a recovery is believed. `wan` is
  debounced at 1 and reacts on the first observation.
- If you have a gateway with two working *wired* uplinks, the [capture
  runbook](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#capturing-a-real-failover)
  is fifteen minutes that would settle the wired-to-wired case for everyone.

## Where to go next

- [WAN and internet state keys](/state-keys/wan-and-internet/) — `wan`, `internet` and `wan.quality` are three different questions.
- [Home Assistant and qBittorrent actions](/actions/external-services/) — the full reasoning behind the edge-action shape.
- [Arbitration](/concepts/arbitration/) — what a shared workload does.
- [Automation API reference](/reference/automation/) — every field, generated from the types.
- [Chart values reference](/reference/values/) — `actions.allowedDestinations` and the debounce table.
