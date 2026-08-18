---
title: "Fleet and device state keys"
description: "devices, device.<name>, firmware, temperature, wifi and poe: what the console knows about your hardware, why per-device keys are opt-in, and what each key refuses to guess."
---

## The fleet: `devices`, and why `device.<name>` is opt-in

An access point can sit dead for days with nothing telling you. `devices` is the
one-value answer to "is anything down": `all-online` while every adopted device
is in contact with the console, `degraded` the moment one is not.

```yaml
  when:
    provider: unifi
    state:
      devices: degraded    # something in the rack stopped answering
```

`device.<name>` is the same observation per device, keyed by the device's name
lowercased with everything non-alphanumeric turned into a hyphen — `US 48`
becomes `device.us-48`. **It is off by default**, and that is a deliberate
asymmetry rather than caution:

```yaml
unifi:
  devices:
    perDeviceKeys: true    # one key, and one metric series, per adopted device
```

Every other key here is bounded by what is compiled in. `device.<name>` is the
first whose *name* comes from your network, so turning it on means one state key,
one `reactor_state_transitions_total` series, and one more thing an Automation
can hold state for **per adopted device** — forty devices, forty of each. The
aggregate costs one series whatever the fleet size, so it ships on and the
per-device keys are something you ask for. Reactor logs at startup when they are
on, and `unifi.devices.perDeviceKeys` is the only setting on this page that
changes how *much* Reactor publishes rather than what it means.

