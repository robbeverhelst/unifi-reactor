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

## What has never been observed

Parsers are written against real captured API responses committed to [`testdata/`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/), never against assumed formats. Two caveats worth stating plainly.

**A genuine WAN failover has still never been observed** ([#34](https://github.com/robbeverhelst/unifi-reactor/issues/34)). `wan` is derived from which port reports `is_uplink`, inferred from one capture in which only one uplink was live — so whether `is_uplink` follows the traffic or just marks the port configured as primary is unconfirmed. What has changed is that the guess is no longer silent or alone: the gateway's own uplink interface is used as a second opinion where `is_uplink` names no single live port, `isp` (from #6) is compared against `wan` across observations, and any disagreement between them is logged rather than resolved. The provider is exercised against five different hypotheses about what a failover looks like, in tests and in `make dev-mock`, and it reports something defensible under all of them. That is not the same as knowing. Treat `wan` as less battle-tested than `ups`, watch for the [disagreement warnings](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover), and if you have a gateway with two working uplinks, the [capture runbook](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#capturing-a-real-failover) is fifteen minutes that would close this.

And the webhook fast path has been exercised against the mock console, not a real one — which is a large part of why it defaults off and why nothing depends on it being right.
