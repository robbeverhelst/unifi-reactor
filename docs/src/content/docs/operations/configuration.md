---
title: "Configuration"
description: "Every chart value that changes what Reactor observes or how it behaves: poll interval, thresholds, log level, rotating the UniFi API key without a restart, and a PodDisruptionBudget."
---

## Configuration

Chart values ([full reference](https://github.com/robbeverhelst/unifi-reactor/blob/main/charts/reactor/README.md)):

| Value | Default | Description |
| --- | --- | --- |
| `crds.install` | `true` | install and upgrade the `Automation` CRD with the release |
| `crds.adopt` | `true` | on the first upgrade from chart 0.3.0 or earlier, take the CRD that packaging left owned by no release into this one |
| `unifi.url` | — | UniFi console base URL; the provider stays disabled until this is set |
| `unifi.site` | `default` | UniFi Network site |
| `unifi.pollInterval` | `30s` | how often WAN, internet and UPS state are observed |

| `unifi.maxObservationAge` | `""` | how old the observed state may get before every automation reports `ObservationStale` and says so. Empty is unbounded — and silent ([above](/concepts/settling-a-noisy-signal/#how-long-reactor-may-act-on-state-that-has-already-changed)) |
| `unifi.insecureSkipVerify` | `true` | accept the console's self-signed certificate |
| `unifi.existingSecret` | `unifi-reactor-credentials` | Secret holding `UNIFI_API_KEY`; re-read on every poll, so rotating the key needs no restart |
| `log.level` | `info` | `debug` adds the per-observation lines used to work out why an automation did not fire |
| `unifi.ups.lowBatteryPercent` | `30` | charge at or below this reports `ups.battery: low` |
| `unifi.ups.criticalBatteryPercent` | `10` | charge at or below this reports `ups.battery: critical` |
| `unifi.ups.shortRuntimeSeconds` | `600` | remaining runtime at or below this reports `ups.runtime: short` |
| `unifi.ups.criticalRuntimeSeconds` | `180` | remaining runtime at or below this reports `ups.runtime: critical` |
| `unifi.ups.highLoadPercent` | `80` | draw at or above this share of the power budget reports `ups.load: high` |
| `unifi.wan.quality.minAvailabilityPercent` | `99` | availability below this reports `wan.quality: degraded` |
| `unifi.wan.quality.maxLatencyMs` | `150` | average latency above this reports `wan.quality: degraded` |
| `unifi.temperature.highCelsius` | `75` | hottest adopted device at or above this reports `temperature: high` |
| `unifi.poe.maxUtilizationPercent` | `90` | a switch delivering at or above this share of its PoE budget reports `poe: insufficient` |
| `unifi.devices.perDeviceKeys` | `false` | also publish a `device.<name>` key per adopted device — [one more series per device](/state-keys/fleet-and-devices/#the-fleet-devices-and-why-devicename-is-opt-in) |
| `unifi.webhook.enabled` | `false` | webhook fast path (below) |
| `actions.allowedDestinations` | `[]` | where outbound actions may go. Empty refuses all of them, and withholds the operator's read access to Secrets ([why](/actions/notifications-and-http/)) |
| `metrics.enabled` | `false` | serve `/metrics` on `:8443` over HTTPS behind the API server's authn/authz filter ([above](/operations/metrics-and-alerts/)) |
| `metrics.serviceMonitor.enabled` | `false` | scrape it with the Prometheus Operator |
| `metrics.rules.enabled` | `false` | ship the alert rules, `ReactorObservationStale` first |
| `metrics.dashboard.enabled` | `false` | ship the overview dashboard as a grafana-operator `GrafanaDashboard` |
| `rbac.clusterWide` | `true` | when `false`, restricts the operator to its own namespace |
| `safety.dryRun` | `false` | evaluate and report everything, write nothing, and withhold the permissions that could ([above](/operations/suspend-and-dry-run/#asking-what-an-automation-would-do)) |
| `safety.detectHPA` | `false` | notice a HorizontalPodAutoscaler driving a target and decline it rather than fight ([above](/concepts/arbitration/#when-something-else-already-owns-the-workload)) |

`Automation` resources are namespaced. An action targets its own namespace by default; naming a different one in `target.namespace` requires `rbac.clusterWide: true`.

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
