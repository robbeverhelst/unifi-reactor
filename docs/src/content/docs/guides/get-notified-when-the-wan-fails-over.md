---
title: "Get a notification when your WAN fails over"
description: "Send a push to ntfy, Discord or Slack from Kubernetes the moment your UniFi gateway switches uplinks — and another when it comes back. The allowlist and Secret it needs first."
---

Your WAN fails over to the 5G backup at 03:00 and nothing tells you. You find
out on Thursday, from the bill.

This is the smallest useful thing Reactor does, and the one that requires you to
configure something: two install-level decisions, one Secret, one Automation.

## What this assumes

- Reactor is installed and `Ready` — [Install](/start/install/).
- You have somewhere to send a message: an [ntfy](https://ntfy.sh) topic, a
  Discord webhook, or a Slack incoming webhook.
- You can run `helm upgrade` against the release, because the allowlist is a
  chart value.

## 1. Allow the destination

**Outbound actions are refused by default.** The allowlist is install
configuration and deliberately not an Automation field:

```yaml
# values.yaml
actions:
  allowedDestinations:
    - https://ntfy.example.com
    - https://discord.com
```

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values \
  --set 'actions.allowedDestinations={https://ntfy.example.com,https://discord.com}'
```

Worth understanding rather than pasting: anyone who can create an `Automation`
in their own namespace can ask Reactor to make a request, and that request leaves
from inside the cluster with the **operator's** network position rather than
theirs — reaching `ClusterIP` Services, your gateway, and whatever else this pod
can route to. Which destinations that is worth is a cluster decision, so it
lives here.

Entries are matched on scheme, host and port only:

- A path is rejected — `https://ntfy.example.com/alerts` is not a valid entry.
- No port means the scheme's default, so a destination on `:8080` has to be
  written out: `http://hooks.example.com:8080`.
- One leading wildcard label is allowed: `https://*.example.com`.
- `*` on its own allows any host, and is an explicit choice rather than a
  default.

Two things are refused whatever you list: the loopback interface, and link-local
addresses (`169.254.0.0/16`, `fe80::/10`) where cloud instance metadata services
live. Redirects are never followed. Private ranges are **not** blocked — an ntfy
box on the LAN is a legitimate destination, which is exactly why the list is
default-deny.

Setting this adds one RBAC rule — `get` on `secrets`, in whatever scope the
manager already has. Leave the list empty and the permission is not granted at
all.

## 2. Put the destination in a Secret

For every transport shipped, the webhook URL **is** the credential — so a
notification has no `url` field at all. It comes from a Secret in the
Automation's own namespace:

```sh
kubectl -n media create secret generic ntfy-credentials \
  --from-literal=url=https://ntfy.example.com/your-topic \
  --from-literal=authorization="Bearer tk_example"
```

| Secret key | Used for |
| --- | --- |
| `url` | the destination. Required |
| `authorization` | sent as the `Authorization` header. Optional |
| `header-<Name>` | sent as the header `<Name>`, e.g. `header-X-Api-Key`. Optional |

There is deliberately no namespace field on a `secretRef`: an Automation may
only ever read credentials from the namespace it lives in, because anyone who can
create an Automation there can already create a Secret there. Nothing from the
Secret is ever logged, put in status, or attached to an Event.

For Discord or Slack the shape is identical — the incoming webhook URL goes in
`url`, and the action type changes:

```sh
kubectl -n media create secret generic discord-credentials \
  --from-literal=url=https://discord.com/api/webhooks/000000/xxxxxxxx
```

## 3. The Automation

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: notify-on-wan-failover
  namespace: media
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: notification.ntfy
      notification:
        secretRef: { name: ntfy-credentials }
        title: "WAN failed over"
        message: "{{ .Key }} moved from {{ .From }} to {{ .To }} at {{ .Time }}. Uplink is now {{ .State.wan }}."

  onExit:
    - type: notification.ntfy
      notification:
        secretRef: { name: ntfy-credentials }
        title: "WAN recovered"
        message: "{{ .Key }} back to {{ .To }} at {{ .Time }}"
```

Transports shipped: `notification.ntfy`, `notification.discord`,
`notification.slack`. Telegram is not among them — its bot token lives in the
URL path alongside a separate chat id, which does not fit the "the URL is the
credential" shape the others share.

Notifications are **edge** actions: they fire on this Automation's own
transitions, own no target and arbitrate with nothing. An edge action in an
`onExit` block still fires on this Automation's own edge, which is why the
recovery message above works.

### What you can put in a message

`title` and `message` are Go [`text/template`](https://pkg.go.dev/text/template)
— the standard library, no Sprig:

| | |
| --- | --- |
| `.Automation` `.Namespace` `.Name` | who reacted |
| `.Provider` `.Matching` | which provider, and which direction the edge went |
| `.Key` `.From` `.To` | the transition that flipped `matching` |
| `.State` | every key this Automation watches, e.g. `{{ .State.wan }}` |
| `.Time` | when the transition was observed, RFC 3339 |
| `json` | quotes a value for embedding in JSON: `{"wan": {{ json .To }}}` |

**`.State` carries the keys this Automation matches on, and nothing else.** It is
the observed value of every key in `spec.when.state`, so an Automation triggered
on `wan` alone cannot put `{{ .State.isp }}` in its message, however plainly
`isp` shows up in `status.observedState`. That is deliberate: the template
context is incapable of carrying anything the Automation's author did not
already ask for. If you want the carrier in the message, match on it as well.

You do not have to remember this, because Reactor checks it when you apply the
Automation rather than when the message would have gone out:

```sh
kubectl -n media get automation notify-on-wan-failover \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
# spec.actions[0].notification.message references state key "isp", which this automation
# does not match on (it matches on wan); add isp to spec.when.state or remove the reference
```

`Ready=False` with reason `TemplateWillNotRender`, and a Warning Event saying
the same thing. It is a report and not a refusal: the Automation is still
accepted, still evaluated, and still claims and releases its targets — a typo in
a notification does not cost you the failover it was reporting. What it buys is
finding out now instead of at 03:00 in six weeks' time. A typo in a key name
(`{{ .State.wam }}`) and a field that does not exist (`{{ .Uplink }}`) are the
same condition, with the same shape of message.

The check has one hole worth knowing, and it is the same hole the rendering has:
a dotted key needs the `index` builtin — `{{ index .State "ups.battery" }}` —
which returns an empty string for a key that is not there instead of failing, so
neither the check nor the send can tell it went wrong.

Only the message is templated. The URL and the headers are literal on purpose:
the destination is what the allowlist decided, and letting observed state edit
it would hand back exactly the choice the allowlist exists to take away.

## What you will see when it fires

```sh
kubectl -n media describe automation notify-on-wan-failover
```
```text
Type    Reason          Age   From        Message
----    ------          ----  ----        -------
Normal  StateEntered    31s   automation  wan moved from "primary" to "backup", so the condition started holding
Normal  EdgeActionSent  31s   automation  notification.ntfy delivered to https://ntfy.example.com:443 after 1 attempt(s)
```

```sh
kubectl -n media get automation notify-on-wan-failover -o jsonpath='{.status.edgeActions}'
# [{"type":"notification.ntfy","status":"Success","attempts":1,
#   "destination":"https://ntfy.example.com:443","time":"2026-08-15T03:00:12Z"}]
```

`destination` is the **origin only** — scheme, host and port. The path and query
are left out everywhere, because for these transports that is where the
credential is.

`Applied` reports `NoTargets`: "this automation only has edge actions, so it
holds no target". That is `Applied=True` and healthy.

### When it fails

```text
Warning  EdgeActionFailed  8s  automation  notification.ntfy was not delivered: https://ntfy.example.com:443: responded 502 Bad Gateway
```

**A failed notification never fails the Automation.** If the same Automation had
also scaled something, the scale is the thing that had to happen and the
notification is the report of it — so the failure is recorded in
`status.edgeActions`, raised as a Warning Event, and `Ready` stays whatever the
target reconciliation made it.

The delivery rules, stated plainly because they are choices:

- **At most once per transition.** The transition is written to status *before*
  anything is sent, so a failed or conflicting status write cannot send the same
  message twice. Nothing is re-sent on a later reconcile — that reconcile has no
  new transition, so it would be a duplicate rather than a retry.
- **Retries happen inside the one reconcile.** A notification is a publish, so
  it is tried three times against a timeout, a 5xx or a 429.
- **A desired-state action goes first.** A transition whose target could not be
  written is not committed, so nothing announces a workload was paused while it
  is still running.
- **A suspended Automation sends nothing**, the same way a deleted one does not.
- **Nothing fires on deletion.** A "WAN recovered" message caused by a
  `kubectl delete` would be a lie.

If the destination is not on the allowlist, nothing leaves the cluster and the
reason names the fix:

```sh
kubectl -n media get automation notify-on-wan-failover -o jsonpath='{.status.edgeActions[0].reason}'
# outbound actions are disabled on this install: no destination is allowed, so https://ntfy.example.com:443 was refused
```

## What this does not cover

- **No reminders.** Events fire on edges, not on states: you get one message when
  the condition starts holding and one when it stops, not one an hour while the
  outage lasts.
- **No Telegram**, for the reason above. A bot API call is expressible as
  [`http.request`](/actions/notifications-and-http/#httprequest) if you want it.
- **No response handling.** The outbound client drains and discards every
  response body, so nothing a destination replies can reach an Automation beyond
  the status code.
- **No delivery guarantee.** Three attempts inside one reconcile, then it is a
  Warning and a missed message. If you need alerting you can rely on, alert on
  [`reactor_state_info` in Prometheus](/operations/metrics-and-alerts/) instead —
  Reactor exports the same state as metrics, and Alertmanager is built for the
  job a notification action is not.
- **It does not tell you the link is *bad*.** `wan: backup` is a failover.
  `internet: down` and `wan.quality: degraded` are two other questions —
  [WAN and internet state keys](/state-keys/wan-and-internet/).

## Worth knowing about the trigger

`wan` has now been confirmed against a real failover
([#34](https://github.com/robbeverhelst/unifi-reactor/issues/34), closed as
verified): on 2026-08-18 the primary uplink was unplugged, the console failed
over to a cellular backup and back, and `wan` moved with it. One caveat
survives: that failover was resolved by the gateway's own uplink interface
name, because a cellular uplink carries no `is_uplink` field at all — so a
wired-to-wired failover, one wired port taking over from another, remains
unobserved. A notification is still the safest possible place to find out how
your hardware behaves — it writes nothing, and a message that never arrives
costs you an evening rather than a workload.

Which makes this Automation a good first one to install even if you plan to
scale things later: it tells you whether `wan` moves on your hardware before
anything depends on it.

Reactor already keeps a second opinion on this. `isp` — the carrier behind the
live uplink — comes from a different endpoint, and any disagreement between the
two is
[logged rather than resolved](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover).
To get that carrier into the message you have to match on it, since `.State`
only carries the keys in `spec.when.state` — and if you forget, the Automation
says so with `TemplateWillNotRender` rather than waiting for the failover:

```yaml
  when:
    provider: unifi
    state:
      wan: backup
      isp: your-carrier-slug   # now {{ .State.isp }} renders
```

That is a narrower condition, not just a richer message — both keys have to
match. Look your slug up first with
`kubectl -n reactor-system logs deploy/reactor | grep 'key=isp'`.

## Where to go next

- [Notifications and HTTP requests](/actions/notifications-and-http/) — `http.request`, headers, bodies and the full threat model.
- [Home Assistant and qBittorrent actions](/actions/external-services/) — calling a service instead of sending a message.
- [Events and status](/operations/events/) — reading the whole incident with `kubectl describe`.
- [Automation API reference](/reference/automation/) — every field of `notification` and `request`.
- [Chart values reference](/reference/values/) — `actions.allowedDestinations` and what it grants.
