---
title: "Notifications and HTTP requests"
description: "notification.ntfy, .discord and .slack send a message; http.request calls anything with an HTTP API. What you have to allow first, how messages are templated, and what a failure does not do."
---

## Telling you what happened

Everything above is invisible unless someone is reading controller logs — including the cases where Reactor deliberately did *nothing*, like holding state when the console went quiet. Two action types fix that by leaving the cluster: `notification.*` sends a message, `http.request` calls anything with an HTTP API.

Both are **edge actions**, like [`kubernetes.restart`](/actions/kubernetes/#restarting-a-workload). They fire on this automation's own transitions and own nothing — unlike the desired-state actions, which declare a level that is arbitrated across every automation sharing a target. An edge action in an `onExit` block still fires on this automation's own edge.

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
      target: {kind: Deployment, name: qbittorrent}
      replicas: 0
    - type: notification.ntfy
      notification:
        secretRef: {name: ntfy-credentials}
        title: "Reactor: {{ .Name }}"
        message: "{{ .Key }} moved from {{ .From }} to {{ .To }}; qbittorrent paused"

  onExit:
    - type: kubernetes.scale
      target: {kind: Deployment, name: qbittorrent}
      replicas: 1
    - type: notification.ntfy
      notification:
        secretRef: {name: ntfy-credentials}
        message: "{{ .Key }} back to {{ .To }}; qbittorrent resumed"
```

Transports shipped: `notification.ntfy`, `notification.discord`, `notification.slack`. Telegram is not shipped — its bot token lives in the URL path alongside a separate chat id, which does not fit the "the URL is the credential" shape the others share.

### Two things you have to set up first

**1. Allow the destination.** Outbound actions are refused by default and the allowlist is an install value, not something an automation can set:

```yaml
# values.yaml
actions:
  allowedDestinations:
    - https://ntfy.example.com
    - https://discord.com
```

This is the security boundary and it is worth understanding rather than pasting: anyone who can create an `Automation` in their own namespace can ask Reactor to make a request, and that request goes out from inside the cluster with the operator's network position rather than theirs. [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#outbound-actions) has the reasoning and what is refused whatever you list.

**2. Put the destination in a Secret.** For every transport shipped, the webhook URL *is* the credential — so a notification has no URL field at all:

```sh
kubectl -n media create secret generic ntfy-credentials \
  --from-literal=url=https://ntfy.example.com/your-topic \
  --from-literal=authorization="Bearer tk_example"
```

| Secret key | Used for |
| --- | --- |
| `url` | the destination. Required for `notification.*`; for `http.request`, an alternative to `request.url` |
| `authorization` | sent as the `Authorization` header |
| `header-<Name>` | sent as the header `<Name>`, e.g. `header-X-Api-Key` |

The Secret must be in the automation's own namespace, and nothing from it is ever logged, put in status, or attached to an Event.

### Messages

`title`, `message` and `http.request`'s `body` are Go [`text/template`](https://pkg.go.dev/text/template) — the standard library, no Sprig:

| | |
| --- | --- |
| `.Automation` `.Namespace` `.Name` | who reacted |
| `.Provider` `.Matching` | which provider, and which direction the edge went |
| `.Key` `.From` `.To` | the transition that flipped `matching` |
| `.State` | every key this automation watches, e.g. `{{ .State.wan }}` |
| `.Time` | when the transition was observed, RFC 3339 |
| `json` | quotes a value for embedding in JSON: `{"wan": {{ json .To }}}` |

Only the message and the body are templated. The URL and the headers are literal on purpose — the destination is what the allowlist decided, and letting observed state edit it would hand back exactly the choice the allowlist exists to take away.

A key that does not exist is an error rather than the words `no value`, so a typo fails loudly at the moment the notification would have gone out. That covers `{{ .State.wan }}`; the `index` builtin (which you need for a dotted key, `{{ index .State "ups.battery" }}`) returns an empty string instead.

Values are treated as data, not structure, whatever they contain — which matters most for [`isp`](/state-keys/), the one key whose values are an open set rather than an enum. Notification bodies are built with a JSON encoder rather than by string formatting, `json` is there so an `http.request` body can embed a value without hand-quoting it, and anything travelling in a header is reduced to printable ASCII.

### `http.request`

```yaml
- type: http.request
  request:
    method: POST                       # GET, POST, PUT or PATCH; defaults to POST
    url: https://example.com/hook      # or omit it and put url in the Secret
    secretRef: {name: hook-credentials}
    headers:
      - name: X-Reactor-Source
        value: homelab
    body: '{"automation": {{ json .Automation }}, "wan": {{ json .To }}}'
  timeoutSeconds: 10
```

### When a notification fails

**A failed notification never fails the automation.** The scale is the thing that had to happen; the notification is the report of it. So a failure is recorded in `status.edgeActions` and raised as a Warning `Event`, and `Ready` stays whatever the target reconciliation made it:

```sh
kubectl -n media get automation pause-downloads-on-backup-wan -o jsonpath='{.status.edgeActions}'
# [{"type":"notification.ntfy","status":"Failed","attempts":3,
#   "destination":"https://ntfy.example.com:443",
#   "reason":"https://ntfy.example.com:443: responded 502 Bad Gateway",...}]

kubectl -n media describe automation pause-downloads-on-backup-wan
# Warning  EdgeActionFailed  notification.ntfy was not delivered: ...
```

Ordering and delivery, stated plainly because they are choices rather than accidents:

- **The scale happens first.** A transition whose target could not be written is not committed, so nothing announces a workload was paused while it is still running. It is announced on the retry that succeeds.
- **At most once per transition.** The transition is written to status *before* anything is sent, so a failed or conflicting status write cannot send the same message twice. Nothing is re-sent on a later reconcile — that reconcile has no new transition, so a re-send would be a duplicate, not a retry.
- **Retries happen inside the one reconcile.** A notification is a publish, so it is tried three times against a timeout, a 5xx or a 429. `http.request` is not: `GET` and `PUT` retry ([RFC 9110](https://www.rfc-editor.org/rfc/rfc9110#name-idempotent-methods) calls them idempotent), and `POST` and `PATCH` are attempted exactly once unless you set `request.idempotent: true`. Reactor cannot tell your webhook from your order API, and a duplicate side effect is worse than a missed one when nobody knows what the side effect is.
- **A suspended automation sends nothing**, the same way a deleted one does not. Suspending is a reversible delete.
- **Nothing fires on deletion.** Deleting an automation is not a state transition, and a "WAN recovered" message caused by a `kubectl delete` would be a lie.

## Install values, and the RBAC they imply

```yaml
actions:
  allowedDestinations:
    - https://ntfy.example.com
    - https://discord.com
    - http://hooks.example.com:8080     # a non-default port has to be written out
    - https://*.example.com             # one leading wildcard label
```

With the list empty — the default — every such action is refused, and the Automation says which destination to add:

```bash
kubectl -n media get automation notify-on-failover -o jsonpath='{.status.edgeActions[0].reason}'
# outbound actions are disabled on this install: no destination is allowed, so https://ntfy.example.com:443 was refused
```

**Why this is a chart value and not an Automation field.** Anyone who can create an `Automation` in their own namespace can ask Reactor to make a request, and it goes out from inside the cluster with the operator's network position rather than theirs — reaching `ClusterIP` Services, your gateway, and whatever else this pod can route to. Which destinations that is worth is a cluster decision, so it lives here. [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#outbound-actions) has the full threat model.

Two things are refused regardless of what you list: the loopback interface, and link-local addresses (`169.254.0.0/16`, `fe80::/10`) where cloud instance metadata services live. Redirects are never followed. Private ranges are **not** blocked — an ntfy box on the LAN is a legitimate destination and nothing can tell it apart from a `ClusterIP` Service, which is why the list is default-deny.

### The RBAC this implies

Setting `actions.allowedDestinations` adds one rule to the manager's Role or ClusterRole:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get"]
```

Credentials for outbound actions live in Secrets in the namespace of the `Automation` that references them, so this follows `rbac.clusterWide` like the rest of the manager's rules. Leave the list empty and the permission is not granted at all. Reactor reads these with an uncached client, so no Secret is held in the operator's memory beyond the request that used it.

If you run with `networkPolicy.enabled: true` and a narrowed `networkPolicy.egress`, remember to allow the destinations too — the allowlist is Reactor's own control, not a Kubernetes one.
