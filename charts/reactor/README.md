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
| `ups` | `online`, `on-battery` | whether a UniFi UPS is on mains or battery |
| `ups.battery` | `normal`, `low`, `critical` | remaining charge vs. the configured thresholds |

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
| `unifi.debounce.keys` | `{ups.battery: 2}` | per-key overrides for signals that settle rather than switch |
| `uninstall.releaseClaims` | `true` | Run a pre-delete Job that hands every held workload back before the operator is removed |
| `uninstall.timeoutSeconds` | `120` | Hard bound on that Job, so a stuck release delays rather than blocks the uninstall |
| `rbac.clusterWide` | `true` | `false` restricts the operator to the release namespace (cross-namespace `target.namespace` stops working) |
| `image.repository` | `ghcr.io/robbeverhelst/unifi-reactor` | Manager image |
| `image.tag` | chart `appVersion` | Image tag |
