# Troubleshooting

Reactor observes state and reconciles against it, so almost every problem is one of three things: it cannot see the state, it disagrees with you about what the state means, or it cannot act on the target. This page is ordered by how often each turns out to be the answer.

> Examples below use documentation addresses (`192.0.2.10`) and placeholder names. Substitute your own.

## Start here

Four questions answer most problems, and the first one is the one nothing else can tell you:

```sh
# 0. Is it still observing at all? (needs metrics.enabled — see §13)
#    time() - reactor_last_observation_timestamp_seconds

# 1. What does Reactor think is true?
kubectl -n reactor-system logs deploy/reactor | grep -E 'state (observed|transition)'
# INFO state transition provider=unifi key=wan from=primary to=backup

# 2. What does this Automation think?
kubectl -n media get automation pause-downloads-on-backup-wan -o yaml

# 3. Why is it saying that?
kubectl -n media describe automation pause-downloads-on-backup-wan
```

`state observed` is a debug line and the chart defaults to `info`, so it is usually absent. Turn it on for as long as you are debugging:

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set log.level=debug
```

At the default level you still get every change, because `state transition` logs at INFO whenever a key moves:

```text
INFO state transition provider=unifi key=wan from=primary to=backup
```

`log.format: json` switches the encoder if you are reading these through a log collector rather than by eye.

The status block is the single most useful artifact, and it is what a bug report needs:

```yaml
status:
  matching: true
  observedState:
    wan: backup
  lastTransition: {key: wan, from: primary, to: backup, time: "..."}
  lastExecution: {status: Success, onExit: false, time: "..."}
  conditions:
    - type: Ready
      status: "True"
      reason: Reconciled
```

## Reading the `Ready` condition

| Reason | Meaning | Where to look |
| --- | --- | --- |
| `Reconciled` | Normal. Evaluated against observed state. | — |
| `Suspended` | `spec.suspend: true`. State is still observed; no target is claimed. | [§1](#1-nothing-happens-when-the-state-changes) |
| `DryRun` | `spec.dryRun: true`, or the whole install runs with `safety.dryRun`. Everything is evaluated; nothing is written. | [§14](#14-an-automation-is-not-acting-and-is-telling-you-what-it-would-do) |
| `ProviderStateUnavailable` | No state has been observed yet for this provider. | [§1](#1-nothing-happens-when-the-state-changes) |
| `StateKeyUnavailable` | A key this Automation needs vanished from the observation. Last known matching state is held. | [§2](#2-statekeyunavailable-and-held-state) |
| `ObservationStale` | The console has stopped answering at all, and the last state it gave is older than `unifi.maxObservationAge`. Everything is held and still acted on. | [§2a](#2a-observationstale-and-how-old-a-decision-is-allowed-to-be) |
| `ActionFailed` | An action returned an error. `status.lastExecution.reason` has the message. | [§5](#5-rbac-refuses-a-cross-namespace-target), [§6](#6-the-crd-invalid-ownership-metadata-or-a-stale-schema) |

`Applied` carries its own reasons, and two of them are not faults: `DeferredToOtherAutomation` is a peer's more restrictive claim winning, and `TargetManagedByHPA` is Reactor declining a target another controller drives — [§15](#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment).

An Automation left over from before `spec.trigger` was removed has no conditions at all: `spec.when` is now required, so the API server rejects any write to it, status included. It is reported once per reconcile in the operator log and as a Warning Event on the resource instead:

```sh
kubectl -n media describe automation notify-on-client-connect | tail -5
# Warning  EventTriggerRemoved  spec.trigger was removed from v1alpha1 and was never
#          implemented; this automation does nothing, delete it
```

Delete it. Event triggers never ran on any version, so nothing is lost, and nothing it referenced was ever claimed. See the README's [Stability](../README.md#stability) section for when they come back.

> On v0.3.0, `ActionFailed` is reported with `status: "True"` — a bug where the condition status was not flipped alongside the reason. Read the *reason*, not the status, on that version. Fixed in the target-ownership batch.

---

## Reading the Event stream

`kubectl describe automation <name>` ends with an Event list, and for most
questions it is faster than anything else on this page — it needs no log
access, and it is already in chronological order:

```sh
kubectl -n media describe automation pause-downloads-on-backup-wan | tail -20
```

| Reason | Type | Means |
| --- | --- | --- |
| `StateEntered` / `StateExited` | Normal | the condition started or stopped holding; the message names the key that moved |
| `TargetHeld` / `TargetReleased` | Normal | a write to a target actually happened; the message names the level in words ("0 replicas", "suspended") |
| `DeferredToOtherAutomation` | Normal | a peer's more restrictive claim won — [§7](#7-two-automations-fighting-over-one-target) |
| `EdgeActionSent` | Normal | an edge action ran: a notification or HTTP request delivered, or a restart applied |
| `ReversalDisagreement` | Warning | two Automations declared different `onExit` levels for one target, so they disagree about its normal size — [§7](#the-workload-came-back-at-the-wrong-number) |
| `StateKeyUnavailable` | Warning | a key vanished and state is being held — [§2](#2-statekeyunavailable-and-held-state) |
| `ObservationStale` | Warning | the console has stopped answering and decisions are being taken against old state — [§2a](#2a-observationstale-and-how-old-a-decision-is-allowed-to-be) |
| `ActionFailed` | Warning | a desired-state action could not be applied — [§5](#5-rbac-refuses-a-cross-namespace-target) |
| `RetryBudgetExhausted` | Warning | Reactor stopped retrying and is waiting for the next state change |
| `EdgeActionFailed` / `EdgeActionSkipped` | Warning | an edge action did not happen — [§12](#12-a-notification-or-http-request-did-not-arrive) |
| `ReleaseFailed` | Warning | deletion could not hand a target back and let the object go anyway — [§8](#8-a-workload-is-stuck-down-after-an-automation-was-deleted) |
| `EventTriggerRemoved` | Warning | a leftover `spec.trigger` automation that does nothing; delete it |
| `DryRun` | Normal | a dry run reached the transition it would have acted on; the message says what it would have done — [§14](#14-an-automation-is-not-acting-and-is-telling-you-what-it-would-do) |
| `TargetManagedByHPA` | Warning | a HorizontalPodAutoscaler already drives the target, so Reactor declined it rather than fight — [§15](#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment) |

**Being outvoted is `Normal`, not a Warning.** Two Automations sharing a
workload and one of them losing is the arbitration working as designed.
`ReversalDisagreement` is the Warning next to it, and the difference is not
severity for its own sake: two automations wanting a workload down for different
reasons are both right, while two declaring different normal sizes for it cannot
both be — nothing Reactor does resolves that, so somebody has to.

**Events fire on edges, not on states.** A condition that has been held for an
hour raised one Event when it started, not one every fifteen seconds — so an
old timestamp on a Warning means "still true since then", not "stale". Read the
Age column with that in mind, and read `status.conditions` for what is true
*now*.

**No Events at all** on an Automation that is clearly doing things has one
likely cause: the operator's RBAC does not grant `create` and `patch` on
`events` in the **`events.k8s.io`** API group. A rule naming only the core
group (`""`) is refused on every emission, and the refusal is logged by the
event broadcaster and surfaced nowhere else:

```sh
kubectl auth can-i create events.events.k8s.io \
  --namespace media \
  --as system:serviceaccount:reactor-system:reactor
