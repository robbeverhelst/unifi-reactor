---
title: "Settling a noisy signal"
description: "Debounce: how many consecutive observations a changed value must hold for before Reactor believes it, why every key ships with the number it does, and what each extra sample costs."
---

## Settling a noisy signal

A changed value can be required to hold for several consecutive observations before Reactor acts on it, which stops one flapping signal driving repeated actions:

```yaml
unifi:
  debounce:
    default: 1          # react on the first observation
    keys:
      ups.battery: 2    # ...but let a threshold crossing settle
      ups.runtime: 2
      ups.load: 3       # ...a live wattage moves second to second
      isp: 2            # ...and let a re-geolocated carrier settle
      internet: 3       # ...and don't believe one bad probe round
      wan.quality: 3
      devices: 2        # ...and don't believe one missed heartbeat
      device.*: 2       # a trailing * covers keys named after your hardware
      firmware: 3       # ...and nothing about an update is urgent
      temperature: 3    # ...and a measurement hovers on its threshold
      wifi: 2           # ...and an AP heartbeat can miss a beat
      poe: 3            # ...and a PoE draw moves when a radio comes up
      outlet.*: 1       # a relay is a switch position; there is nothing to settle
      data.usage: 1     # the console already settled this against the real plan
```

Each extra sample costs one `pollInterval` of reaction time, so the default is `1`: a WAN failover and a power cut both deserve an immediate reaction, and neither flaps. `ups.battery` ships at `2` because it is a threshold crossing — a charge hovering at 30% would otherwise report `low`, `normal`, `low` — and because a battery drains over minutes, so spending one more poll to be sure costs nothing. At the default 30s poll that makes a battery-level escalation react in 60s worst case instead of 30s.

`isp` ships at `2` for a different reason: it is not a link state but the result of a geolocation lookup on whatever public address the gateway currently holds, so it can report `unknown` for a poll or two while a new address is being resolved — precisely during the failover you would be reacting to. One extra sample skips that window. `wan` and `ups` need none of this: they are switch positions, and they do not flap.

`ups.runtime` matches `ups.battery` at `2`: it is the same kind of escalation, and its default thresholds are set against that delay rather than in isolation — 2 samples is 60s at the default poll, and a `critical` threshold of 180s leaves two minutes between Reactor believing the reading and the UPS running out. Move one and you have moved the other.

`ups.load` is the exception at `3`, because it is the only key derived from an *instantaneous* measurement: a server spinning up shifts the draw by a few hundred watts in one poll, where a battery drains monotonically over minutes. A momentary burst past 80% must not be a reason to shed load.

`internet` and `wan.quality` ship at `3`, the highest in the chart, because they are the two keys derived from probes to the outside world rather than from anything on your desk. A single poll in which a probe target rate-limits or a resolver blips must not shed a cluster's load. At the default 30s poll that is 90 seconds before either an outage or a recovery is believed — deliberately symmetric, because a link flapping in and out is exactly when repeatedly scaling workloads up and down does the most damage.

Debounce is also the whole of the flap control for `wan.quality`, and that is worth being explicit about, because bucketing a measurement is where a threshold usually needs hysteresis. It does not need it here: debounce promotes a value only after N *consecutive* identical observations, so a measurement hovering on a threshold produces `good`, `degraded`, `good` and is never promoted at all — the key simply holds what it had. A second, differently-shaped flap control in the provider would be a second thing to reason about for a problem the engine already solves for every key.

`devices` and `device.*` ship at `2`. A device's state is a switch position like `wan`, but it is the *console's* judgement about a heartbeat rather than a wire it can see, and a busy console that misses one beat must not be a reason to page anyone. One extra sample is 60 seconds at the default poll, which is nothing against the failure this key exists for — an AP that has been dead for days.

`firmware` ships at `3`, and here the reason is that nothing is lost by it. The key is not derived from your hardware at all but from the console's lookup against Ubiquiti's release catalogue, which can refresh, blip or briefly disagree with itself — and *no* firmware update needs reacting to within 30 seconds. Extra samples are free when reaction time does not matter, so it takes the most of any key.

