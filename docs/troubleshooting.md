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
| `ProviderStateUnavailable` | No state has been observed yet for this provider. | [§1](#1-nothing-happens-when-the-state-changes) |
| `StateKeyUnavailable` | A key this Automation needs vanished from the observation. Last known matching state is held. | [§2](#2-statekeyunavailable-and-held-state) |
| `ActionFailed` | An action returned an error. `status.lastExecution.reason` has the message. | [§5](#5-rbac-refuses-a-cross-namespace-target), [§6](#6-the-crd-invalid-ownership-metadata-or-a-stale-schema) |

An Automation left over from before `spec.trigger` was removed has no conditions at all: `spec.when` is now required, so the API server rejects any write to it, status included. It is reported once per reconcile in the operator log and as a Warning Event on the resource instead:

```sh
kubectl -n media describe automation notify-on-client-connect | tail -5
# Warning  EventTriggerRemoved  spec.trigger was removed from v1alpha1 and was never
#          implemented; this automation does nothing, delete it
```

Delete it. Event triggers never ran on any version, so nothing is lost, and nothing it referenced was ever claimed. See the README's [Stability](../README.md#stability) section for when they come back.

> On v0.3.0, `ActionFailed` is reported with `status: "True"` — a bug where the condition status was not flipped alongside the reason. Read the *reason*, not the status, on that version. Fixed in the target-ownership batch.

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

Two ways out, and the second is usually better in a homelab you did not want cluster-wide RBAC in:

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
# reactor.robbeverhelst.com/baseline-replicas: "1"
# reactor.robbeverhelst.com/claimed-by: "power/shed-on-battery,net/pause-on-backup-wan"
# reactor.robbeverhelst.com/claimed-at: "..."
```

`baseline-replicas` is what Reactor found before it first claimed the target, and it is what a reversal restores. `claimed-by` and `claimed-at` are advisory — refreshed each reconcile, never read back as truth — and exist so `kubectl describe deploy` explains the zero to a human at 3am.

**A scale-*up* Automation loses to any scale-down claim on the same target**, because `min` encodes "most restrictive wins". `status.targets[].effective` makes it visible instead of silent. If you need the opposite, that is a design conversation, not a misconfiguration.

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

None of these stops anything: state is still published and Automations still run. They exist
because the `wan` mapping has never been checked against a real failover, and a wrong mapping
that says nothing is far worse than one that complains. If you have a gateway with two working
uplinks, [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34) is where these lines
turn into an answer.

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

## 12. A notification or HTTP request did not arrive

Outbound actions never fail the Automation — the scale is the thing that had to happen, the notification is the report of it — so `Ready` stays `True` and the answer is in `status.edgeActions` and in the resource's Events:

```sh
kubectl -n media get automation notify-on-failover -o jsonpath='{.status.edgeActions}' | jq
kubectl -n media describe automation notify-on-failover | grep EdgeAction
```

| `reason` contains | What it means | Fix |
| --- | --- | --- |
| `outbound actions are disabled on this install` | `actions.allowedDestinations` is empty, which is the default | Add the destination to the chart value |
| `is not allowed by this install` | The destination is not on the allowlist. The message names it as `scheme://host:port` | Add exactly that, port included |
| `refusing to dial ... loopback` / `link-local` | The host resolved to an address that is refused whatever the allowlist says | Not a misconfiguration to work around; see [SECURITY.md](../SECURITY.md#outbound-actions-http-request-and-notification) |
| `refusing to follow a redirect to` | The endpoint answered with a redirect. Redirects name a destination the allowlist never approved | Point the action at the final URL |
| `reading secret ... not found` | No credential Secret of that name in the **Automation's** namespace | Create it there; there is no cross-namespace read |
| `has no "url" key` | The Secret exists but carries no destination | Add `url` to it |
| `responded 4xx` | The endpoint rejected it. A 401 or 403 is a credential problem | Check the Secret's `url` and `authorization` |
| `rendering template` | A message referenced a field or state key that does not exist | `{{ .State.wan }}` errors on a typo, by design |

An empty `status.edgeActions` when you expected one means the action never fired rather than failed. That is one of:

- **Nothing transitioned.** Edge actions fire on a change of `status.matching`, not on every reconcile. `kubectl get automation` shows the current value.
- **The workload could not be scaled.** A transition whose desired-state action failed is not committed, so nothing is announced until the retry succeeds — deliberately, so no message says a workload was paused while it is still running. Look at `status.lastExecution`.
- **The automation is suspended.** `spec.suspend: true` sends nothing, the same way a deleted one does not.
- **It was a deletion.** Deleting an Automation is not a state transition and fires no edge action.

A destination is only ever reported as `scheme://host:port`. That is not a truncation bug: for every notification transport the path is the credential, so it is kept out of status, logs and Events on purpose.

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

## 14. Still stuck

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
