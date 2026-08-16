---
title: "RBAC refusals and CRD upgrade failures"
description: "Forbidden on a cross-namespace target, and the invalid ownership metadata error a chart upgrade hits when the CRD was installed the old way."
---

## 5. RBAC refuses a cross-namespace target

```text
Ready  False  ActionFailed
target other-ns/qbittorrent not reachable with current RBAC
(cross-namespace targets need cluster-wide permissions): deployments.apps
"qbittorrent" is forbidden: ...
```

An action targets the Automation's own namespace unless `target.namespace` says otherwise, and naming a different namespace requires the operator to hold cluster-wide permissions:

```sh
helm get values reactor -n reactor-system | grep -A2 rbac
```

With `rbac.clusterWide: true` the chart installs a ClusterRole/ClusterRoleBinding; with `false` it installs a Role in the release namespace only. Confirm what the ServiceAccount can actually do:

```sh
kubectl auth can-i patch deployments \
  --namespace other-ns \
  --as system:serviceaccount:reactor-system:reactor
```

Scaling needs **two** permissions, because a replica count is read and written through the `scale` subresource while the baseline annotation goes on the object itself. If a target's annotations appear but its replicas never move, this is why:

```sh
kubectl auth can-i update statefulsets/scale \
  --namespace other-ns \
  --as system:serviceaccount:reactor-system:reactor
```

A `Node` target is a different problem with a different fix. Node access is opt-in, so the message says so directly:

```text
Ready  False  ActionFailed
target Node/worker-03 not reachable with current RBAC
(node actions are opt-in: install with rbac.allowNodeActions=true): ...
```

Nodes are cluster-scoped, so enabling that creates a ClusterRole even in a namespace-scoped install — see the README before you do. The manifest bundle does not offer node RBAC at all; use the chart, or grant the ClusterRole yourself.

Two ways out for a namespaced target, and the second is usually better in a homelab you did not want cluster-wide RBAC in:

- `helm upgrade ... --set rbac.clusterWide=true`
- Move the Automation into the target's namespace and drop `target.namespace`. Automations are namespaced precisely so they can live next to what they act on.

> With `rbac.clusterWide: false`, the operator watches only the release namespace, and Automations outside it are not reconciled at all — they never get a status. If a resource you created is showing no status whatsoever, check this before anything else. The chart passes the scope to the operator as `WATCH_NAMESPACE`; without it a namespaced install would watch every namespace, be refused at every list, and sit there reporting itself healthy while reconciling nothing.

---

## 6. The CRD: `invalid ownership metadata`, or a stale schema

The `Automation` CRD is a chart **template**, so `helm upgrade` updates the schema like anything else, and `helm.sh/resource-policy: keep` means `helm uninstall` leaves the CRD and your Automations alone.

### Upgrading from chart 0.3.0 or earlier

**There is nothing to do.** Those versions installed the CRD through the chart's `crds/` directory, which Helm applies but never records as part of the release — so the first upgrade to a chart that templates it meets a CRD owned by nobody, which Helm refuses to touch. The chart now adopts that CRD itself, on that one upgrade, and `helm upgrade` is the whole procedure.

What it does, so that nothing about it is a surprise:

- A hook Job — its own ServiceAccount, and a ClusterRole granting `get` and `patch` on that single CRD name — runs before the release is applied, sets the three keys Helm looks for (`app.kubernetes.io/managed-by=Helm`, `meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`, taken from the release you are installing), and puts the chart's schema live in the same patch.
- It is rendered **only** when there is something to adopt. A fresh install, and every upgrade after the first, renders no Job and no cluster-scoped permission at all.
- The CRD is never deleted or recreated, and no `Automation` is read or written. The resources stored under it survive, as they do on any other upgrade.
- It cleans up after itself when it succeeds, and stays put when it fails so you can read `kubectl logs job/<release>-adopt-crd`.