`temperature` ships at `3` because it is a measurement that hovers: thermals move with the room, the fan and the load, and a reading sitting near the threshold would otherwise report `high`, `normal`, `high`. Debounce is the whole of the flap control here, exactly as it is for `wan.quality` — three consecutive identical readings, or the key holds what it had.

`wifi` ships at `2` for the same reason `devices` does, and from the same underlying fact: an AP count is the console's judgement about heartbeats, and one missed beat on a busy console must not fire an automation.

`poe` ships at `3`, the same argument as `ups.load`: it is an instantaneous measurement, and an AP's radios or a camera's heater coming up move the draw by tens of watts within one poll. Its 90% default assumes those three samples — raise the threshold towards 100 and there is no headroom left to react in during them.

`device.*` is the one entry here that is a pattern rather than a key. Per-device key names come from your hardware, so no list written here could name them; a trailing `*` matches every key with that prefix. An exact key always wins over a pattern, and the longest matching prefix wins between patterns, so `device.ap-attic: 5` pulls one device out of the group it belongs to.

Debouncing happens in the shared state store, so every automation sees the same settled value. Two automations can never disagree about the current state and fight over a workload they share.

This is the setting to revisit the moment you write a [`kubernetes.restart`](/actions/kubernetes/#restart-is-why-debounce-matters): scaling is idempotent and a flap costs nothing, while a restart under a flapping key is a rollout per poll.

## How long Reactor may act on state that has already changed

Debounce is half of an answer to a question worth asking outright, because a policy engine acting on something that stopped being true is the failure everything else here is arranged to avoid. **There are two windows, and only one of them is bounded by anything.**

**A value that changed** reaches every Automation within **`pollInterval` × the key's debounce samples**. Worst case is the change landing just after a poll, then needing its samples on the poll cadence: `wan` at the defaults is 30 seconds, `internet` is 90. Both terms are yours, and that product *is* the bound — there is deliberately nothing else in the path. The reconciler re-evaluates every 15 seconds regardless and is woken immediately on a transition, so it contributes latency, not staleness.

The webhook fast path narrows the **first** term only. A delivery brings the next observation forward, and while a value is still proving itself against its debounce threshold a delivery is [ignored outright](/operations/webhook-fast-path/). That is not an oversight: a delivery only ever says *look now*, and if it could also supply the samples that promote a value, anyone who can reach the endpoint could fast-forward a key straight through the settling time you asked for.

**A console that stops answering** has no such window. A failed observation is logged and dropped — the next poll is the recovery mechanism — so the store keeps reporting the last state it has, and every reconcile re-decides against it for as long as the console is away. That is **correct and stays correct**: withdrawing state Reactor can no longer confirm would release claims during exactly the incident that took the console offline, which is the same reason a key that vanishes gets [`StateKeyUnavailable`](/troubleshooting/state-keys/#2-statekeyunavailable-and-held-state) and held state rather than an `onExit`.

What was missing is that the second case said nothing. `StateKeyUnavailable` announces itself on the Automation; a console that has gone quiet announced itself only in `time() - reactor_last_observation_timestamp_seconds`, which needs metrics enabled and somebody watching. So:

```yaml
unifi:
  pollInterval: 30s
  maxObservationAge: 5m    # empty by default: unbounded, and silent
```

Past that age, every Automation driven by the provider reports it, and **goes on acting**:

```text
Ready  False  ObservationStale
provider "unifi" has not been observed since 2026-08-14T09:12:41Z, past the 5m0s this
install allows; still acting on the state it last reported
```

```yaml
status:
  observedAt: "2026-08-14T09:12:41Z"   # always reported; this is what every field below is only as current as
```

plus a Warning `Event` and `reactor_stale_decisions_total`, which is the attributable half of the observation gauge: the gauge says Reactor went blind, this says automations were still deciding while it was.

It is off by default and it changes no behaviour when on — no claim is released, no `onExit` runs, no target moves. Set it against `pollInterval` and the samples above rather than in isolation: a changed value already takes up to 90 seconds to be believed at the defaults, so anything under about four poll intervals reports a slow console rather than a blind operator.