```

Charts from this release on grant it. An operator installed from an older chart,
or with hand-written RBAC copied from one, will be silent.

**They expire.** Events live on your cluster's retention, an hour by default.
They are for the incident you are in; `status` is the durable record.

---

## 1. Nothing happens when the state changes

Work down this list; it is ordered by likelihood.

**It is suspended.** The cheapest check, and it survives restarts and upgrades because it is spec, not state:

```sh
kubectl -n media get automation
# NAME                           PROVIDER   MATCHING   SUSPENDED   READY   AGE
# pause-downloads-on-backup-wan  unifi      false      true        True    3h
```

A suspended Automation observes and reports state but claims nothing, so its targets sit wherever the automations still in force put them — see [pausing an automation](../README.md#pausing-an-automation). `kubectl patch automation <name> --type=merge -p '{"spec":{"suspend":false}}'` puts it back in force, and it re-claims on the next reconcile if its condition still holds.

**The provider is not enabled.** Without `unifi.url` set, the provider never starts and every state Automation sits at `ProviderStateUnavailable` forever. The startup log says so plainly:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'UniFi provider'
# INFO UniFi provider disabled (UNIFI_URL not set); state triggers will stay pending
```

Fix with `--set unifi.url=https://192.0.2.10` on `helm upgrade`.

