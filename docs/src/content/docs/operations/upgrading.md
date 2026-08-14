---
title: "Upgrading Reactor"
description: "The CRD upgrades with the release, which needs a one-time hand-over on installs from chart 0.3.0 or earlier — plus the two behaviour changes that came with v0.3.0."
---

## Upgrading from chart 0.3.0 or earlier

Nothing to do — `helm upgrade` is the whole procedure.

Those versions installed the CRD through `crds/`, which Helm applies without recording it as part of the release, so the first upgrade to a chart that templates it meets a CRD owned by nobody and Helm will not touch it. The chart adopts that CRD itself on that one upgrade: a hook Job with its own ServiceAccount and a ClusterRole granting `get` and `patch` on that single CRD name sets the ownership metadata from the release you are installing, and puts the current schema live in the same patch. Nothing is deleted or recreated — the CRD stays live, and your Automations with it.

It is rendered only when there is something to adopt, so a fresh install and every later upgrade carry no Job and no cluster-scoped permission. A CRD belonging to a *different* release is never adopted: that upgrade stops, naming the release that owns it.

Set `crds.adopt=false` to hand the CRD over yourself instead; [the troubleshooting guide](/troubleshooting/rbac-and-crd/#6-the-crd-invalid-ownership-metadata-or-a-stale-schema) has the two `kubectl` commands, which are also the fallback if the hook ever fails.

## Managing the CRD outside the release

Set `crds.install=false` where an admin or a GitOps controller owns CRDs. Apply the CRD **before** upgrading the release, so the schema is never older than the operator expecting it:

```bash
kubectl apply -f https://raw.githubusercontent.com/robbeverhelst/unifi-reactor/v<chart-version>/config/crd/bases/reactor.robbeverhelst.com_automations.yaml

helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --set crds.install=false
```

## Behaviour changes since v0.3.0

> **Upgrading from v0.3.0:** an automation with no `onExit` used to leave its workload scaled down permanently. It now restores the baseline instead. Set `reversal: None` to keep the old behaviour.

> **Upgrading from v0.3.0:** an Automation using `spec.trigger` can no longer be created or updated, and `spec.when` is now required. Existing ones survive in etcd — Helm never deletes your resources — and keep doing what they always did, which is nothing. Reactor names them in its log and in an Event on the resource; `kubectl delete` them.
