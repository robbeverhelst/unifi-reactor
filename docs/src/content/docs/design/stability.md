---
title: "Stability and roadmap"
description: "What is pre-1.0 and may break, why spec.trigger was removed from v1alpha1 rather than left accepting configuration it dropped, and what is planned next."
---

Early days: the API group is `v1alpha1` and the project is pre-1.0, so expect breaking changes between minor versions.

## What a v1.1 user has to change

Nothing is required, and no workload changes what it does. One procedure gets shorter, and three things become visible that were not:

- **The first upgrade from chart 0.3.0 or earlier no longer needs the two `kubectl` commands.** The chart adopts the CRD into the release itself, through a pre-upgrade hook Job rendered only when there is something to adopt and cleaned up when it succeeds ([what it does, in full](/troubleshooting/rbac-and-crd/#upgrading-from-chart-030-or-earlier)). If that `label`/`annotate` pair is written down in a runbook, it is now the fallback rather than the procedure; `crds.adopt: false` keeps the manual route.
- **`unifi.maxObservationAge` is new and empty, which is exactly what you have today** — unbounded, and silent, if the console stops answering. Setting it makes every automation report `Ready=False` with reason `ObservationStale` past that age, raise a Warning `Event`, and publish `reactor_stale_decisions_total`. It changes nothing about what is written: no claim is released and no `onExit` runs, because going blind must not scale workloads back up mid-outage ([why](/concepts/settling-a-noisy-signal/#how-long-reactor-may-act-on-state-that-has-already-changed)). Start at four or five poll intervals.
- **`status.observedAt` is new on every Automation**, additive and always populated once anything has been observed. If you have alerting or scripts that treat an unexpected status field as drift, this is the one to expect.
- **Two automations that declare different `onExit` levels for one target now say so** — a Warning `Event` with reason `ReversalDisagreement`, `status.targets[].reversalDisagreement`, and `reactor_reversal_disagreements_total`. **Nothing about the resolved value changes**: `min` still wins, and the workload comes back at exactly the number it came back at before ([why it is reported and not resolved](/concepts/reversal-and-baselines/#when-they-disagree-about-coming-back)). This is not gated behind a value, because a contradiction between two of your own specs is not something to opt into being told about — but if you have such a pair today, expect one Warning per automation involved on the first reconcile after the upgrade. Fixing it is a one-line spec edit.

**`spec.trigger` — the event-shaped trigger kind — has been removed from `v1alpha1`.** Up to v0.3.0 the CRD accepted it, CEL-validated it, and then ignored it: no version of the engine has ever processed an event trigger. A v1 whose API accepts configuration it silently drops is worse than one that does not offer the field at all, so it is gone until it is real. Two things had to exist before it could come back, and one of them now does:

- **an action that expresses an occurrence** — *met.* `http.request` and `notification.*` are edge actions: they fire on a transition rather than declaring a level, so an event trigger now has something to run.
- **a captured delivery to match against** — *still missing, and the blocker.* `trigger.match` matched on payload fields, and no UniFi Alarm Manager payload has ever been captured — [`testdata/unifi/webhooks/`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md) is empty, and the webhook fast path deliberately never reads a delivery body. Parsers here are written against real captures, never against an assumed shape, and an event matcher is a parser.

The two-kind split itself is unchanged and still the design. `when` is what that promise protects: nothing with an observable current value will be re-modelled as an event, and no state automation has to migrate when `trigger` returns in `v1alpha2` with the shape it always had.

> **Upgrading from v0.3.0:** an Automation using `spec.trigger` can no longer be created or updated, and `spec.when` is now required. Existing ones survive in etcd — Helm never deletes your resources — and keep doing what they always did, which is nothing. Reactor names them in its log and in an Event on the resource; `kubectl delete` them.

**The name stays `unifi-reactor` through v1**, and adding providers does not change that. The user-facing surface is already provider-neutral — the API group is `reactor.robbeverhelst.com`, the kind is `Automation` with a `provider` field, the chart is `reactor`, the namespace is `reactor-system` — so a NUT, Proxmox, or Prometheus provider lands with no breaking change and nothing to migrate. Only the repository, the Go module path, and the image carry the `unifi-` prefix, and those are the surfaces you touch least. Discovery favours the specific name besides: people search for a UniFi Kubernetes operator, and `reactor` alone has a lot of prior art. If a second provider ever gains real users, renaming is a repository rename (GitHub redirects), a transition period publishing the image under both paths, and a major-version bump of the module path — a decision for when it has users, not for a version boundary on its own.

Parsers are written against real captured API responses committed to [`testdata/`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/), never against assumed formats. Two caveats worth stating plainly.

**A genuine WAN failover has still never been observed** ([#34](https://github.com/robbeverhelst/unifi-reactor/issues/34)). `wan` is derived from which port reports `is_uplink`, inferred from one capture in which only one uplink was live — so whether `is_uplink` follows the traffic or just marks the port configured as primary is unconfirmed. What has changed is that the guess is no longer silent or alone: the gateway's own uplink interface is used as a second opinion where `is_uplink` names no single live port, `isp` (from #6) is compared against `wan` across observations, and any disagreement between them is logged rather than resolved. The provider is exercised against five different hypotheses about what a failover looks like, in tests and in `make dev-mock`, and it reports something defensible under all of them. That is not the same as knowing. Treat `wan` as less battle-tested than `ups`, watch for the [disagreement warnings](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover), and if you have a gateway with two working uplinks, the [capture runbook](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#capturing-a-real-failover) is fifteen minutes that would close this.

And the webhook fast path has been exercised against the mock console, not a real one — which is a large part of why it defaults off and why nothing depends on it being right.

## Roadmap

- Event triggers for genuinely point-in-time things, like a client connecting — returning as `spec.trigger` in `v1alpha2`, once a real delivery payload has been captured and there is an edge action to run ([why it is not in `v1alpha1`](/design/stability/))
- More actions: `restart`, CronJob suspend, and the UniFi write actions
- Richer status conditions, and debounce made visible in status rather than only in the log
- More providers, driven by demand: NUT, Proxmox, Prometheus alerts, Home Assistant

Non-goals: replacing UniFi Network or UniFi OS, becoming a general-purpose workflow engine like n8n or Argo Workflows, replacing Home Assistant, or executing arbitrary shell commands.
