---
title: "A state key is missing, held, or not what you expected"
description: "StateKeyUnavailable and held state, wan and isp disagreeing about a failover, and one section per key that can go missing — internet, devices, firmware, temperature, poe, outlets."
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

**What it means.** The console has stopped answering. Not one key missing from a reply — [that is §2](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state) — but no successful reply at all since the timestamp in the message. A failed observation is logged and dropped, because the next poll is the recovery mechanism, so the state Reactor reports is simply the last one it got.

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

**Then fix the console, not the Automation.** The cause is in [§3](/troubleshooting/credentials-and-reachability/#3-credentials-and-reachability) — an expired API key, a rebooted gateway, a network policy, a certificate. Every failed attempt logs it:

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
| `is_uplink does not name a single live WAN port` | No port claimed the uplink, or more than one did. Reactor fell back to the gateway's uplink interface. | Nothing; this is the fallback working. On a failover to a **cellular** backup it is the expected path for as long as cellular carries the traffic — a cellular uplink never reports `is_uplink` at all, so the uplink interface is the signal that resolves it ([#104](https://github.com/robbeverhelst/unifi-reactor/issues/104)). On all-wired gateways it is worth reporting if it persists rather than appearing for one poll during a switchover |
| `The health endpoint accumulated uptime on an uplink other than the one wan names` | A **third** signal, from a different endpoint, disagrees — and the strongest one, because uptime is traffic the console watched pass rather than a statement about configuration. | This is the most useful thing you can report on [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34). Post the `uptime_stats` block alongside every `wanN` block's fields |

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
| Neither key ever appears, and no line above | `wan` itself is not derivable, which withholds `wan.quality` too — `internet` should still be there. Start at [§13](/troubleshooting/nothing-is-happening/#13-reactor-is-running-but-nothing-is-reacting) |

`wifi` comes from the same response and degrades the same way. It is derived from
the `wlan` subsystem's AP counts rather than from its `status` string, so:

| What you see | What it means |
| --- | --- |
| `The wlan subsystem reports no AP counts` (debug) | `num_adopted` or `num_disconnected` is missing. Neither is read as zero, so the key is withheld |
| `No access point is adopted` (debug) | Zero adopted APs — there is no WiFi here to be healthy. Not the same as `ok` |
| `wifi: warning` you cannot explain | The debug line names the numbers: `wifi wifi=warning adopted=4 disconnected=1 connected=3`. One of your APs is out of contact — `devices` and the per-device keys say which |
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

## 10g. `wan` disappeared during a failover

The symptom: `wan` read `primary`, the gateway failed over, and instead of
`backup` the key *vanished* — `StateKeyUnavailable` on every Automation
matching it, and a `when: {wan: backup}` Automation that never fired. A
missing key holds the last decision (see
[§2](#2-statekeyunavailable-and-held-state)), so the automation written for
exactly this failover stayed quiet through it: the last thing it observed was
"not matching, the primary is fine".

On releases carrying the fix for
[#104](https://github.com/robbeverhelst/unifi-reactor/issues/104) the known
cause is gone: a gateway with a cellular backup reports it as a **third** WAN
(`wan3`), Reactor used to decode exactly two, and on failover the live uplink
matched nothing it had decoded. Every `wanN` the gateway reports is now
collected, so that failover reads `backup`.

If the key still disappears on a current release, the gateway is uplinked
through an interface that matches no `wanN` entry at all — a combination this
mapping has not seen. Confirm, then report it:

```sh
kubectl -n reactor-system logs deploy/reactor | grep "wan will not be published"
```

Post the gateway's `uplink.name` and the `ifname` of every `wanN` block on
[#104](https://github.com/robbeverhelst/unifi-reactor/issues/104), with
addresses removed.

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
([`hack/capture-unifi.sh`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md)) is what makes that fixable — it keeps an
allowlist of fields, so it is safe to run and share the result of.

`Could not determine the UniFi Network version` instead means the Integration API endpoint did
not answer: older Network releases do not serve it, and a console that is unreachable for the
first seconds of a pod's life looks the same. Reactor retries a few times and then carries on;
only the version report is lost, and the poller's own errors tell you if the console is really
unreachable.

The [compatibility matrix](/concepts/when-reactor-cannot-see/#compatibility) is what these lines are checked against.

---
