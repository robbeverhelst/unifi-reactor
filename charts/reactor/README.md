# reactor

Helm chart for [UniFi Reactor](https://github.com/robbeverhelst/unifi-reactor) — an operator that reacts to observed UniFi state (WAN failover, …) with declarative actions on your cluster.

## Install

```bash
kubectl create namespace reactor-system
kubectl -n reactor-system create secret generic unifi-reactor-credentials \
  --from-literal=UNIFI_API_KEY=<your UniFi API key>

helm install reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system \
  --set unifi.url=https://192.168.1.1
```

Create an API key in the UniFi UI under Settings → Control Plane → Integrations.

## What this has been tested against

UniFi Network **10.5.67** on a **UDM Pro** with a **UniFi UPS 2U**, and Kubernetes 1.25+ (CI runs
an envtest 1.36 API server and the current kind default node image). The full matrix, including
what is expected to work rather than verified, is in the
[project README](https://github.com/robbeverhelst/unifi-reactor#compatibility).

Reactor logs the UniFi Network version and the Kubernetes version it finds at startup, and warns
— without refusing to start — when the console is outside that range.

## The CRD

The `Automation` CRD is a **template** in this chart rather than a file under `crds/`. Helm installs a chart's `crds/` directory on first install and never touches it again on upgrade, silently — so a release that changed the schema would upgrade cleanly while the cluster kept the old CRD, and the operator would start writing fields the API server rejects.

As a template it upgrades with everything else. It carries `helm.sh/resource-policy: keep`, so `helm uninstall` leaves both the CRD and every `Automation` stored under it in place. Removing it is a deliberate act:

```bash
kubectl delete crd automations.reactor.robbeverhelst.com   # also deletes every Automation
```

### Upgrading from chart 0.3.0 or earlier

Those versions installed the CRD through `crds/`, which Helm does not record as part of the release. The first upgrade to a chart that templates it fails with `invalid ownership metadata` until the existing CRD is handed over to the release:

```bash
kubectl label crd automations.reactor.robbeverhelst.com \
  app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate crd automations.reactor.robbeverhelst.com \
  meta.helm.sh/release-name=reactor \
  meta.helm.sh/release-namespace=reactor-system --overwrite
```

Use your own release name and namespace. Nothing is deleted or recreated — the CRD stays live, and your Automations with it.

### Managing the CRD outside the release

Set `crds.install=false` where an admin or a GitOps controller owns CRDs. Apply the CRD **before** upgrading the release, so the schema is never older than the operator expecting it:

```bash
kubectl apply -f https://raw.githubusercontent.com/robbeverhelst/unifi-reactor/v<chart-version>/config/crd/bases/reactor.robbeverhelst.com_automations.yaml

helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --set crds.install=false
```

## First Automation

```yaml
apiVersion: reactor.robbeverhelst.com/v1alpha1
kind: Automation
metadata:
  name: pause-qbittorrent-on-backup-wan
  namespace: media          # Automations live next to what they act on
spec:
  when:
    provider: unifi
    state:
      wan: backup
  actions:
    - type: kubernetes.scale
      target: {kind: Deployment, name: qbittorrent}
      replicas: 0
  onExit:
    - type: kubernetes.scale
      target: {kind: Deployment, name: qbittorrent}
      replicas: 1
```

## Sharing a target between Automations

A workload's replica count is arbitrated across every Automation referencing
it: while any of their conditions hold, the target sits at the **most
restrictive** count asked for, and it is restored only once none of them do.
`spec.onExit` therefore declares what an Automation *wants* once nothing claims
the target, not a list run the moment its own condition ends.

Omit `onExit` and the target is restored to its **baseline** — what it was set
to before Reactor first claimed it. Reactor records that on the Deployment:

```yaml
metadata:
  annotations:
    reactor.robbeverhelst.com/baseline-replicas: "1"
    reactor.robbeverhelst.com/claimed-by: "media/shed-on-battery"
    reactor.robbeverhelst.com/claimed-at: "2026-08-13T02:41:07Z"
```

The annotations are removed when the last claim is released, after which
Reactor stops asserting a value for that workload entirely. Set
`spec.reversal: None` to have an Automation leave its targets wherever they
were left instead.

If Flux or Argo CD manages a target Deployment, exclude `spec.replicas` and the
`reactor.robbeverhelst.com` annotations from its drift detection, or the two
controllers will fight over the workload.

## Uninstalling

Helm does not delete the `Automation` CRD or your `Automation` resources on
uninstall — they survive and simply stop reconciling. Anything Reactor had
scaled down would therefore stay down forever, so a `pre-delete` hook Job
releases every claim first and removes the finalizers that nothing would be
left to service.

The Job stops the operator before releasing anything. Helm removes the
release's own resources only once its pre-delete hooks have finished, so a
controller still running would simply re-claim what the hook released, and
re-add the finalizer — turning a later `kubectl delete crd` into a hang.

```sh
helm uninstall reactor -n reactor-system              # workloads restored
helm uninstall reactor -n reactor-system --no-hooks   # skip it, leave them as they are
```

If the hook fails the uninstall fails; re-run with `--no-hooks` to proceed.
Workloads keep their `baseline-replicas` annotation either way, so their
pre-Reactor value is always recoverable by hand.

Deleting an Automation while the controller is down leaves its finalizer with
nothing to release it:

```sh
kubectl patch automation <name> -n <namespace> \
  --type=merge -p '{"metadata":{"finalizers":[]}}'
```

## Pod Security

The controller pod satisfies the **`restricted`** Pod Security Standard with no exemptions — it sets `runAsNonRoot`, `seccompProfile: RuntimeDefault`, drops all capabilities, and runs with `allowPrivilegeEscalation: false` and a read-only root filesystem. You can label its namespace accordingly without any trial and error:

```sh
kubectl label namespace reactor-system \
  pod-security.kubernetes.io/enforce=restricted
```

Note that the operator patches *other* workloads' Deployments. If a target namespace enforces or warns on `restricted` and the target's own pod spec doesn't comply, the API server returns an admission warning on the otherwise successful patch. That warning describes the target, not Reactor; it is logged under the `target-warning` logger at debug level so it can't be mistaken for a failed action.

## State keys

The UniFi provider publishes these keys; each is only present when the matching
hardware is adopted by the controller.

| Key | Values | Source |
| --- | --- | --- |
| `wan` | `primary`, `backup` | which uplink the gateway is using |
| `isp` | a slug (`telenet`), or `unknown` | the carrier behind the live uplink |
| `ups` | `online`, `on-battery` | whether a UniFi UPS is on mains or battery |
| `ups.battery` | `normal`, `low`, `critical` | remaining charge vs. the configured thresholds |

`isp` is the only key with an open value set — it is your carrier's name, lowercased with
non-alphanumerics turned into hyphens. Read it off a state transition line before matching on it.
It is also Reactor's cross-check on `wan`: the two are independent answers to "did the uplink
change", and when only one of them moves, Reactor logs the disagreement instead of picking a
winner.

`ups` and `ups.battery` are independent on purpose: an automation matching
`ups: on-battery` stays matched for the whole outage as the battery drains,
so its `onExit` actions cannot fire mid-outage. Match both keys to react to an
escalation:

```yaml
  when:
    provider: unifi
    state:
      ups: on-battery
      ups.battery: critical    # all keys must match
```

## Webhook fast path (optional, off by default)

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

## Outbound actions

`http.request`, the `notification.*` types and the named integrations (`homeassistant.service`) send a request out of the cluster. All of them are **off until you say where they may go**, and all of them go through one client, so this one value covers every current and future outbound action type:

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

**Why this is a chart value and not an Automation field.** Anyone who can create an `Automation` in their own namespace can ask Reactor to make a request, and it goes out from inside the cluster with the operator's network position rather than theirs — reaching `ClusterIP` Services, your gateway, and whatever else this pod can route to. Which destinations that is worth is a cluster decision, so it lives here. [SECURITY.md](../../SECURITY.md#outbound-actions) has the full threat model.

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

### Creating a credential Secret

For every notification transport, the webhook URL *is* the credential, so it lives in the Secret rather than in the `Automation`:

```bash
kubectl -n media create secret generic ntfy-credentials \
  --from-literal=url=https://ntfy.example.com/your-topic \
  --from-literal=authorization="Bearer tk_example"
```

| Key | Used for |
| --- | --- |
| `url` | The destination. Required for `notification.*`; an alternative to `request.url` for `http.request` and to `homeAssistant.url` for `homeassistant.service` |
| `authorization` | Sent as the `Authorization` header. This is where a Home Assistant long-lived access token goes, as `Bearer <token>` |
| `header-<Name>` | Sent as the header `<Name>`, e.g. `header-X-Api-Key` |

The Secret must live in the `Automation`'s own namespace — there is no namespace field on `secretRef`, on purpose. Nothing from it is logged, put in status, or attached to an `Event`; a destination is only ever reported as `scheme://host:port`.

## Operations

### Log level

`log.level` takes `debug`, `info` (the default), `error`, or a V-level number. `debug` turns on the per-observation lines — what each poll saw, and why a transition did or did not happen — which is what you want when an automation did not fire:

```bash
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set log.level=debug
```

`log.format: json` switches the encoder for a log collector.

Charts up to 0.3.0 hardcoded the manager's arguments and ran at debug; the default is now `info`. `--set log.level=debug` restores the previous output.

### Rotating the UniFi API key

The key is mounted from `unifi.existingSecret` and re-read on **every poll**, so rotation takes effect on its own — no restart, no second controller, nothing for anyone to remember:

```bash
kubectl -n reactor-system create secret generic unifi-reactor-credentials \
  --from-literal=UNIFI_API_KEY=<new key> \
  --dry-run=client -o yaml | kubectl -n reactor-system apply -f -
```

The kubelet refreshes the mounted file within its sync period (about a minute by default) and the next poll authenticates with the new key. Revoke the old key in the UniFi UI once you see polling continue. If the file is ever unreadable or empty, that poll fails and is logged; the next one retries.

If you would rather have the pod restart on change — because you already run [reloader](https://github.com/stakater/Reloader), say — annotate the Deployment instead:

```yaml
annotations:
  secret.reloader.stakater.com/reload: unifi-reactor-credentials
```

### PodDisruptionBudget

Off by default, because with one replica a budget cannot protect anything: `minAvailable: 1` turns a node drain into a hang. Run two replicas and enable it — leader election keeps exactly one instance acting, so the second is a warm standby:

```yaml
replicaCount: 2
podDisruptionBudget:
  enabled: true
  minAvailable: 1
```

### NetworkPolicy

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

## Events

Every transition, every write to a target, every action that failed is raised as
a Kubernetes Event on the Automation, so `kubectl describe automation` tells the
story of a failover without anyone needing log access:

```sh
kubectl -n media describe automation pause-qbittorrent-on-backup-wan | tail -20
```

Nothing to enable. Two things are worth knowing.

**The RBAC changed in this release.** Reactor records through the
**`events.k8s.io/v1`** API, not the deprecated core one. They share storage but
are separate API groups for authorization, so the previous rule — which named
only `apiGroups: [""]` — was refused on every emission. The refusal is logged by
the event broadcaster inside the controller and surfaced nowhere else, so the
symptom was an Automation with no Events at all, which reads as nothing having
happened. Upgrading fixes it; hand-written RBAC copied from an older chart needs
the same change:

```sh
kubectl auth can-i create events.events.k8s.io --namespace media \
  --as system:serviceaccount:reactor-system:reactor
```

**Events fire on edges, not on states.** A reconcile happens at least every 15
seconds, so anything raised from a steady condition would be an API write every
15 seconds per Automation. A target already at the right value produces nothing;
a condition still reporting the same reason produces nothing after the first;
`ActionFailed` stops at the retry budget. An old timestamp on a Warning means
"still true since then", not "stale" — `status.conditions` is what is true now.

`Normal` and `Warning` are used deliberately rather than as a severity dial.
Being outvoted by a more restrictive claim is `Normal`, because that is how two
Automations sharing a workload are meant to behave.

## Metrics, alerts and a dashboard (optional, off by default)

Reactor's worst failure mode is silent: if it stops observing, every Automation
quietly stops reacting and nothing in the cluster notices. `metrics.enabled`
is what makes that visible.

```sh
helm upgrade reactor ... \
  --set metrics.enabled=true \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.rules.enabled=true
```

It is off by default because enabling it opens a port on a running pod and
creates a Service, and an upgrade should never do either on its own. The
manifest bundle (`install.yaml`) turns it on; the chart renders exactly the
same shape, so the only difference between the two paths is the default.

### The auth posture

The endpoint serves **HTTPS on `:8443` behind the API server's authn/authz
filter**, which is what the kubebuilder scaffold has always done and what
`install.yaml` ships. A scraper must present a bearer token belonging to a
ServiceAccount that is allowed to `get` the `/metrics` non-resource URL.

Enabling metrics therefore grants the operator `create` on `tokenreviews` and
`subjectaccessreviews` — that is how it asks the API server to authenticate and
authorize each scrape. Both are cluster-scoped, so those rules are a ClusterRole
in both `rbac.clusterWide` modes.

Both reviews are cluster-scoped resources with no namespaced form, so those
rules are a **ClusterRole even under `rbac.clusterWide: false`** — the one place
a namespace-scoped install still creates cluster-scoped RBAC. With metrics off,
or with `metrics.secure: false`, a namespace-scoped install creates nothing
cluster-scoped at all. What is granted is delegation — the right to ask the API
server whether a caller is allowed in — not access to anything that caller
holds.

The chart creates a `<release>-metrics-reader` ClusterRole and **deliberately
does not bind it**. It cannot know which ServiceAccount your Prometheus scrapes
with, and a binding to the wrong one looks granted and is not:

```sh
kubectl create clusterrolebinding reactor-metrics-reader \
  --clusterrole=reactor-metrics-reader \
  --serviceaccount=monitoring:prometheus-k8s
```

The controller generates a self-signed certificate for the endpoint unless
`--metrics-cert-path` points at a real one, so the ServiceMonitor ships with
`insecureSkipVerify: true`. Issue a certificate and set
`metrics.serviceMonitor.serverName` to turn verification on. Setting
`metrics.secure: false` serves plain HTTP instead, which only makes sense when
something else already controls who can reach the pod.

`metrics.serviceMonitor.labels` is optional on purpose: a Prometheus whose
`serviceMonitorSelector` is `{}` scrapes every ServiceMonitor and needs none of
them.

### What is measured

The decision layer — what Reactor observed, what matched, what it did, and how
fast. Raw UniFi telemetry is deliberately **not** re-exported; a UniFi exporter
covers that better, and duplicating it would only produce two numbers to
disagree about. Reconcile counts, workqueue depth and reconcile latency come
from controller-runtime's own `controller_runtime_*` series on the same
endpoint.

The metric worth alerting on above all others:

```promql
time() - reactor_last_observation_timestamp_seconds > 90
```

`reactor_actions_total` carries a `kind` label of `desired_state` or `edge`, and
no alert should be written without it. A failed `kubernetes.scale` means the
cluster is not in the state you asked for; a failed notification means nobody
was told, while the workload was still scaled and the Automation is still
`Ready`.

### Cardinality

`reactor_state_info{provider,key,value}` is published **only for state keys
whose value set the provider declares closed** — `wan`, `ups`, `ups.battery`.
`isp` is not one of them: its values are carrier slugs derived from whatever
public address your gateway holds, so labelling by them would add one permanent
time series per carrier ever seen. `reactor_state_transitions_total` is not
labelled by `from`/`to` for the same reason.

Nothing is lost: what a key currently holds is in the Automation's
`status.observedState` and in an Event on the resource. Prometheus keeps the
counts; Kubernetes keeps the detail.

The `namespace`/`name` labels on `reactor_automation_*` are safe for a different
reason — a series appears only when someone writes another Automation, and a
deleted one's series are dropped rather than left reporting forever.

### Alert rules

`metrics.rules.enabled` ships a `PrometheusRule` (needs the Prometheus Operator
CRD). `metrics.rules.observationStaleSeconds` defaults to `90`, three times the
default `unifi.pollInterval` — **raise it if you raise that**, or a slower poll
will page you.

| Alert | Severity | Means |
| --- | --- | --- |
| `ReactorObservationStale` | critical | Reactor has gone blind; every Automation has silently stopped reacting |
| `ReactorObservationAbsent` | critical | it is publishing no observations at all, or is not running |
| `ReactorObservationFailing` | warning | sustained poll errors — the warning before the two above |
| `ReactorActionFailing` | warning | an Automation matched but could not act on its target |
| `ReactorEdgeActionFailing` | warning | a notification or HTTP call did not go out; the Automation is unaffected |
| `ReactorAutomationNotReady` | warning | invalid config, a missing state key, or a failed action |
| `ReactorReactionSlow` | warning | p95 observation-to-action above `metrics.rules.reactionLatencySeconds` |
| `ReactorUPSOnBattery` | info | the UPS is running on battery |
| `ReactorWANOnBackup` | info | the gateway is on its backup uplink |

The last two fire on observed state rather than on a fault: they are how your
existing alerting learns what your network already knows. Set
`metrics.rules.informational: false` to leave them out.

### Dashboard

`metrics.dashboard.enabled` ships a grafana-operator `GrafanaDashboard`. It pins
no datasource — the dashboard carries a `datasource` variable you pick when you
open it — and no folder or tag specific to any organisation, so the same JSON
imports into any Grafana unedited. It is a plain file in the chart at
`dashboards/reactor.json` if you would rather import it by hand.

`metrics.dashboard.instanceSelector` decides which Grafana instances
grafana-operator installs it into; it defaults to
`matchLabels: {dashboards: grafana}`, the operator's own convention.

All three of `serviceMonitor`, `rules` and `dashboard` refuse to render without
`metrics.enabled`, rather than shipping something that queries series nothing is
publishing — which fails as silence rather than as an error.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `crds.install` | `true` | install and upgrade the `Automation` CRD with the release; `false` when you manage it yourself |
| `unifi.url` | `""` | UniFi console base URL (required to enable the provider) |
| `unifi.site` | `default` | UniFi Network site |
| `unifi.pollInterval` | `30s` | WAN state poll interval (polling is the source of truth) |
| `unifi.insecureSkipVerify` | `true` | Accept the console's self-signed certificate |
| `unifi.existingSecret` | `unifi-reactor-credentials` | Secret containing `UNIFI_API_KEY`, mounted and re-read per poll |
| `log.level` | `info` | `debug`, `info`, `error`, or a V-level number |
| `log.format` | `console` | `console` or `json` |
| `metrics.enabled` | `false` | Serve `/metrics`; see [Metrics, alerts and a dashboard](#metrics-alerts-and-a-dashboard-optional-off-by-default) |
| `metrics.secure` | `true` | HTTPS behind the API server's authn/authz filter; `false` serves plain HTTP |
| `metrics.port` | `8443` | Port the endpoint listens on inside the pod |
| `metrics.service.enabled` | `true` | Create a ClusterIP Service for it |
| `metrics.service.port` | `8443` | Service port |
| `metrics.reader.create` | `true` | Create an unbound ClusterRole granting `get` on `/metrics` |
| `metrics.serviceMonitor.enabled` | `false` | Scrape it with the Prometheus Operator |
| `metrics.serviceMonitor.interval` | `""` | Empty inherits the Prometheus instance's default |
| `metrics.serviceMonitor.labels` | `{}` | Optional — a `serviceMonitorSelector` of `{}` needs none |
| `metrics.serviceMonitor.insecureSkipVerify` | `true` | The endpoint's certificate is self-signed unless you issue one |
| `metrics.serviceMonitor.serverName` | `""` | Set alongside a real certificate to verify it |
| `metrics.rules.enabled` | `false` | Ship the alert rules as a `PrometheusRule` |
| `metrics.rules.observationStaleSeconds` | `90` | 3 × `unifi.pollInterval`; raise it if you raise that |
| `metrics.rules.reactionLatencySeconds` | `60` | p95 observation-to-action above which reacting is too slow |
| `metrics.rules.informational` | `true` | Include `ReactorUPSOnBattery` and `ReactorWANOnBackup` |
| `metrics.rules.labels` | `{}` | Extra labels, for a Prometheus that selects on them |
| `metrics.dashboard.enabled` | `false` | Ship the dashboard as a grafana-operator `GrafanaDashboard` |
| `metrics.dashboard.instanceSelector` | `{matchLabels: {dashboards: grafana}}` | Which Grafana instances to install it into |
| `metrics.dashboard.folder` | `""` | Grafana folder; empty means the instance's default |
| `podDisruptionBudget.enabled` | `false` | see [PodDisruptionBudget](#poddisruptionbudget) before enabling with one replica |
| `podDisruptionBudget.minAvailable` | `1` | set this or `maxUnavailable`, not both |
| `networkPolicy.enabled` | `false` | deny all ingress and apply `networkPolicy.egress` |
| `networkPolicy.ingress` | `[]` | extra ingress rules |
| `networkPolicy.egress` | all IPv4 | narrow to DNS, the API server, and your console |
| `annotations` | `{}` | annotations on the Deployment (e.g. reloader) |
| `podAnnotations` | `{}` | annotations on the pod |
| `unifi.ups.lowBatteryPercent` | `30` | charge at or below this reports `ups.battery: low` |
| `unifi.ups.criticalBatteryPercent` | `10` | charge at or below this reports `ups.battery: critical` |
| `unifi.debounce.default` | `1` | consecutive observations a changed value needs before Reactor acts; each extra sample costs one `pollInterval` of reaction time |
| `unifi.debounce.keys` | `{ups.battery: 2, isp: 2}` | per-key overrides for signals that settle rather than switch |
| `unifi.webhook.enabled` | `false` | Run the webhook receiver; a delivery triggers a poll, never a state change |
| `unifi.webhook.port` | `9090` | Port the receiver listens on inside the pod |
| `unifi.webhook.path` | `/webhooks/unifi` | URL path deliveries are accepted on |
| `unifi.webhook.existingSecret` | `unifi-reactor-webhook` | Secret containing the shared secret |
| `unifi.webhook.tokenKey` | `UNIFI_WEBHOOK_TOKEN` | Key in that Secret |
| `unifi.webhook.minObserveInterval` | `500ms` | Floor between observations, so a delivery burst is not a request burst |
| `unifi.webhook.service.enabled` | `true` | Create a Service for the receiver |
| `unifi.webhook.service.type` | `ClusterIP` | **Not reachable from your console**; see [Exposing it](#exposing-it) |
| `unifi.webhook.service.port` | `9090` | Service port |
| `unifi.webhook.service.annotations` | `{}` | Service annotations (MetalLB pools, cloud LB hints, …) |
| `unifi.webhook.service.loadBalancerIP` | `""` | Address to request when `type: LoadBalancer` |
| `unifi.webhook.registration.enabled` | `false` | Let Reactor create its own Alarm Manager rule on the console |
| `unifi.webhook.registration.publicURL` | `""` | URL the console should POST to (required when registering) |
| `unifi.webhook.registration.ruleTitle` | `unifi-reactor` | Title of the rule Reactor creates and recognizes |
| `unifi.webhook.registration.existingSecret` | `unifi-reactor-console` | Secret containing `UNIFI_USERNAME` and `UNIFI_PASSWORD` |
| `actions.allowedDestinations` | `[]` | Where outbound actions may go. Empty refuses all of them; see [Outbound actions](#outbound-actions) |
| `uninstall.releaseClaims` | `true` | Run a pre-delete Job that hands every held workload back before the operator is removed |
| `uninstall.timeoutSeconds` | `120` | Hard bound on that Job, so a stuck release delays rather than blocks the uninstall |
| `rbac.clusterWide` | `true` | `false` restricts the operator to watching and acting on the release namespace only (cross-namespace `target.namespace` stops working, and Automations elsewhere are not reconciled at all) |
| `image.repository` | `ghcr.io/robbeverhelst/unifi-reactor` | Manager image |
| `image.tag` | chart `appVersion` | Image tag |
