---
title: "Uninstalling Reactor"
description: "Deleting an Automation hands its workload back rather than stranding it, and a pre-delete hook does the same for the whole release — including what to do when one is stuck."
---

## Removing an automation, or Reactor itself

Deleting an automation while it is holding a workload down hands the workload back rather than stranding it — a finalizer releases the claim first. Removing the policy removes its effect, even mid-outage, so an automation deleted while the UPS is still on battery brings its workload back up.

`helm uninstall` is the case worth understanding, because Helm does **not** delete the `Automation` CRD or your `Automation` resources. They survive the uninstall and simply stop reconciling. A pre-delete hook therefore releases every claim before the operator goes away, and removes the finalizers, which nothing would be left to service:

```sh
helm uninstall reactor -n reactor-system    # workloads return to their pre-Reactor values
helm uninstall reactor -n reactor-system --no-hooks    # skip it; workloads stay where they are
```

Set `uninstall.releaseClaims: false` to make that skip the default. Either way, every workload keeps its `baseline-replicas` annotation, so what it was before Reactor touched it is always recoverable by hand.

What is **not** covered: deleting the operator's Deployment directly, or losing the cluster. Reactor does not supervise its own absence — the annotations are the answer there. And if you ever delete an automation while the controller is down, its finalizer has nothing to release it:

```sh
kubectl patch automation <name> -n <namespace> \
  --type=merge -p '{"metadata":{"finalizers":[]}}'
```

## The pre-delete hook, and why it stops the operator first

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