Per-device keys are also never labelled by value in Prometheus, for the same
reason `isp` is not — see [Cardinality](/operations/metrics-and-alerts/#cardinality-on-purpose). Which device is down
is in the Automation's `status.observedState`, in an Event, and in a `V(1)` log
line naming the device and the console's own disconnection reason.

Three things are excluded on purpose. **Unadopted and pending devices**: the
console can see your neighbour's AP, and it is not your fleet. **A device in a
transient state** — provisioning, upgrading and heartbeat-missed are recognised
explicitly, and provisioning has been observed on real hardware, on three device
classes at once during a config push — because reading any of them as `offline`
would report a firmware upgrade as a fleet outage, and a device mid-provision is
genuinely neither online nor offline. That exclusion is a decision about known
states, logged at `V(1)`; a state this provider has never seen is a separate
case, still counted towards neither key, and still logged at INFO asking to be
reported. **A device reporting no state at all**, which is absence, not zero.

Renaming a device on the console makes its old key *vanish* rather than report
`offline`, which Reactor treats as lost visibility: the last known state is held
and `Ready=False` reports `StateKeyUnavailable`, so nothing fires `onExit`
because you retitled a switch. The same is true of a device you remove. And two
devices whose names slugify to the same key — `AP 1` and `ap-1` — publish
*neither*, because picking one would be arbitrary and the arbitrary pick could be
the one hiding the dead device. `devices` still counts both.

## `firmware` turns "I should check for updates sometime" into something that pages

```yaml
  when:
    provider: unifi
    state:
      firmware: updates-available
  then:
    - type: notification.ntfy      # or http.request, to open a ticket
```

One key for the whole fleet: `updates-available` while the console has an update
waiting for **any** adopted device, `current` when it does not. Which devices, and
what would move from which version to which, is a `V(1)` log line — a version
string is not something `spec.when` could match, and one metric series per version
is the cardinality failure [`isp` would have been](/operations/metrics-and-alerts/#cardinality-on-purpose).

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-firmware'   # needs log.level=debug
# firmware firmware=updates-available devicesReporting=6 devicesUpgradable=ap-attic=6.6.65->7.0.50 modelsPastEOL=1
```

**Reactor will not upgrade anything.** Observing is in scope and applying is not:
a firmware upgrade reboots hardware, and an operator choosing when that happens is
the whole point of being told it is available.

A device that does not report the field is not a device that is up to date, so a
console where *nothing* answers publishes no `firmware` key at all rather than
`current` — and the committed captures are exactly that case, which is why this
parser is [not yet verified](/concepts/when-reactor-cannot-see/#compatibility). Devices that stay silent while
others answer are named in the same log line rather than assumed current.

`model_in_eol` is read and deliberately **not** published as a key, though it is
arguably the more valuable fact. It is an inventory property rather than a state:
it does not transition, so an Automation matching it would sit permanently
matched, which is a report rather than a reaction. It is counted in the log line
above, and a key for it is a decision to argue for separately.

## `temperature` is the hottest device, bucketed

A switch cooking in a warm rack degrades before it fails. `temperature` reports
`high` when the hottest adopted device is at or above a threshold **you
configure**, or when the console itself says a device is overheating:

```yaml
unifi:
  temperature:
    highCelsius: 75
```

```yaml
  when:
    provider: unifi
    state:
      temperature: high    # stop the transcode job before the rack gets worse
```

The console's own `overheating` flag outranks the threshold: the firmware knows
what that model tolerates and a number in this repository does not. Otherwise the
hottest *sensor* on the hottest *device* decides — a board is as hot as its
hottest part, and averaging would let one cool sensor hide a cooking one.

Like [`wan.quality`](/state-keys/wan-and-internet/#internet-is-the-one-wan-cannot-express) and `ups.load`, this
is a **bucketed measurement**, and for the same two reasons: `spec.when` compares
strings, so a number cannot be a state value, and one metric series per distinct
reading is unbounded. The readings themselves are a `V(1)` log line — which is
where you find out what your rack actually runs at before setting the threshold:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-temperature'   # needs log.level=debug
# temperature temperature=normal hottestCelsius=58.5 hottestDevice=switch-48 thresholdCelsius=75 devicesFanless=2
```

The default of 75 °C is set **against the debounce**, not in isolation, the same
way [`ups.runtime`'s thresholds are](/state-keys/power-and-ups/#charge-is-a-poor-shutdown-trigger-runtime-is-a-better-one): UniFi switches and APs
normally sit at 40–60 °C, so 75 °C plus 3 samples means a reading that genuinely
held for 90 seconds rather than a fan spinning up late. Lower it towards the
normal operating range and that hysteresis stops meaning anything, because the
reading will cross the line and stay there. Move one and you have moved the other.

A device reporting no temperature is **not a device at 0 °C** — it publishes
nothing, and a fleet where nothing is instrumented publishes no `temperature` key
at all. Reading a missing sensor as zero would make the rack look coldest exactly
when a sensor stops answering.

## `wifi` is the subsystem, not any one AP

`devices: degraded` says something in the rack stopped answering. `wifi` says how
much of your *WiFi* is left, which is a different question and often the one that
matters to whoever is complaining:

| Value | When |
| --- | --- |
| `ok` | every adopted access point is connected |
| `warning` | at least one is disconnected, but not all of them |
| `error` | **every** adopted access point is disconnected |

It is derived from the console's own `num_disconnected` and `num_adopted` counts
rather than from the `wlan` subsystem's `status` string, and #9 asked for that
choice to be documented rather than left mysterious. The counts are a fact that can
be explained — "1 of 3 access points is disconnected" is an answer; "UniFi said
warning" is not — and they make `error` *derivable*, where mapping the vendor
string through would have been inference: no capture has ever shown any subsystem
saying `error`. In the one capture there is, the two agree exactly (`warning`, with
1 of 3 APs disconnected), so this sharpens the console's verdict rather than
contradicting it.

The status string is still read, and a mismatch is counted as
`reactor_provider_signal_disagreements_total{signal="wifi-status-disagrees"}` and
logged. If UniFi's `warning` turns out to mean something else — airtime, channel
interference — that counter rises instead of the derivation quietly being wrong.

A site with **no** adopted access points publishes no `wifi` key: there is no WiFi
there to be healthy, which is not the same as WiFi that is fine.

The three values are alternatives, not steps of a ladder — the same shape
[`internet`](/state-keys/wan-and-internet/#internet-is-the-one-wan-cannot-express) has. An automation matching
`wifi: warning` **stops** matching when the value moves to `error`, and reverses,
because those are two different values of one key. Match the value you mean, or
write one automation per value:

```yaml
  when:
    provider: unifi
    state:
      wifi: error         # every access point is gone, not just one
```

This is not the `ups`/`ups.battery` trap it resembles. There, one *fact* (on
battery) escalated into a second fact (the charge), and splitting them was the
fix. Here all three values answer a single question — how much WiFi is left — so
one key with three values is the right shape, and choosing between them is the
automation author's job.

## `poe` is headroom, before it becomes an outage

An overloaded PoE budget silently drops APs and cameras. `poe` reports
`insufficient` when the worst switch is delivering at or above a share of its
budget **you configure**:

```yaml
unifi:
  poe:
    maxUtilizationPercent: 90
```

`insufficient` means *the headroom is gone*, not that a port has already been
denied power — by the time the console refuses a port, the camera is off. The
worst switch decides, because one switch out of headroom drops the cameras on
that switch whatever the rest of the rack has spare.

Another [bucketed measurement](#temperature-is-the-hottest-device-bucketed), with
the watts in a `V(1)` log line:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'unifi-poe'   # needs log.level=debug
# poe poe=ok worstUtilizationPercent=32.5 worstSwitch=switch-48 draws=switch-48=63.5/195W
```

The interesting rule here is what happens to a port that is **powering something
and will not say how much**: it makes that whole switch unreadable, and the switch
is left out rather than counted as drawing nothing. Counting it as 0 W would report
headroom that is not there — under-counting the draw is the one direction that
hides the exact failure this key exists to catch. Other switches are still
measured, so one unreadable switch does not take the key with it.

A switch reporting no budget is likewise not a switch with no budget, and never a
denominator. A fleet with no readable PoE switch publishes no `poe` key at all.