A CRD that belongs to a **different** Helm release is never adopted. That upgrade stops before it changes anything, naming the release that owns it — take it from there deliberately, or upgrade with `--set crds.install=false` and leave the CRD to whoever manages it.

**Doing it by hand instead.** With `--set crds.adopt=false` the chart renders no hook — and stops the upgrade, because nothing is then left that could put the schema live and Helm will not update a CRD it does not own:

```text
Error: UPGRADE FAILED: execution error at (reactor/templates/crds.yaml:...):
the CustomResourceDefinition automations.reactor.robbeverhelst.com belongs to
no Helm release ... and crds.adopt=false turned off the hook that would have
handed it over.
```

It stops before anything is applied, and repeats the pair of commands the hook runs for you — use your own release name and namespace, then upgrade again:

```sh
kubectl label crd automations.reactor.robbeverhelst.com \
  app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate crd automations.reactor.robbeverhelst.com \
  meta.helm.sh/release-name=reactor \
  meta.helm.sh/release-namespace=reactor-system --overwrite
```

The same commands are the fallback if the hook itself fails — its logs say why, and adopting by hand needs no more than this. Once adopted, by either route, it never recurs.

### `helm get manifest` shows no CRD after that upgrade

Expected, on that one revision, and only that one. The CRD is live, owned by your release, and serving the schema the chart shipped — it simply is not part of the manifest Helm recorded for that revision.

Helm checks whether it owns an object while it *prepares* an upgrade, before it runs a single hook. An upgrade that rendered a CRD nobody owns would therefore fail before the hook that establishes ownership could exist. So the chart leaves the CRD out of the release on the adopting upgrade, and the hook applies the schema in the same patch that takes ownership.

That is why the two views disagree, and why both are right:

```sh
helm template reactor oci://ghcr.io/robbeverhelst/charts/reactor   # renders the CRD
helm get manifest reactor -n reactor-system                        # does not
```

`helm template` has no cluster to look in, so it cannot tell the CRD is unowned and renders the ordinary case. The release was rendered against your cluster, where the CRD was unowned, so it rendered the adopting case.

The next `helm upgrade` finds the CRD owned, carries it in the release like any other resource, and it stays there. Nothing is different about a release deployed through Pulumi, Argo CD or Flux — the decision is a `lookup` at render time, made by whichever Helm does the rendering.

To check where you are:

```sh
kubectl get crd automations.reactor.robbeverhelst.com \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}{"\n"}'
```

A release name means the CRD is adopted and the next upgrade will carry it. Empty means adoption has not happened yet.

### A valid Automation is rejected, or a field is silently dropped

**Symptom.** Anything of the form "this is documented but the cluster says it does not exist": validation rejecting a resource that matches the docs, or a field you just added disappearing on apply.

**Cause.** The operator expects a schema the API server does not have. On a current chart this means the CRD is managed outside the release (`crds.install=false`) and was not applied before the upgrade. On chart 0.3.0 or earlier it is the old `crds/` trap: Helm installed the CRD on first install and never touched it again, silently, so every later schema change shipped broken.

**Confirm** by asking the API server what it knows:

```sh
kubectl explain automation.spec --recursive | grep -i <the-field-you-expect>
kubectl get crd automations.reactor.robbeverhelst.com \
  -o jsonpath='{.metadata.annotations}'
```

**Fix.** With `crds.install=true` (the default), `helm upgrade` is the fix — the template carries the current schema. With `crds.install=false`, apply the CRD for the version you are moving to **before** upgrading the release, so the schema is never older than the operator expecting it:

```sh
kubectl apply -f https://raw.githubusercontent.com/robbeverhelst/unifi-reactor/v<chart-version>/config/crd/bases/reactor.robbeverhelst.com_automations.yaml
```

Applying a CRD never touches existing Automation resources. **Deleting one deletes every Automation in the cluster with it** — never "fix" a schema problem by deleting the CRD.

---
