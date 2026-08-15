---
title: "RBAC and security"
description: "The Pod Security Standard the controller satisfies, the NetworkPolicy to narrow it to DNS, the API server and your console, and where every permission Reactor asks for is justified."
---

Reactor runs with a dedicated ServiceAccount holding exactly the verbs it needs. Which verbs those are depends on what you turn on, and each one is argued for where the feature that needs it is documented:

| Permission | Granted when | Where it is explained |
| --- | --- | --- |
| `get` on Secrets | `actions.allowedDestinations` is non-empty | [Notifications and HTTP](/actions/notifications-and-http/#install-values-and-the-rbac-they-imply) |
| `list` on HorizontalPodAutoscalers | `safety.detectHPA` is set | [Arbitration](/concepts/arbitration/#turning-hpa-detection-on) |
| `get` and `patch` on Nodes, cluster-scoped | `rbac.allowNodeActions` is set | [Kubernetes actions](/actions/kubernetes/#closing-a-node-to-new-work) |
| `create` on TokenReviews and SubjectAccessReviews | `metrics.enabled` is set | [Metrics and alerts](/operations/metrics-and-alerts/#the-auth-posture-and-the-alerts-that-ship) |
| `create` on `events.k8s.io` Events | always | [Events and status](/operations/events/) |
| write verbs on workload kinds | withheld entirely under `safety.dryRun` | [Suspend and dry run](/operations/suspend-and-dry-run/) |

`rbac.clusterWide: false` restricts the manager to its own namespace; an action targeting another namespace then reports the target as unreachable rather than failing at the API server. The threat model for everything that leaves the cluster is in [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md).

## Pod Security

The controller pod satisfies the **`restricted`** Pod Security Standard with no exemptions — it sets `runAsNonRoot`, `seccompProfile: RuntimeDefault`, drops all capabilities, and runs with `allowPrivilegeEscalation: false` and a read-only root filesystem. You can label its namespace accordingly without any trial and error:

```sh
kubectl label namespace reactor-system \
  pod-security.kubernetes.io/enforce=restricted
```

Note that the operator patches *other* workloads' Deployments. If a target namespace enforces or warns on `restricted` and the target's own pod spec doesn't comply, the API server returns an admission warning on the otherwise successful patch. That warning describes the target, not Reactor; it is logged under the `target-warning` logger at debug level so it can't be mistaken for a failed action.

## NetworkPolicy

Off by default. Enabling it denies all inbound traffic and applies `networkPolicy.egress`, which starts unrestricted so that turning the policy on cannot silently break polling. Narrow it to the three things the operator actually talks to — DNS, the API server, and your console:

```yaml
networkPolicy:
  enabled: true
  egress:
    - to:
        - namespaceSelector:
            matchLabels: { kubernetes.io/metadata.name: kube-system }
      ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
    - to:
        - ipBlock: { cidr: <your API server IP>/32 }
      ports:
        - { protocol: TCP, port: 6443 }
    - to:
        - ipBlock: { cidr: <your UniFi console IP>/32 }
      ports:
        - { protocol: TCP, port: 443 }
```

Denying all inbound is right until you turn the webhook fast path on, at which
point the console does need to reach the receiver. The chart does **not** widen
`networkPolicy.ingress` for you — a policy that opens a port you did not write
there would be a bad surprise — so add the rule yourself. Without it, deliveries
are dropped and Reactor quietly falls back to the poll interval, which is the
one failure here that looks like nothing at all:

```yaml
networkPolicy:
  ingress:
    - from:
        - ipBlock: { cidr: <your UniFi console IP>/32 }
      ports:
        - { protocol: TCP, port: 9090 }
```

The same applies to `metrics.enabled`: Prometheus has to reach the metrics port,
and the chart does not widen `networkPolicy.ingress` for that either.

```yaml
networkPolicy:
  ingress:
    - from:
        - namespaceSelector:
            matchLabels: { kubernetes.io/metadata.name: monitoring }
      ports:
        - { protocol: TCP, port: 8443 }
```