**The console is not reachable, or the key is wrong.** See [§3](#3-credentials-and-reachability).

**Your condition does not match the observed vocabulary.** All keys in a `state` block must match, exactly, case-sensitively. Compare what you wrote against what the provider actually publishes:

```sh
kubectl -n media get automation pause-downloads-on-backup-wan -o jsonpath='{.spec.when.state}'
kubectl -n reactor-system logs deploy/reactor | grep 'state observed' | tail -1
```

The published keys and values are listed in the [README state-key table](../README.md#state-keys). `wan: Backup`, `ups: battery`, and `ups.battery: LOW` all silently never match.

**It already matched.** Actions run on *transitions*. If `status.matching` was already `true` when you deployed the workload, nothing fires until the state leaves and re-enters. `status.lastTransition` tells you when it last flipped. To force a re-run during testing, flip the state at the source (`make dev-mock` exposes `POST /flip` and `POST /ups`) rather than editing the resource.

**You are inside the latency window.** Reaction time is bounded by the poll interval (`unifi.pollInterval`, default 30s). A transition wakes affected Automations immediately, but if that wake queue is saturated the Automation falls back to a periodic re-evaluation of ~15s. That falling-back is a debug line, so you only see it with `log.level=debug`:

```text
DEBUG wake queue full, leaving it to periodic re-evaluation automation=media/pause-downloads-on-backup-wan
```

Occasional lines are harmless by design — the wake is an optimization, the poll is the mechanism. Continuous ones mean the reconciler is not keeping up.

**Only the leader polls.** With `replicaCount > 1`, the non-leader replicas observe nothing; that is correct. Check the logs of the pod holding the lease, not an arbitrary one.

---

## 2. `StateKeyUnavailable` and held state

```text
Ready  False  StateKeyUnavailable
provider "unifi" is not reporting ups, ups.battery; holding last known state
```

**What it means.** A key the Automation needs disappeared from the observation entirely. Providers omit keys they cannot observe rather than inventing a value, so this says: the hardware publishing that key is no longer visible to the console — a UPS that dropped off, a gateway mid-reboot, a device removed from the site.

**What Reactor does.** It holds `status.matching` at its last known value and does *not* run `onExit`. This is deliberate and it is the behaviour you want: losing sight of a UPS during a power cut is not evidence that the power came back. Treating it as "no longer matching" would scale your workloads back up in the middle of the outage.

**What it is not.** It is not the same as the key being present with a different value — that is a normal transition. And it is not `ProviderStateUnavailable`, which means *no* state at all has been observed for the provider.

**Confirm which keys vanished** by comparing the message against the last full observation:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'state observed' | tail -1
```

**Fix the hardware, not the Automation.** Re-adopt or power up the device. The key reappears on the next poll and the condition clears on its own. If the device is gone for good, delete or rewrite the Automations that reference its keys — otherwise they hold their last matching state indefinitely, which is exactly what "held" means.

**A worked trap.** The UPS keys are only published when a UniFi UPS is adopted. Writing `when: {ups: on-battery}` with no UPS on the site gives you an Automation that never matches and permanently reports `StateKeyUnavailable`. That is not a bug; it is the operator declining to guess.

---

## 2a. `ObservationStale`, and how old a decision is allowed to be

```text
Ready  False  ObservationStale
provider "unifi" has not been observed since 2026-08-14T09:12:41Z, past the 5m0s this
install allows; still acting on the state it last reported
```

**What it means.** The console has stopped answering. Not one key missing from a reply — [that is §2](#2-statekeyunavailable-and-held-state) — but no successful reply at all since the timestamp in the message. A failed observation is logged and dropped, because the next poll is the recovery mechanism, so the state Reactor reports is simply the last one it got.

**What Reactor does: exactly what it was doing.** Nothing is released, no `onExit` runs, no target moves. This is deliberate and it is the behaviour you want for the same reason §2 is: the console is often unreachable *because* of the thing the automation is reacting to. Handing workloads back the moment Reactor loses sight of a UPS would bring them up on battery power. So the bound governs what is **said**, never what is **done**.

**Two windows, and only this one is unbounded.** A value that *changed* reaches an automation within `unifi.pollInterval` × that key's debounce samples — 30 seconds for `wan` at the defaults, 90 for `internet`. A console that has gone quiet has no such window at all, which is why it is the one that has to announce itself.

**How old is it?** Every Automation reports the observation its decisions are being taken against, whether or not a bound is set:

```sh
kubectl get automation -A -o custom-columns=\
'NAME:.metadata.name,MATCHING:.status.matching,OBSERVED:.status.observedAt'
```

**Turning the report on.** It is empty by default, which means unbounded:

```sh
helm upgrade reactor ... --set unifi.maxObservationAge=5m
```

Set it against `unifi.pollInterval` and the debounce samples rather than in isolation. Anything under about four poll intervals reports a slow console rather than a blind operator.

**Then fix the console, not the Automation.** The cause is in [§3](#3-credentials-and-reachability) — an expired API key, a rebooted gateway, a network policy, a certificate. Every failed attempt logs it:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'state observation failed'
```

The condition clears on its own on the first successful poll; nothing has to be reset.

**The fleet-wide version of the same question** needs no bound and no Automation, but it does need `metrics.enabled`, and somebody looking:

```promql
time() - reactor_last_observation_timestamp_seconds   # is Reactor still seeing anything
rate(reactor_stale_decisions_total[15m])              # was it still deciding while it was not
```

The shipped `ReactorObservationStale` alert is the first of those. The counter is the attributable half: the gauge says Reactor went blind, the counter says automations went on making decisions while it was.

**What it is not.** It is not `ProviderStateUnavailable`, which means nothing has *ever* been observed — a first start against a console that has never answered. An install that has been running for a week and lost its console reports this instead, and keeps its claims.

---

## 3. Credentials and reachability

Every observation failure logs the same line, with the cause in the error:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'state observation failed'
```

| Error contains | Cause | Fix |
| --- | --- | --- |
| `unexpected status 401` / `403` | API key wrong, revoked, or from a different console | Re-create the key under **Settings → Control Plane → Integrations** and update the Secret |
| `unexpected status 404` | Wrong site. `unifi.site` is the *internal reference*, not the display name | Check the site list; `default` is right for single-site setups |
| `x509` / `certificate signed by unknown authority` | UniFi OS serves a self-signed certificate | `unifi.insecureSkipVerify: true` (the chart default), or trust the CA |
| `context deadline exceeded` / `no route to host` | Pod cannot reach the console | Check NetworkPolicy and that the URL is reachable *from the pod*, not from your laptop |
| `connection refused` on the API path | URL points at the wrong port or scheme | Use the console base URL only — no path, e.g. `https://192.0.2.10` |
| `no gateway with an active WAN uplink and no UPS found` | Reachable and authenticated, but nothing recognizable in the device list | Confirm the gateway is adopted on that site |

Test reachability from inside the cluster rather than from your machine:

```sh
kubectl -n reactor-system run unifi-probe --rm -it --restart=Never \
  --image=curlimages/curl -- \
  curl -sk -o /dev/null -w '%{http_code}\n' \
  -H "X-API-KEY: $UNIFI_API_KEY" \
  'https://192.0.2.10/proxy/network/api/s/default/stat/device'
```

`200` means the credential and the path are both fine and the problem is elsewhere.

### Rotating the API key

The Secret is **mounted**, and the key is re-read from the file on every poll. Rotation therefore needs no restart: update the Secret, and the next poll after the kubelet refreshes the file authenticates with the new key.

```sh
kubectl -n reactor-system create secret generic unifi-reactor-credentials \
  --from-literal=UNIFI_API_KEY=<new key> \
  --dry-run=client -o yaml | kubectl -n reactor-system apply -f -
```

Watch polling continue before revoking the old key in the UniFi UI. Four things can still go wrong:

**Nothing happens for up to a minute.** The kubelet refreshes a mounted Secret on its sync period, roughly a minute by default. A `401` in that window is expected if you revoked the old key first. Revoke second.

**The key file is unreadable or empty.** That poll fails with a distinctive error and the next one retries:

```text
ERROR state observation failed reading unifi api key from /etc/unifi-reactor/credentials/UNIFI_API_KEY: ...
ERROR state observation failed unifi api key file /etc/unifi-reactor/credentials/UNIFI_API_KEY is empty
```

Usually the Secret was replaced without the `UNIFI_API_KEY` key, or with an empty value. Note this is also checked at startup — the operator exits rather than starting up unable to authenticate.

**The Secret is mounted through `subPath`.** A `subPath` mount is a copy made at container start and is *never* updated in place, which silently restores the old "needs a restart" behaviour. The chart mounts the whole directory precisely to avoid this; if you have customised the mount, this is your answer.

**You are on chart 0.3.0 or earlier**, or you set `UNIFI_API_KEY` in the environment yourself. That path injects the key once at container start, where it is fixed for the life of the process. Confirm which one you are on:

```sh
kubectl -n reactor-system get deploy reactor \
  -o jsonpath='{.spec.template.spec.containers[0].env[*].name}'
# UNIFI_API_KEY_FILE  -> mounted, re-read per poll
# UNIFI_API_KEY       -> injected once, rotation needs a restart
```

For the env path, `kubectl -n reactor-system rollout restart deployment/reactor` after rotating. Better, upgrade the chart. If you would rather restart on change anyway — because you already run something like reloader — the chart takes Deployment `annotations` for it.

### The webhook shared secret

Only relevant with `unifi.webhook.enabled`. This is a **second, separate** credential from the API key, and its failures all look the same from the outside: reactions go back to taking up to `unifi.pollInterval`, because the fast path is an optimization and losing it breaks nothing else. Nothing is stuck — it is just slow again.

```sh
kubectl -n reactor-system logs deploy/reactor | grep -i 'webhook'
```

| What you see | Cause |
| --- | --- |
| `Rejected a webhook delivery presenting no valid token` (needs `log.level=debug`) | The console's rule does not carry the secret in `UNIFI_WEBHOOK_TOKEN`, or carries an old one |
| `Webhook fast path listening` and then nothing | Deliveries never arrive: the console cannot reach the receiver. It is a ClusterIP by default, and a `networkPolicy` with the default `ingress: []` also denies them |
| `Could not register the Alarm Manager rule` | Self-registration failed; the error names the reason. Polling is unaffected, and the rule can be created by hand in the UniFi UI |

Rotating this secret is not restart-free the way the API key is, and it takes two steps: the console holds a copy inside the Alarm Manager rule, and Reactor never edits that rule. Update the Secret, restart the pod, then update or recreate the rule in the UniFi UI. Deliveries are refused in between, which costs latency and nothing else.

---

## 4. `onExit` fired — or did not — when you did not expect it

`onExit` declares **the level you want while the condition does not hold**, not a script that runs at the moment of exit. Most surprises come from reading it the second way.

**It fired and you expected nothing.** A key you match on changed value. With `when: {ups: on-battery, ups.battery: critical}`, a battery recovering from 8% to 15% moves `ups.battery` from `critical` to `low`, the condition stops holding, and the reversal applies — even though the power is still out. That is the semantics working; the fix is matching only the keys you actually care about.

**It fired the moment you suspended the Automation.** That is the same semantics again: suspending takes the policy out of force, so the target is arbitrated as if the Automation were gone, and the reversal applies even though the condition still holds — `status.matching` stays `true` and says so. See [pausing an automation](../README.md#pausing-an-automation) for why that matches what deleting does, and for the `reversal: None` patch that freezes the workload instead.

**It did not fire and you expected it to.** Three candidates, in order:

1. The condition never stopped holding. Check `status.matching` and `status.observedState`.
2. A key went missing rather than changing, so state is held — see [§2](#2-statekeyunavailable-and-held-state).
3. Another Automation still claims the same target and its claim wins. See [§7](#7-two-automations-fighting-over-one-target).

**You omitted `onExit` and the workload came back anyway.** From v1, an Automation with no `onExit` restores the target's recorded baseline when it stops matching, rather than leaving the workload wherever it was put. A workload that never comes back was judged the worse default. To get the old behaviour — Reactor sets it and never touches it again — set `spec.reversal: None` explicitly.

> The `reversal` field and baseline restore land with the target-ownership change. On v0.3.0, an omitted `onExit` leaves the target untouched on exit.

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

The `Automation` CRD is a chart **template**, so `helm upgrade` updates the schema like anything else, and `helm.sh/resource-policy: keep` means `helm uninstall` leaves the CRD and your Automations alone. Two things still go wrong, and the first one happens exactly once.

### `helm upgrade` fails with `invalid ownership metadata`

**Symptom.** Upgrading from chart 0.3.0 or earlier, the upgrade refuses before changing anything:

```text
Error: UPGRADE FAILED: rendered manifests contain a resource that already
exists. Unable to continue with update: CustomResourceDefinition
"automations.reactor.robbeverhelst.com" ... invalid ownership metadata;
label validation error: missing key "app.kubernetes.io/managed-by" ...
```

**Cause.** Those versions installed the CRD through the chart's `crds/` directory, which Helm applies but does not record as part of the release. The new chart renders the same object as a template, finds it already there owned by nobody, and stops. This is Helm protecting you, not a broken chart.

**Fix.** Hand the existing CRD over to the release, then upgrade again. Nothing is deleted or recreated — the CRD stays live and your Automations with it:

```sh
kubectl label crd automations.reactor.robbeverhelst.com \
  app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate crd automations.reactor.robbeverhelst.com \
  meta.helm.sh/release-name=reactor \
  meta.helm.sh/release-namespace=reactor-system --overwrite
```

Use your own release name and namespace — a mismatch here produces the same error with a different message, naming the release it thinks owns the object. Once adopted, this never recurs.

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

## 7. Two Automations fighting over one target

When more than one Automation names the same target, the desired value is resolved by a fold over every Automation claiming it, not by whichever reconciled last. For `kubernetes.scale` the fold is `min` — the most restrictive claim wins, so shedding beats restoring.

Two Automations, `power/shed-on-battery` and `net/pause-on-backup-wan`, both scaling `media/qbittorrent` to 0. Power returns while the WAN is still on backup: the first stops matching and wants 1, the second still matches and wants 0, and `min(1, 0) = 0`. The workload correctly stays down. It comes back when the last claim releases.

This is visible rather than mysterious — each Automation reports what it wanted and what it got:

```yaml
status:
  matching: false
  targets:
    - ref: media/qbittorrent
      desired: 1        # what this Automation alone wants
      effective: 0      # what the arbiter resolved
      deferredBy:
        - power/shed-on-battery
```

`deferredBy` names who outvoted you. And the target itself explains its own state:

```sh
kubectl -n media get deploy qbittorrent -o jsonpath='{.metadata.annotations}'
# reactor.robbeverhelst.com/baseline-replicas: "1"   # or baseline-suspend on a CronJob
# reactor.robbeverhelst.com/claimed-by: "power/shed-on-battery,net/pause-on-backup-wan"
# reactor.robbeverhelst.com/claimed-at: "..."
```

`baseline-replicas` is what Reactor found before it first claimed the target, and it is what a reversal restores. `claimed-by` and `claimed-at` are advisory — refreshed each reconcile, never read back as truth — and exist so `kubectl describe deploy` explains the zero to a human at 3am.

**A scale-*up* Automation loses to any scale-down claim on the same target**, because `min` encodes "most restrictive wins". `status.targets[].effective` makes it visible instead of silent. If you need the opposite, that is a design conversation, not a misconfiguration.

### The workload came back at the wrong number

A different failure with the same shape, and the one that is easy to miss because it only shows up *after* the incident is over. Two Automations sharing a target can disagree about what its **normal** size is:

```yaml
# power/shed-on-battery          # net/pause-on-backup-wan
onExit: [replicas: 1]            onExit: [replicas: 3]
```

While either matches, the workload is at 0 and everything above applies. When the last one releases, the reversals are folded the same way live claims are — `min(1, 3) = 1` — and the workload comes back at 1.

**Reactor still takes `min`, and will not guess which number you meant.** What it does is say that the two specs contradict each other, from the moment they do rather than at release:

```sh
kubectl -n power get automation shed-on-battery -o jsonpath='{.status.targets[0].reversalDisagreement}'
# [{"claimant":"net/pause-on-backup-wan","desired":3,"level":"3 replicas"},
#  {"claimant":"power/shed-on-battery","desired":1,"level":"1 replicas"}]
```

```sh
# every automation currently contradicting another about a target
kubectl get automation -A -o json | jq -r '
  .items[] | .status.targets[]? | select(.reversalDisagreement) |
  "\(.ref): \([.reversalDisagreement[] | "\(.claimant) wants \(.level)"] | join(", "))"' | sort -u
```

There is a Warning `Event` with reason `ReversalDisagreement` on each Automation involved, and `reactor_reversal_disagreements_total` for the fleet-wide count.

**Fix it in the specs, not in Reactor.** Decide what the workload's normal size is and write the same number in both, or give one of them `reversal: None` so it contributes no level at all. `None` is never part of a disagreement, and two Automations both on `Baseline` agree by construction — they resolve to the same recorded baseline. The cases reported are `Declared` against `Declared`, and `Declared` against `Baseline`.

**It is a Warning, unlike `DeferredToOtherAutomation`.** Being outvoted on a live claim is two correct policies arbitrating, which is the design working. This is two policies that cannot both be correct, where the value Reactor picks is a tie-break rather than an answer.

**None of this reaches a claimant that is not an Automation.** The fold is over what Reactor can see, so a HorizontalPodAutoscaler on the same Deployment is not resolved — it is fought, unless detection is on. That is [§15](#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment), and it looks quite different: a workload that flaps rather than one that settles on a value somebody else asked for.

> The fold, `status.targets[]`, and the target annotations land with the target-ownership change. On v0.3.0 the outcome of two Automations on one target depends on reconcile order.

---

## 8. A workload is stuck down after an Automation was deleted

The worst failure mode in the system, because the cause and the symptom are a week apart. Reactor scaled something to 0, the Automation was deleted, and nothing is left to scale it back up.

**Find everything Reactor is currently holding down:**

```sh
kubectl get deploy -A \
  -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,REPLICAS:.spec.replicas,BASELINE:.metadata.annotations.reactor\.robbeverhelst\.com/baseline-replicas' \
  | grep -v '<none>'
```

Any row with a baseline annotation is or was claimed. A row where `REPLICAS` is 0 and `BASELINE` is not is a stranded workload, and the annotation is its restore instruction:

```sh
kubectl -n media scale deploy/qbittorrent --replicas=1
kubectl -n media annotate deploy/qbittorrent \
  reactor.robbeverhelst.com/baseline-replicas- \
  reactor.robbeverhelst.com/claimed-by- \
  reactor.robbeverhelst.com/claimed-at-
```

This works with Reactor uninstalled, which is the point of putting the baseline on the target rather than anywhere else.

**Why it should not happen from v1.** Automations holding a claim carry a `reactor.robbeverhelst.com/release-claims` finalizer. On delete, Reactor recomputes the fold without the deleted Automation, patches the target, clears the annotations if it was the last claimant, and removes the finalizer. Deleting an Automation mid-outage brings the workload back even though the condition still holds — removing the automation removes the policy.

**Three cases the finalizer does not cover:**

*`kubectl delete` while the operator is down.* The resource sits in `Terminating` until the operator returns and processes the finalizer. Start it back up if you can. If the operator is gone for good and you need the resource gone:

```sh
kubectl -n media patch automation pause-downloads-on-backup-wan \
  --type=merge -p '{"metadata":{"finalizers":[]}}'
```

That strands whatever it was holding. Restore the target by hand using the baseline annotation above.

*`helm uninstall`.* The CRD carries `helm.sh/resource-policy: keep`, so both it and every Automation stored under it *survive* the uninstall — deliberately, because losing people's resources to an uninstall is worse. They simply stop reconciling, and workloads freeze wherever Reactor last put them. No finalizer ever fires, because nothing is being deleted. The chart ships a pre-delete hook that releases every claim before the controller goes away, gated by `uninstall.releaseClaims` (default `true`); if you disabled it, or the hook failed, sweep the annotations manually before removing the chart.

The hook stops the operator before it releases anything. Helm removes the release's own resources only once its pre-delete hooks have finished, so a controller left running would re-claim every workload the hook had just released — and re-add the finalizer, which by then has nothing left to service it. If an uninstall is interrupted after the hook has run but before Helm finishes, the operator is left scaled to zero; `helm upgrade` or `helm rollback` puts it back.

Deleting the CRD afterwards is a deliberate, separate act, and it takes every Automation with it:

```sh
kubectl delete crd automations.reactor.robbeverhelst.com
```

*Deleting the CRD while Automations still carry finalizers.* Nothing is left to remove them and deletion hangs forever. Clear the finalizers first (the patch above, over every Automation), then delete the CRD.

**And one case where the finalizer fired and could not finish.** Different from the three above: Reactor is running, the finalizer is doing exactly its job, and the release itself keeps failing — the target has been deleted, RBAC changed under it, an admission webhook is refusing the write. Each failed attempt is counted in `status.releaseAttempts` and reported as `Applied=False` with reason `ReleaseFailed`, carrying the error:

```sh
kubectl -n media get automation pause-downloads-on-backup-wan -o jsonpath=\
'{.status.releaseAttempts}{"\n"}{.status.conditions[?(@.type=="Applied")].message}{"\n"}'
```

It is bounded at three attempts, 5 then 10 seconds apart, after which Reactor removes the finalizer anyway and lets the object go:

```text
Warning  ReleaseFailed  could not hand targets back after 3 attempts, deleting anyway: ...
```

That is the same trade this whole section is about, made in the other direction: a stranded workload is recoverable from its `baseline-replicas` annotation, and a resource stuck `Terminating` forever is not. So treat that Event as the one that sends you to the restore commands at the top of this section — it is the only case where the finalizer existed, ran, and still left a workload behind.

You will never catch `releaseAttempts` at 3. The third failure is the reconcile that removes the finalizer, so the object is deleted before that value could be written; on a live object it reads 1 or 2, and only ever while it is `Terminating`.

**What is explicitly not covered:** the controller being deleted outright, permanently evicted, or the cluster rebuilt. Reactor does not supervise its own absence. The baseline annotation on the target is the answer in those cases, and it is the reason it lives there.

> The finalizer, the pre-delete hook, and `uninstall.releaseClaims` land with the target-ownership change. On v0.3.0 there is no finalizer and no release-on-delete: deleting an Automation strands its target, and the manual restore above is the only route.

---

## 9. GitOps: Reactor's changes look like drift

If your targets are managed by Flux or Argo CD, Reactor and your GitOps controller will fight over the same Deployments. Reactor writes:

- `spec.replicas` — already true today, and the entire point of the operator
- `metadata.annotations.reactor.robbeverhelst.com/*` — the baseline and claim record

Both must be excluded from drift detection on any target an Automation names, or your GitOps controller will restore the replica count Reactor just changed, in a loop.

Argo CD, on the target Application:

```yaml
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      name: qbittorrent
      jsonPointers:
        - /spec/replicas
        - /metadata/annotations/reactor.robbeverhelst.com~1baseline-replicas
```

If you would rather exclude by field manager than by path, check what Reactor's patches are actually recorded under before writing the name into config:

```sh
kubectl -n media get deploy qbittorrent -o jsonpath='{.metadata.managedFields[*].manager}'
```

Flux, on the target Kustomization, via `spec.patches` or by omitting `replicas` from the source manifest entirely. The general rule: whatever field an Automation controls should not be specified in Git.

---

## 10. `wan` and `isp` disagree about a failover

Reactor derives `wan` from which WAN port reports `is_uplink`, and cross-checks it against two
signals that answer the same question independently: the interface the gateway names as its
uplink, and the ISP behind the address it currently holds. When those stop agreeing, it says so
rather than picking a winner:

```sh
kubectl -n reactor-system logs deploy/reactor | grep unifi-wan
```

| What you see | What it means | What to do |
| --- | --- | --- |
| `The gateway's WAN signals disagree about which uplink is live` | `is_uplink` and `uplink.name` point at different ports. Reactor reports the `is_uplink` answer. | Check which uplink is actually carrying traffic in the UniFi UI, and say which one Reactor got right on [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34) |
| `The ISP behind the uplink changed but the gateway still reports the same uplink` | Your traffic moved to a different carrier while `wan` did not move. If that was a failover, `wan` missed it. | Same — this is the observation issue #34 is open for |
| `The gateway changed uplink but the ISP behind it did not change` | `wan` moved without your carrier changing. Normal if both uplinks are with the same ISP; suspicious otherwise. | Nothing, unless your two uplinks are with different carriers |
| `The uplink believed to be live does not report itself as online` | The port Reactor thinks is carrying traffic reports something other than `online` in `last_wan_status`. | Note the exact status value on [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34) — only `online` has ever been observed, and the failed value is unknown |
| `is_uplink does not name a single live WAN port` | No port claimed the uplink, or both did. Reactor fell back to the gateway's uplink interface. | Nothing; this is the fallback working. Worth reporting if it persists rather than appearing for one poll during a switchover |
| `The health endpoint accumulated uptime on an uplink other than the one wan names` | A **third** signal, from a different endpoint, disagrees — and the strongest one, because uptime is traffic the console watched pass rather than a statement about configuration. | This is the most useful thing you can report on [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34). Post the `uptime_stats` block alongside the `wan1`/`wan2` fields |

None of these stops anything: state is still published and Automations still run. They exist
because the `wan` mapping has never been checked against a real failover, and a wrong mapping
that says nothing is far worse than one that complains. If you have a gateway with two working
uplinks, [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34) is where these lines
turn into an answer.

### What is *not* a disagreement

`internet: down` while `wan: primary` is not one of these, and Reactor will never
log it as one. That combination is precisely the failure mode `internet` exists to
observe — the link is up, the uplink is unchanged, and there is no internet — so
treating it as a contradiction would fire a warning on exactly the case the key was
added for. If you see it, believe it: your uplink is selected and useless.

`wan.quality: degraded` while `internet: ok` is not one either. They answer different
questions over different time horizons: `internet` is the console's judgement about
reachability right now, `wan.quality` is availability and latency averaged over the
console's uptime window (24 hours on the hardware it was captured from). A link that
was down for twenty minutes this morning is legitimately `degraded` and `ok` at the
same time for the rest of the day.

---

## 10a. `internet` or `wan.quality` never appears

Both come from `stat/health`, which is a **separate request** from the one that
produces `wan`, `isp` and the UPS keys. A console that answers one and not the other
publishes the keys it can — that is the same per-key degradation as a UPS dropping
off, and it is deliberate — so the two failures look different in the logs:

```sh
kubectl -n reactor-system logs deploy/reactor | grep -E 'unifi-health|unifi-observe'
```

| What you see | What it means |
| --- | --- |
| `The health endpoint failed; internet and wan.quality are unavailable this poll` | The request failed or returned a non-200. The device keys are still being published. Check the API key has access and that the console is not mid-reboot |
| `The www subsystem reports a status this provider does not recognise` | Your console uses a status string this provider has never seen. **Please report it** — the mapping is inferred from one capture, and this line is the evidence that would fix it |
| `The health response carries no uptime stats for the live uplink` | The `uptime_stats` block does not have an entry for the uplink `wan` names. Expected mid-switchover; worth reporting if it persists |
| `The live uplink's health entry reports no availability` (at `log.level=debug`) | The console reported the uplink but no numbers for it, so `wan.quality` is withheld rather than guessed at zero |
| Neither key ever appears, and no line above | `wan` itself is not derivable, which withholds `wan.quality` too — `internet` should still be there. Start at [§13](#13-reactor-is-running-but-nothing-is-reacting) |

`wifi` comes from the same response and degrades the same way. It is derived from
the `wlan` subsystem's AP counts rather than from its `status` string, so:

| What you see | What it means |
| --- | --- |
| `The wlan subsystem reports no AP counts` (debug) | `num_adopted` or `num_disconnected` is missing. Neither is read as zero, so the key is withheld |
| `No access point is adopted` (debug) | Zero adopted APs — there is no WiFi here to be healthy. Not the same as `ok` |
| `wifi: warning` you cannot explain | The debug line names the numbers: `wifi wifi=warning adopted=3 disconnected=1 connected=2`. One of your APs is out of contact — `devices` and the per-device keys say which |
| `The console's own wlan status and the value derived from its AP counts disagree` | UniFi's own wording and the counts have parted company. The counts are what `wifi` reports. If this fires steadily, UniFi's `warning` means something the counts do not — worth reporting on [#9](https://github.com/robbeverhelst/unifi-reactor/issues/9) |

The same granularity applies to the UPS keys. `ups.runtime` is published only
when the UPS reports a `timeToRemain` above zero, and `ups.load` only when it
reports both an output and a non-zero budget — so a UPS that reports charge but
no runtime estimate publishes `ups` and `ups.battery` and withholds
`ups.runtime` alone. An Automation matching the withheld key goes
`StateKeyUnavailable` and **holds its claim**, which during a power failure is
the only safe answer: losing the estimate is not the outage ending.

If `ups.runtime` is missing while the UPS is plainly reporting everything else,
check `timeToRemain` in the device record directly. `0` and `-1` are both this
firmware's way of saying "no estimate", and both are treated as one:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'state observed'   # needs log.level=debug
```

---

## 10b. `devices` or a `device.<name>` key is missing or unexpected

`device.<name>` keys are **off by default**. If none of them appear, that is the
default doing its job — one key per adopted device is one metric series per
adopted device, so you have to ask:

```sh
helm upgrade ... --set unifi.devices.perDeviceKeys=true
kubectl -n reactor-system logs deploy/reactor | grep 'Per-device state keys are on'
```

The aggregate `devices` key is published either way. With `log.level=debug` one
line per poll says what the fleet looks like and names the devices behind it:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'device fleet'
# device fleet devices=degraded adopted=6 offline=1 offlineDevices=ap-attic=inactive perDeviceKeys=false
```

| What you see | What it means |
| --- | --- |
| No `devices` key at all | Nothing in the device list is adopted *and* reporting a recognisable state. `No adopted device reported a recognisable state` at debug level confirms it |
| A device you own is not in `offlineDevices` and has no key | `Skipping a device that is not adopted`, or `An adopted device reports no state`. An absent `state` is never read as offline |
| `A device reports a state this provider does not recognise` | Provisioning, upgrading or heartbeat-missed. It counts towards neither key on purpose — a firmware upgrade is not a fleet outage. **Please report the number**, it is what would extend the mapping |
| `Two or more devices share one key after slugifying their names` | `AP 1` and `ap-1` both want `device.ap-1`, so neither is published. Rename one on the console. `devices` still counts both |
| A key vanished and `Ready=False`/`StateKeyUnavailable` | The device was renamed, removed or unadopted. Reactor holds the last known state rather than firing `onExit`, which is why retitling a switch does not scale a workload back up. Update the Automation to the new slug |

A `device.<name>` key has no `reactor_state_info` series and never will — its key
name comes from your network, so it is not a metric label. Use
`status.observedState` on the Automation, or the debug line above.

## 10c. `firmware` never appears

The field it is derived from — `upgradable` — is **not in any capture this project
has**, so the parser is written to the shape UniFi documents and is unverified. It
is built to fail by publishing nothing rather than by publishing `current`:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-firmware'   # needs log.level=debug
# No adopted device reports whether it is upgradable; firmware will not be published devicesSilent=udmpro,ups-2u
```

If you see that line, your console names the field something else — or does not
report it — and [#12](https://github.com/robbeverhelst/unifi-reactor/issues/12) is
where the finding belongs. Dump one device record and look for it:

```sh
curl -sk -H "X-API-KEY: $UNIFI_API_KEY" \
  "$UNIFI_URL/proxy/network/api/s/default/stat/device" \
  | jq '[.data[] | {name, version, upgradable, upgrade_to_firmware, model_in_eol}]'
```

`devicesSilent` in the healthy version of that line is not a problem: the field is
per device type, and the devices that *do* answer are enough to publish the key.
Nothing silent is ever assumed to be current.

## 10d. `temperature` never appears, or reports `high` on a cool rack

Like `firmware`, this key is derived from fields **no capture in this project
contains**, so start by looking at what the parser actually saw:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-temperature'   # needs log.level=debug
# temperature temperature=normal hottestCelsius=58.5 hottestDevice=switch-48 thresholdCelsius=75 devicesInstrumented=4
```

| What you see | What it means |
| --- | --- |
| `No adopted device reports its thermals` | Nothing in the fleet reports `has_temperature`, a reading, or `overheating`. A UniFi UPS genuinely has none; if a switch or an AP is adopted, the field names differ on your firmware and [#11](https://github.com/robbeverhelst/unifi-reactor/issues/11) wants to know |
| `A device claims temperature reporting but published no reading` | Instrumented and silent. It keeps the key alive and contributes no number — it is **not** counted as 0 °C |
| `high` at a `hottestCelsius` that looks cool | Either the console set `overheating` (check `devicesOverheating` — its verdict outranks the threshold), or **the readings are not Celsius**. That unit is unverified. Compare `hottestCelsius` against what the UniFi UI shows for the same device |
| `normal` on a rack you know is hot | Your threshold is above what the hardware reports. Read `hottestCelsius` over a day, then set `unifi.temperature.highCelsius` a little above it |

Change the threshold and the debounce together. `temperature` settles over 3
samples, and the 75 °C default assumes a normal operating range of 40–60 °C; move
the threshold into that range and 90 seconds of hysteresis stops meaning anything,
because the reading crosses the line and stays there.

## 10e. `poe` never appears

The third parser written against fields no capture contains. Same first move:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-poe'   # needs log.level=debug
# poe poe=ok worstUtilizationPercent=32.5 worstSwitch=switch-48 draws=switch-48=63.5/195W
```

| What you see | What it means |
| --- | --- |
| `No adopted switch reports a readable PoE budget` with an empty `switchesUnreadable` | Nothing reports `total_max_power` and a `port_table`. A gateway, an AP and a UniFi UPS all legitimately report neither; if a PoE switch is adopted, the field names differ on your firmware and [#14](https://github.com/robbeverhelst/unifi-reactor/issues/14) wants to know |
| `switchesUnreadable=switch-48=port3(class Class 4) of 4 powered ports report no wattage` | A port is powering something and will not say how much, so that switch is left out entirely rather than counted as drawing nothing. Under-counting the draw would report headroom that is not there |
| `poe: ok` on a switch you know is full | Check `draws` against what the UniFi UI shows for the same switch. If the watts are far too low, `poe_power` is arriving in a form this parser did not expect — it accepts a number and a numeric string, and treats anything else as no reading |

```sh
curl -sk -H "X-API-KEY: $UNIFI_API_KEY" \
  "$UNIFI_URL/proxy/network/api/s/default/stat/device" \
  | jq '[.data[] | select(.total_max_power) | {name, total_max_power,
        ports: [.port_table[] | select(.poe_enable) | {port_idx, poe_power, poe_class}]}]'
```

That output is exactly what the parser reads. Post it on #14 — with the device
name removed — if it does not match what Reactor logged.

---

## 10f. An `outlet.<n>` key is missing, or is not the one you expected

Outlets are the one key in this batch whose fields are all in a real capture, so
a missing one is usually about *addressing* rather than about parsing:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-outlets'
# outlets device=ups-2u outlets=outlet.1=on,outlet.2=on,... needs log.level=debug
# relayGroups="1=[outlet.1 outlet.2 outlet.3 outlet.4] 2=[outlet.5 outlet.6 outlet.7 outlet.8]"
```

The grouping line is at INFO and appears whenever the grouping is first seen or
changes, so it is in the default log stream.

| What you see | What it means |
| --- | --- |
| No `unifi-outlets` line at all | No adopted device lists any outlet. Note that the captured gateway reports `"outlet_table": []` — having the field is not having outlets |
| The key is `outlet.5` and you expected `outlet.nas` | The outlet still carries the console's `Outlet 5` placeholder. Name it in the UniFi UI; any name of the form `Outlet <number>` is treated as the index spelled out, not as a name |
| The key was `outlet.nas` and is now gone | You renamed the outlet. The old key vanishing is lost visibility, so the last known state is **held** and `Ready=False` reports `StateKeyUnavailable` — nothing fires `onExit` because you relabelled a socket. Point the automation at the new key |
| `Two or more outlets are addressed by the same key` | Two outlets have the same name, or one is named after another's index. Neither is published, because picking one would be arbitrary and this key names something carrying mains power. Rename one |
| `outletsUnreadable=outlet.4=no relay_state` | That outlet will not say what position it is in. Absent is not off, so it publishes nothing rather than reporting an outage |
| `More than one adopted device reports an outlet table` | Outlet indexes restart on every chassis, so only the first device's outlets are published. Report it on [#23](https://github.com/robbeverhelst/unifi-reactor/issues/23), which has to decide how a second one is addressed before either can be switched |

### Reactor will not switch an outlet, and that is deliberate

There is no action, no flag and no allowlist for it. The captured UPS puts
outlets 1–4 in `relay_group: 1` and 5–8 in `relay_group: 2`, and nobody has
confirmed whether the hardware switches an outlet or a whole bank — if it is the
bank, "turn off outlet 3" means "cut outlets 1 to 4". See
[#23](https://github.com/robbeverhelst/unifi-reactor/issues/23).

If you have the console in front of you, you can settle it in a minute. Pick an
outlet in a bank carrying nothing you care about, toggle **one** outlet in the
UniFi UI, and read the next line Reactor logs:

```text
Outlet state changed. If you are running the relay-group experiment on issue #60, this line is its readout
  moved=outlet.5=on->off relayGroup=2 movedInGroup=1 outletsInGroup=4
  verdict="outlets in this group moved independently of each other"
```

`movedInGroup=1` of `4` means outlets switch individually. `4` of `4` means the
relay group is the switching unit. Either way, post it on
[#60](https://github.com/robbeverhelst/unifi-reactor/issues/60) — that one line
is what unblocks #23.

---

## 11. Reactor warns about your UniFi Network version

```text
INFO This UniFi Network version is newer than anything Reactor has been tested against;
     if state keys are missing, an incompatible API is the first thing to suspect
     version=11.0.0 supported="10.x (verified on 10.5.67)"
```

This is a warning, not a refusal — Reactor starts and polls normally. It is here so that
`no gateway reporting WAN ports and no UPS found in the device list` reads as an incompatibility
rather than as a configuration mistake, which is what it looks like otherwise.

If everything works, nothing needs doing, and a note on
[#43](https://github.com/robbeverhelst/unifi-reactor/issues/43) saying which console and version
worked is worth more than the warning is. If state keys *are* missing, the fields the parser
reads have probably moved, and a capture from your console
([`hack/capture-unifi.sh`](../testdata/unifi/README.md)) is what makes that fixable — it keeps an
allowlist of fields, so it is safe to run and share the result of.

`Could not determine the UniFi Network version` instead means the Integration API endpoint did
not answer: older Network releases do not serve it, and a console that is unreachable for the
first seconds of a pod's life looks the same. Reactor retries a few times and then carries on;
only the version report is lost, and the poller's own errors tell you if the console is really
unreachable.

The [compatibility matrix](../README.md#compatibility) is what these lines are checked against.

---

## 12. An edge action did not happen — or happened too often

Edge actions never fail the Automation — the desired-state action is the thing that had to happen, and a notification or a restart is not — so `Ready` stays `True` and the answer is in `status.edgeActions` and in the resource's Events:

```sh
kubectl -n media get automation notify-on-failover -o jsonpath='{.status.edgeActions}' | jq
kubectl -n media describe automation notify-on-failover | grep EdgeAction
```

| `reason` contains | What it means | Fix |
| --- | --- | --- |
| `outbound actions are disabled on this install` | `actions.allowedDestinations` is empty, which is the default | Add the destination to the chart value |
| `is not allowed by this install` | The destination is not on the allowlist. The message names it as `scheme://host:port` | Add exactly that, port included |
| `refusing to dial ... loopback` / `link-local` | The host resolved to an address that is refused whatever the allowlist says | Not a misconfiguration to work around; see [SECURITY.md](../SECURITY.md#outbound-actions) |
| `refusing to follow a redirect to` | The endpoint answered with a redirect. Redirects name a destination the allowlist never approved | Point the action at the final URL |
| `reading secret ... not found` | No credential Secret of that name in the **Automation's** namespace | Create it there; there is no cross-namespace read |
| `has no "url" key` | The Secret exists but carries no destination | Add `url` to it |
| `responded 4xx` | The endpoint rejected it. A 401 or 403 is a credential problem | Check the Secret's `url` and `authorization` |
| `rendering template` | A message referenced a field or state key that does not exist | `{{ .State.wan }}` errors on a typo, by design |
| `has no "authorization" key` | `homeassistant.service` found no token, and refuses to collect a 401 to find out | Add `authorization=Bearer <long-lived-token>` to the Secret |
| `did not render to a JSON object` | `homeAssistant.data` rendered to a list, a bare string or nothing | Service data is an object: `{"entity_id": "light.hall"}` |
| `set no "SID" cookie` | qBittorrent rejected the username or password. It answers a wrong one with `200 OK` and no cookie, not a 401 | Check the `username` and `password` keys in the Secret |
| `needs both a "username" and a "password"` | `qbittorrent.*` found only one of them | Both are required; an instance that bypasses authentication is an `http.request`, not this action |
| `responded 404 Not Found` on `/torrents/pause` | qBittorrent 5.0 deprecated `pause`/`resume` in favour of `stop`/`start`; an instance that removed them answers 404 | Report it — Reactor uses the compatible names deliberately |

An empty `status.edgeActions` when you expected one means the action never fired rather than failed. That is one of:

- **Nothing transitioned.** Edge actions fire on a change of `status.matching`, not on every reconcile. `kubectl get automation` shows the current value.
- **The workload could not be scaled.** A transition whose desired-state action failed is not committed, so nothing is announced until the retry succeeds — deliberately, so no message says a workload was paused while it is still running. Look at `status.lastExecution`.
- **The automation is suspended.** `spec.suspend: true` sends nothing, the same way a deleted one does not.
- **It was a deletion.** Deleting an Automation is not a state transition and fires no edge action.

A destination is only ever reported as `scheme://host:port`. That is not a truncation bug: for every notification transport the path is the credential, so it is kept out of status, logs and Events on purpose. A `kubernetes.restart` reports its target as `Kind/namespace/name` in the same field.

### A workload keeps restarting

`kubernetes.restart` is the only action where a repeat is harmful, so this has exactly one cause worth looking for: **the state key driving it is flapping.**

```sh
# how many times has this key actually changed value?
#   increase(reactor_state_transitions_total{key="wan"}[1h])
kubectl -n media describe automation restart-on-recovery | grep -E 'StateEntered|StateExited'
```

A pair of `StateEntered` / `StateExited` Events every poll is a flapping signal, not a restart bug. The engine only acts on transitions, so a *steady* condition never restarts anything twice — but each flap is a genuine transition and therefore a genuine rollout.

The fix is [debounce](../README.md#settling-a-noisy-signal), and it belongs on the key rather than on the automation, so that every automation still sees one settled value:

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values \
  --set unifi.debounce.keys.wan=3
```

Each extra sample costs one `pollInterval` of reaction time. The shipped default of `1` is chosen for `kubernetes.scale`, where a flap is harmless; a key that drives a restart should be raised above it. `spec.suspend: true` stops the restarts immediately while you decide.

Two things that are *not* the cause: a reconcile without a transition (edge actions do not fire), and a retry (a restart is attempted exactly once per transition and never retried).

---

## 13. Reactor is running but nothing is reacting

The failure this whole page cannot help with: Reactor is up, healthy, logging
nothing unusual, and has silently stopped observing. Every Automation holds its
last known state, no error is raised, and the next real outage is simply not
handled. There is nothing to grep for, because nothing went wrong loudly.

One number answers it, and it is why `metrics.enabled` exists:

```promql
time() - reactor_last_observation_timestamp_seconds
```

Above three poll intervals, Reactor is blind. `ReactorObservationStale` is that
query with a threshold on it, and it is the alert to wire up before any other.

Without metrics enabled, the same question by hand — the last line is the
answer, and its timestamp is what matters:

```sh
kubectl -n reactor-system logs deploy/reactor --timestamps   | grep -E 'state (observed|transition)' | tail -1
```

`state observed` needs `log.level=debug`; `state transition` only appears when
something changed, so on a quiet network it can be hours old and still healthy.
That ambiguity is exactly what the metric removes.

### Turning the endpoint on

```sh
helm upgrade reactor ... --set metrics.enabled=true
```

It serves HTTPS on `:8443` behind the API server's authn/authz filter, so a
scrape needs a token. Check it by hand from inside the cluster:

```sh
kubectl -n reactor-system exec deploy/reactor --   wget -qO- --no-check-certificate https://127.0.0.1:8443/metrics
# 401 — expected: the endpoint authenticates every scrape, including this one
```

| What you see | Cause |
| --- | --- |
| `connection refused` | `metrics.enabled` is not set, so the binary is not listening at all |
| Prometheus reports the target as `down` with a 401 or 403 | its ServiceAccount is not bound to the `<release>-metrics-reader` ClusterRole. The chart creates that role and deliberately does not bind it |
| target `down` with a TLS error | the endpoint's certificate is self-signed unless you issue one; the shipped ServiceMonitor sets `insecureSkipVerify` |
| the target is not discovered at all | `metrics.serviceMonitor.enabled` is off, or your Prometheus selects on labels the ServiceMonitor does not carry — see `metrics.serviceMonitor.labels` |
| the target scrapes but there are no series | a `networkPolicy` with the default `ingress: []` denies the scrape; the chart does not widen it for you |

### A state key you expected has no series

`reactor_state_info` is published **only for keys whose value set is closed**.
`isp` is deliberately absent: its values are carrier names, an open set, and one
series per carrier ever seen is how a Prometheus instance gets hurt. Read `isp`
off the Automation's `status.observedState` or its Events instead.

A key that *is* declared but shows `0` for every value has not been observed —
no UPS adopted, or the hardware dropped off. That is the metric side of
[`StateKeyUnavailable`](#2-statekeyunavailable-and-held-state), and it is a
different statement from "observed, and it is not that value".

### `reactor_provider_signal_disagreements_total` is climbing

Two independent signals for the same fact stopped agreeing. Nothing stops and
no state is withheld — see
[§10](#10-wan-and-isp-disagree-about-a-failover) for what each `signal` label
means and why these are reported rather than resolved. The counter exists so a
wrong `wan` derivation announces itself on a graph instead of waiting for
somebody to read the logs during an outage.

---

## 14. An automation is not acting, and is telling you what it would do

`Ready=True` with reason `DryRun` is not a fault. Something asked this automation to describe itself rather than run, and there are two separate things that could have:

```sh
# Is it this automation?
kubectl -n media get automation shed-on-battery -o jsonpath='{.spec.dryRun}'

# Or the whole install?
kubectl -n reactor-system get deploy reactor \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -o '\-\-dry-run'
```

They report differently, and the difference tells you which one you are looking at:

| | `spec.dryRun` on the automation | `safety.dryRun` on the install |
| --- | --- | --- |
| Where the answer is | `status.targets[].preview` | `status.targets[].effective`, unwritten |
| `Applied` message | "a dry run claims no target" | "this install runs as a dry run" |
| Effect on peers | none — it is out of force, so it is arbitrated as if absent | none — everything is in force and nothing is written |
| Metrics | — | `reactor_arbitrations_total{outcome="withheld"}` is the only outcome published |

`increase(reactor_arbitrations_total{outcome="withheld"}[1h])` is the fleet-wide version of the same question: a live install publishes none of these, and an install that thinks it is live but is not publishes nothing else.

**Reading a preview.** `preview.effective` is what the target would be held at with this automation's claim folded in; `preview.deferredBy` is who would still outvote it, `preview.wouldDefer` is who it would outvote, and `preview.onExit` is what it would hand back when its condition ended. It is computed whether or not the condition currently holds, on purpose — the automation you most want to check is the one for an outage.

It is not a forecast. The peers, the observed state and the target can all change before the condition holds, and nothing in a fold can predict whether the write would be *accepted*: RBAC, an admission webhook, a deleted target, and [a HorizontalPodAutoscaler already driving the field](#15-reactor-and-a-horizontalpodautoscaler-want-the-same-deployment) are all outside what it knows.

**Nothing was written, but the workload is still down.** Turning the install-wide dry run on does not release what Reactor was already holding, because releasing is a write too. Those workloads freeze where they are with their annotations intact — [§8](#8-a-workload-is-stuck-down-after-an-automation-was-deleted) is how to get them back, and the fix is to suspend or delete those automations before enabling `safety.dryRun`, not after.

---

## 15. Reactor and a HorizontalPodAutoscaler want the same Deployment

The symptom is a workload flapping on a fifteen-second cycle during an outage, or `Applied=False` with reason `TargetManagedByHPA` and no flapping at all — which of the two you get depends on whether detection is on.

**What is happening.** Reactor writes `spec.replicas`; an HPA computes one from metrics and writes it back. [§7](#7-two-automations-fighting-over-one-target) does not apply: arbitration resolves claims *between Automations*, because Reactor can see all of them, and an HPA is a claimant it cannot see. There is nothing to fold it into.

It is worth knowing this got *louder* rather than quieter with target ownership. Claims are re-asserted on every reconcile rather than once per transition, so what used to be a one-off flap is now a sustained oscillation. Same bug, more visible.

**Confirm it:**

```sh
kubectl -n media get hpa -o custom-columns=\
'NAME:.metadata.name,KIND:.spec.scaleTargetRef.kind,TARGET:.spec.scaleTargetRef.name'
```

Anything whose `TARGET` an Automation also names is in this fight.

**The fix is to turn detection on**, and it is worth doing before you need it:

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values --set safety.detectHPA=true
```

Reactor then lists the HPAs in a target's namespace before claiming it, and if one points at the target it writes nothing at all — not the replica count and not the baseline annotation, because a baseline captured from a value the HPA is actively changing would restore a meaningless number later. `status.targets[].managedBy` names the HPA, a Warning Event says so, `reactor_arbitrations_total{outcome="declined"}` counts it, and the Automation stays `Ready=True`. Its other targets are unaffected.

**A workload Reactor was already holding is handed back** to its baseline when an HPA appears over it, and then let go. That is deliberate and it is the case worth getting right: an HPA does not scale a workload up from zero, so a Reactor that simply went quiet while holding it at 0 would leave it there with neither controller willing to move it.

**Detection is on but every claim now fails.** The permission is missing:

```sh
kubectl auth can-i list horizontalpodautoscalers.autoscaling \
  --namespace media --as system:serviceaccount:reactor-system:reactor
```

Reactor fails closed here rather than writing blind, because an install that turned detection on has said it cares. `safety.detectHPA=true` grants it; a hand-written Role copied from an older chart will not have it.

**"I want Reactor to win during the outage."** There is no `force`, on purpose. Overriding means writing `spec.replicas` harder, which is the oscillation rather than a way out of it — the HPA syncs again in seconds. What would actually work is suspending the HPA by patching its `minReplicas`/`maxReplicas`, and that is *write* access to somebody's autoscaling policy, which is a much bigger permission and a separate decision. Today the answers are: remove or suspend the HPA, or shed load somewhere it does not reach — `kubernetes.cronjob.suspend` and `kubernetes.cordon` are unaffected by any of this.

**An empty `managedBy` is not a promise.** KEDA, a GitOps controller correcting drift, and a cron job running `kubectl scale` own `spec.replicas` just as hard, and none of them is discoverable through a stable API. An HPA is the common case and the one that can be seen; the general problem is not solvable by detection. If a workload flaps and no HPA names it, look for those next, and see [§9](#9-gitops-reactors-changes-look-like-drift).

---

## 16. Still stuck

Collect these and open an issue — the [bug report template](https://github.com/robbeverhelst/unifi-reactor/issues/new/choose) asks for exactly this, and without it nothing is reproducible:

```sh
# UniFi Network version and console model — UniFi UI → Settings → System

# Chart and image version
helm -n reactor-system list
kubectl -n reactor-system get deploy reactor \
  -o jsonpath='{.spec.template.spec.containers[0].image}'

# The Automation and its status
kubectl -n <ns> get automation <name> -o yaml

# Operator logs — ideally with log.level=debug set while reproducing
kubectl -n reactor-system logs deploy/reactor --tail=200
```

**Redact before posting.** Logs and resource dumps can contain your public IP, your ISP, internal hostnames, and site identifiers. Nothing in a bug report needs any of them.

## See also

- [Development](development.md) — running against the mock with `make dev-mock`, which is the fastest way to reproduce a state-transition problem without hardware
- [Adding a provider](adding-a-provider.md) — why keys are omitted rather than guessed, from the other side of the seam
- [Chart reference](../charts/reactor/README.md) — every value, both RBAC modes
