---
title: "An action did not happen, or happened twice"
description: "onExit firing when you did not expect it, and edge actions — notifications, HTTP requests, console writes — that did not arrive or arrived more often than they should have."
---

## 4. `onExit` fired — or did not — when you did not expect it

`onExit` declares **the level you want while the condition does not hold**, not a script that runs at the moment of exit. Most surprises come from reading it the second way.

**It fired and you expected nothing.** A key you match on changed value. With `when: {ups: on-battery, ups.battery: critical}`, a battery recovering from 8% to 15% moves `ups.battery` from `critical` to `low`, the condition stops holding, and the reversal applies — even though the power is still out. That is the semantics working; the fix is matching only the keys you actually care about.

**It fired the moment you suspended the Automation.** That is the same semantics again: suspending takes the policy out of force, so the target is arbitrated as if the Automation were gone, and the reversal applies even though the condition still holds — `status.matching` stays `true` and says so. See [pausing an automation](/operations/suspend-and-dry-run/#pausing-an-automation) for why that matches what deleting does, and for the `reversal: None` patch that freezes the workload instead.

**It did not fire and you expected it to.** Three candidates, in order:

1. The condition never stopped holding. Check `status.matching` and `status.observedState`.
2. A key went missing rather than changing, so state is held — see [§2](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state).
3. Another Automation still claims the same target and its claim wins. See [§7](/troubleshooting/conflicts-and-drift/#7-two-automations-fighting-over-one-target).

**You omitted `onExit` and the workload came back anyway.** From v1, an Automation with no `onExit` restores the target's recorded baseline when it stops matching, rather than leaving the workload wherever it was put. A workload that never comes back was judged the worse default. To get the old behaviour — Reactor sets it and never touches it again — set `spec.reversal: None` explicitly.

> The `reversal` field and baseline restore land with the target-ownership change. On v0.3.0, an omitted `onExit` leaves the target untouched on exit.

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
| `refusing to dial ... loopback` / `link-local` | The host resolved to an address that is refused whatever the allowlist says | Not a misconfiguration to work around; see [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#outbound-actions) |
| `refusing to follow a redirect to` | The endpoint answered with a redirect. Redirects name a destination the allowlist never approved | Point the action at the final URL |
| `reading secret ... not found` | No credential Secret of that name in the **Automation's** namespace | Create it there; there is no cross-namespace read |
| `has no "url" key` | The Secret exists but carries no destination | Add `url` to it |
| `responded 4xx` | The endpoint rejected it. A 401 or 403 is a credential problem | Check the Secret's `url` and `authorization` |
| `rendering template` | A message referenced a field or state key that does not exist | `{{ .State.wan }}` errors on a typo, by design. Nearly always caught before this, as `TemplateWillNotRender` — see [§12a](#12a-templatewillnotrender) |
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

The fix is [debounce](/concepts/settling-a-noisy-signal/), and it belongs on the key rather than on the automation, so that every automation still sees one settled value:

```sh
helm upgrade reactor oci://ghcr.io/robbeverhelst/charts/reactor \
  --namespace reactor-system --reuse-values \
  --set unifi.debounce.keys.wan=3
```

Each extra sample costs one `pollInterval` of reaction time. The shipped default of `1` is chosen for `kubernetes.scale`, where a flap is harmless; a key that drives a restart should be raised above it. `spec.suspend: true` stops the restarts immediately while you decide.

Two things that are *not* the cause: a reconcile without a transition (edge actions do not fire), and a retry (a restart is attempted exactly once per transition and never retried).

---

## 12a. `TemplateWillNotRender`

`Ready=False` with this reason means a template on the Automation reads something the template context can never carry, so the action holding it would fail at the moment it fired. It is reported when the object is reconciled — which is when you applied it — rather than on the transition weeks later that the message was written for:

```sh
kubectl -n media get automation notify-on-wan-failover \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
# spec.actions[0].notification.message references state key "isp", which this automation
# does not match on (it matches on wan); add isp to spec.when.state or remove the reference
```

The message names the field, the reference and both ways out. The common case is the one above: **`.State` carries the keys in `spec.when.state` and nothing else**, so an Automation triggered on `wan` cannot read `{{ .State.isp }}` however plainly `isp` shows up in `status.observedState`. The narrowing is deliberate — a template can only ever repeat back something its author already asked for — so the fix is to match on the key as well, which narrows the condition, or to drop the reference.

The other things it reports, all of them the same failure at the same moment:

| Message says | Means |
| --- | --- |
| `references state key "…"` | a key not in `spec.when.state`, including a misspelling of one that is |
| `reads .X, which is not part of the template context` | a field the context does not have. It lists the ones it does |
| `reads .Key.X, but .Key is a string` | a field read off a value that has none |
| `does not parse` | a syntax error, or a function that does not exist. The only functions are the standard ones plus `json` |

Three things it deliberately stays quiet about, because it cannot decide them and a false accusation is worse than the trap: the body of a `range` or `with` block, where dot is something else (`$.State.wan` inside one is still checked), a reference through a variable you assigned, and `{{ index .State "isp" }}` — which renders an empty string rather than failing, and so is not this problem.

**It does not stop the Automation.** Targets are still claimed and released, `onExit` still applies, and everything that is not the broken template still runs. A typo in a notification is not a reason to skip the failover it was reporting. If the Automation only has edge actions, nothing else happens anyway — but the object is still reconciling, and `Ready` goes back to `Reconciled` the moment the template or the trigger is corrected.

---
