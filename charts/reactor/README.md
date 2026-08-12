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

## Values

| Key | Default | Description |
| --- | --- | --- |
| `unifi.url` | `""` | UniFi console base URL (required to enable the provider) |
| `unifi.site` | `default` | UniFi Network site |
| `unifi.pollInterval` | `30s` | WAN state poll interval (polling is the source of truth) |
| `unifi.insecureSkipVerify` | `true` | Accept the console's self-signed certificate |
| `unifi.existingSecret` | `unifi-reactor-credentials` | Secret containing `UNIFI_API_KEY` |
| `rbac.clusterWide` | `true` | `false` restricts the operator to the release namespace (cross-namespace `target.namespace` stops working) |
| `image.repository` | `ghcr.io/robbeverhelst/unifi-reactor` | Manager image |
| `image.tag` | chart `appVersion` | Image tag |
