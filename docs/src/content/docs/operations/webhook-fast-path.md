---
title: "Webhook fast path"
description: "UniFi Alarm Manager can post to Reactor and cut reaction time from a poll interval to about a second. A delivery only ever triggers a poll, so a forged one costs one request."
---

## Webhook fast path

Reactions are normally no faster than `unifi.pollInterval`. UniFi's Alarm Manager can post to Reactor instead, cutting that to about a second — and Reactor can create that Alarm Manager rule itself, rather than asking you to click through the UniFi UI.

It is off by default and stays an optimization. A delivery **triggers a poll**; it never sets state. Its payload is not parsed at all, so a delivery that is dropped, duplicated, replayed or forged costs at most one extra request to your console. Every delivery must present a shared secret, the receiver is not exposed outside the cluster unless you expose it, and self-registration fails soft — if the console does not behave as expected, Reactor logs why and carries on polling.

See the [chart reference](/operations/webhook-fast-path/) for the values, how to make the receiver reachable from your console, and what is worth knowing before turning self-registration on.

Reaction latency is normally bounded by `unifi.pollInterval`. UniFi's Alarm Manager can post to
Reactor instead, and Reactor then re-observes immediately.

A delivery only ever **triggers a poll**. Its payload is never read and can never set state, so a
delivery that is dropped, duplicated, replayed or forged costs at most one extra request to your
console. Polling stays the source of truth; leaving this off costs latency and nothing else.

```sh
kubectl -n reactor-system create secret generic unifi-reactor-webhook \
  --from-literal=UNIFI_WEBHOOK_TOKEN="$(openssl rand -hex 32)"

helm upgrade reactor ... --set unifi.webhook.enabled=true
```

Every delivery must present that secret, as `Authorization: Bearer <token>` or
`X-Reactor-Token: <token>`. A receiver with no secret configured rejects everything rather than
accepting everything.

It composes with `unifi.debounce` rather than overriding it. A delivery can bring an observation
forward, but it cannot supply the consecutive samples a debounced key needs — those always come
from `unifi.pollInterval`. So a key you asked to settle still settles at the pace you asked for,
and no one who can reach the endpoint can hurry it along.

### Exposing it

**The receiver is not reachable from your console by default.** Enabling it creates a ClusterIP
Service, and your UniFi gateway is not in the cluster. Reaching it is a deliberate choice:

| How | Values |
| --- | --- |
| LoadBalancer on a LAN-routable address | `unifi.webhook.service.type=LoadBalancer`, `unifi.webhook.service.loadBalancerIP=<address>` |
| NodePort | `unifi.webhook.service.type=NodePort` |
| Your own Ingress | leave it a ClusterIP and point an Ingress at `<release>-webhook:9090` |
| Not at all | `unifi.webhook.service.enabled=false` — useful when something else fronts the pod |

The chart ships no Ingress: TLS, class and annotations vary too much for a default to be right.

With `replicaCount > 1` the endpoint answers on every replica, but only the leader polls, so a
delivery landing on a standby is accepted and dropped — the same cost as a missed delivery.

### Letting Reactor register its own rule

Reactor can create the Alarm Manager rule itself instead of you creating it in the UniFi UI:

```sh
kubectl -n reactor-system create secret generic unifi-reactor-console \
  --from-literal=UNIFI_USERNAME=<local console account> \
  --from-literal=UNIFI_PASSWORD=<password>

helm upgrade reactor ... \
  --set unifi.webhook.enabled=true \
  --set unifi.webhook.registration.enabled=true \
  --set unifi.webhook.registration.publicURL=http://<reachable address>:9090/webhooks/unifi
```

This is a second, separate credential: the Alarm Manager API sits at the UniFi OS layer and rejects
the API key the poller uses.

Worth knowing before switching it on:

- It writes to your gateway over an **undocumented, reverse-engineered, version-fragile** API
  ([notes](https://github.com/robbeverhelst/unifi-reactor/blob/main/docs/unifi-alarm-manager-api.md)),
  verified against UniFi Network 10.5.67 only.
- It **only ever creates** its rule. It never edits or deletes one, because those verbs were never
  confirmed against a real console. If `publicURL` changes later, the stale rule is reported in the
  logs and left for you to remove in the UniFi UI.
- It **fails soft**. Anything unexpected — login refused, the catalog missing the webhook action,
  the console rejecting the body — is logged and abandoned, and polling continues untouched.
- Creating the rule by hand in the UniFi UI is the conservative option, and it works: point a
  Custom Webhook action at the receiver with a bearer token, or with an `Authorization` header in
  the action's custom-headers list.
