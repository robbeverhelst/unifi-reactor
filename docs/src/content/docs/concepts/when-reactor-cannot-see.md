---
title: "When Reactor cannot see"
description: "Lost visibility is not a condition that ended: Reactor holds the last known state and says so. Plus the compatibility matrix, and the three parsers written against docs rather than a capture."
---

## Holding the last known state

If a provider stops reporting a key at all — the hardware dropped off the controller — Reactor holds the last known state and reports `Ready=False` with `StateKeyUnavailable` rather than treating lost visibility as a condition that ended ([what to do about it](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state)).

## Compatibility

Everything here was built against one setup, and this table says which one. "Verified" means a real capture or a real cluster; "expected" means the code path is version-independent as far as anyone can tell, which is not the same thing.

| | Verified | Expected to work | Known not to work |
| --- | --- | --- | --- |
| UniFi Network | 10.5.67 | 10.x | — |
| Console | UDM Pro (gateway firmware 5.1.26) | UDM/UDM SE/UDR/UXG, Cloud Key with a gateway adopted. A site with no gateway and no UniFi UPS now observes the fleet keys — `devices`, `wifi` and whichever of `firmware`/`temperature`/`poe` the hardware reports — but none of `wan`, `isp` or `ups` | a site with nothing adopted at all: nothing to observe |
| UPS | UniFi UPS 2U (`USWDA26`, firmware 1.6.1) | any UniFi UPS reporting `vbms_table` | third-party UPS over NUT — a separate provider, not this one |
| Kubernetes | CI: envtest 1.36 API server, and the current kind default node image for e2e | 1.25+ — only long-stable APIs are used (`apps/v1` scale, `policy/v1`, `apiextensions/v1`, leases) | — |
| Helm | 3.x | — | — |

Reactor asks the console what it is running and says so at startup:

```sh
kubectl -n reactor-system logs deploy/reactor | grep -E 'version detected|tested against'
# INFO UniFi Network version detected version=10.5.67 verifiedAgainst=10.5.67 verifiedConsole="UDM Pro"
# INFO Kubernetes version detected version=v1.34.1
```

Outside the range above it warns and **carries on**. Refusing to start against a console that would have worked fine is a worse failure than a log line, and most of them will work fine — the warning exists so that a missing state key reads as an incompatibility rather than as a configuration mistake. If your console is not in the table and it works, [say so](https://github.com/robbeverhelst/unifi-reactor/issues/new/choose): every row here started as somebody's report.

State keys degrade one at a time, so a console with no UniFi UPS still reports `wan` and `isp`, and a gateway whose fields have moved still reports `ups`. That holds across endpoints too: an observation reads `stat/device` and `stat/health`, and a console that answers one but not the other publishes the keys it can. Only observing nothing at all is an error.

### Three keys are parsed against a documented shape, not a capture

Every parser here is written against a real captured response — except three, and this is where that is said plainly rather than in a commit message:

| Key | Fields it needs | Status |
| --- | --- | --- |
| `firmware` | `upgradable`, `upgrade_to_firmware`, `model_in_eol` | **no capture contains them.** The committed records carry `version` and nothing else about upgrades |
| `temperature` | `has_temperature`, `overheating`, `temperatures[]`, `general_temperature` | **no capture contains them.** The UniFi UPS 2U reports no thermals at all |
| `poe` | `total_max_power`, `port_table[].poe_power` | **no capture contains them.** No switch record exists in this repository |

They are written to the shape UniFi's own API documents, every field is in the [capture allowlist](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md) so the next real capture settles them, and each fails by **publishing nothing** rather than by publishing a reassuring value: no `upgradable` anywhere means no `firmware` key, not `current`; no thermals means no `temperature` key, not `normal`; an unreadable switch is left out rather than counted as having headroom.

If you run a UniFi switch or an access point, `./hack/capture-unifi.sh` now writes `stat-device-switch.json` and `stat-device-ap.json`, and one of each would settle all three at once.

`outlet.<n>` is the opposite case and is listed here so the contrast is not lost: every field it reads — `index`, `name`, `relay_state`, `relay_group` — **is** in the committed capture, and the parser is written against real bytes. What is unverified about outlets is not the reading but the *writing*, which is why there is none.

## What has and has not been observed

Parsers are written against real captured API responses committed to [`testdata/`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/), never against assumed formats. Two things worth stating plainly.

**A genuine WAN failover has now been observed** ([#34](https://github.com/robbeverhelst/unifi-reactor/issues/34), closed as verified). On 2026-08-18 the primary uplink was physically unplugged for 75 seconds and the console failed over to a cellular backup and back: `wan` moved from `primary` to `backup` and back to `primary`, and an Automation watching it claimed its target, scaled a Deployment to zero, and restored it to the replica count it held before, from the baseline annotation. The more interesting finding is which signal did the work. `is_uplink` — the signal of record — did not resolve the failover and structurally cannot on this hardware: a cellular uplink's record carries no `is_uplink` field at all, so across the whole outage the log read `is_uplink does not name a single live WAN port`, and the gateway's own uplink interface name — written as a fallback — is what produced `backup`. On cellular hardware that fallback is not a fallback; it is the only signal that answers. What remains unobserved is a wired-to-wired failover: that gateway's second wired WAN has nothing plugged into it, so the only failover it can produce goes to cellular, and whether `is_uplink` moves cleanly when one wired port takes over from another is a question no hardware on hand can answer. The [disagreement warnings](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover) still exist for exactly that reason, and the [capture runbook](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#capturing-a-real-failover) is still how a wired-to-wired failover would be captured.

And the webhook fast path has been exercised against the mock console, not a real one — which is a large part of why it defaults off and why nothing depends on it being right.
