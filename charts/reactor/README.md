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

## Values

| Key | Default | Description |
| --- | --- | --- |
| `crds.install` | `true` | install and upgrade the `Automation` CRD with the release; `false` when you manage it yourself |
| `unifi.url` | `""` | UniFi console base URL (required to enable the provider) |
| `unifi.site` | `default` | UniFi Network site |
| `unifi.pollInterval` | `30s` | WAN state poll interval (polling is the source of truth) |
| `unifi.insecureSkipVerify` | `true` | Accept the console's self-signed certificate |
| `unifi.existingSecret` | `unifi-reactor-credentials` | Secret containing `UNIFI_API_KEY` |
| `unifi.ups.lowBatteryPercent` | `30` | charge at or below this reports `ups.battery: low` |
| `unifi.ups.criticalBatteryPercent` | `10` | charge at or below this reports `ups.battery: critical` |
| `rbac.clusterWide` | `true` | `false` restricts the operator to the release namespace (cross-namespace `target.namespace` stops working) |
| `image.repository` | `ghcr.io/robbeverhelst/unifi-reactor` | Manager image |
| `image.tag` | chart `appVersion` | Image tag |
