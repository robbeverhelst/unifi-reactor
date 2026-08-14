---
title: "Stability and roadmap"
description: "What is pre-1.0 and may break, why spec.trigger was removed from v1alpha1 rather than left accepting configuration it dropped, and what is planned next."
---

Early days: the API group is `v1alpha1` and the project is pre-1.0, so expect breaking changes between minor versions.

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
