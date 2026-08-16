# reactor

Helm chart for [UniFi Reactor](https://github.com/robbeverhelst/unifi-reactor) — an operator that reacts to observed UniFi state (WAN failover, UPS power loss, device health) with declarative actions on your cluster.

Full documentation: **[reactor.robbeverhelst.com](https://reactor.robbeverhelst.com)**

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

Nothing is written to your cluster until you create an `Automation`. To bring the install up without it writing at all, add `--set safety.dryRun=true` — everything is evaluated and reported, and the chart withholds every permission that could write ([Suspend and dry run](https://reactor.robbeverhelst.com/operations/suspend-and-dry-run/)).

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

Nothing to do — `helm upgrade` is the whole procedure.

Those versions installed the CRD through `crds/`, which Helm applies without recording it as part of the release, so the first upgrade to a chart that templates it meets a CRD owned by nobody and Helm will not touch it. The chart adopts that CRD itself on that one upgrade: a hook Job with its own ServiceAccount and a ClusterRole granting `get` and `patch` on that single CRD name sets the ownership metadata from the release you are installing, and puts the current schema live in the same patch. Nothing is deleted or recreated — the CRD stays live, and your Automations with it.

It is rendered only when there is something to adopt, so a fresh install and every later upgrade carry no Job and no cluster-scoped permission. A CRD belonging to a *different* release is never adopted: that upgrade stops, naming the release that owns it.

Set `crds.adopt=false` to hand the CRD over yourself instead; [the troubleshooting guide](https://reactor.robbeverhelst.com/troubleshooting/rbac-and-crd/#upgrading-from-chart-030-or-earlier) has the two `kubectl` commands, which are also the fallback if the hook ever fails. Until somebody runs them the CRD still belongs to no release, so the upgrade stops there too — with the same two commands in the message.

#### `helm get manifest` shows no CRD after that upgrade

Expected, on that one revision, and only that one.

Helm checks whether it owns an object while it *prepares* an upgrade, before it runs a single hook — so an upgrade that renders a CRD nobody owns fails before the hook that would establish ownership could exist. The chart therefore leaves the CRD out of the release on the adopting upgrade and lets the hook apply the schema instead. `helm get manifest` for that revision lists `serviceaccount.yaml`, `rbac.yaml` and `deployment.yaml` and no `CustomResourceDefinition`, while `helm template` — which has no cluster to look in, so it cannot tell the CRD is unowned — renders one. Both are correct.

The next `helm upgrade` finds the CRD owned, renders it as an ordinary part of the release, and it stays there. Nothing has to be done to make that happen, and nothing is different about a release deployed through Pulumi, Argo CD or Flux: the decision is a `lookup` made at render time, by whichever Helm does the rendering.

### Managing the CRD outside the release

Set `crds.install=false` where an admin or a GitOps controller owns CRDs. Apply the CRD **before** upgrading the release, so the schema is never older than the operator expecting it:

```bash
kubectl apply -f https://raw.githubusercontent.com/robbeverhelst/unifi-reactor/v<chart-version>/config/crd/bases/reactor.robbeverhelst.com_automations.yaml

helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --set crds.install=false
```

## Values

Every value, its default and what it does: **[Chart values](https://reactor.robbeverhelst.com/reference/values/)**, generated from `values.yaml` so it cannot drift from the chart.

[`values.yaml`](values.yaml) itself is the source of truth and is written to be read while you configure — each key carries its reasoning in the comment above it.

## Documentation

Everything explanatory about running this lives on the site rather than here, so there is one copy of it:

| | |
| --- | --- |
| [Configuration](https://reactor.robbeverhelst.com/operations/configuration/) | thresholds, poll interval, log level, rotating the API key, PodDisruptionBudget |
| [RBAC and security](https://reactor.robbeverhelst.com/operations/rbac-and-security/) | both RBAC modes, Pod Security, NetworkPolicy |
| [Suspend and dry run](https://reactor.robbeverhelst.com/operations/suspend-and-dry-run/) | `safety.dryRun` and `spec.dryRun`, and why the chart enforces the first one too |
| [Metrics, alerts and dashboard](https://reactor.robbeverhelst.com/operations/metrics-and-alerts/) | `metrics.*`, the ServiceMonitor, the alert rules, the Grafana dashboard — and [every series](https://reactor.robbeverhelst.com/reference/metrics/) turning them on publishes |
| [Events and status](https://reactor.robbeverhelst.com/operations/events/) | what Reactor records on an `Automation`, and the RBAC it needs to — [every reason](https://reactor.robbeverhelst.com/reference/events/) it can raise |
| [Webhook fast path](https://reactor.robbeverhelst.com/operations/webhook-fast-path/) | `unifi.webhook.*`, exposing the receiver, and self-registration |
| [Upgrading](https://reactor.robbeverhelst.com/operations/upgrading/) | what `helm upgrade` does, and what it does not |
| [Uninstalling](https://reactor.robbeverhelst.com/operations/uninstalling/) | the pre-delete hook that hands every held workload back, and `--no-hooks` |
| [Actions](https://reactor.robbeverhelst.com/actions/kubernetes/) | every action type, including the allowlists under `actions.*` and `unifi.actions.*` |
| [Troubleshooting](https://reactor.robbeverhelst.com/troubleshooting/) | nothing is happening, credentials, CRD upgrades, RBAC, stranded workloads |

Start at [Your first Automation](https://reactor.robbeverhelst.com/start/first-automation/) if this is a fresh install; [the Automation API reference](https://reactor.robbeverhelst.com/reference/automation/) has every field of `spec` and `status`.
