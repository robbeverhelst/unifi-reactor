---
title: "Power and UPS state keys"
description: "ups, ups.battery, ups.runtime and ups.load are four independent axes on purpose — and remaining runtime, not charge, is what a shutdown Automation should match on."
---

## `ups` and `ups.battery` are separate keys

`ups` and `ups.battery` are separate on purpose. An automation matching `ups: on-battery` stays matched for the whole outage as the battery drains — with a single escalating enum, dropping from `on-battery` to `low-battery` would leave the matching state and fire `onExit`, scaling workloads back **up** in the middle of a power failure. Express escalation by matching both keys instead; all keys in a `state` block must match.

```yaml
  when:
    provider: unifi
    state:
      ups: on-battery
      ups.battery: critical
```

## Charge is a poor shutdown trigger; runtime is a better one

`ups.battery` ignores load, and load is most of the answer: 30% at 300W and 30% at 900W are very different situations. `ups.runtime` is the UPS's own estimate of how long it can carry what is plugged into it *right now*, bucketed against [thresholds you configure](/operations/configuration/), and it is what a shutdown automation should actually match on.

```yaml
  when:
    provider: unifi
    state:
      ups: on-battery
      ups.runtime: critical   # not "the battery is low" — "we are about to run out"
```

`ups.load` is the other half of the same picture: the draw as a fraction of the UPS's power budget. It is what tells you *why* the runtime is short, and it is worth matching before an outage rather than during one — a UPS already running at 85% has no headroom to give you when the power goes.

It is published on mains as well as on battery, deliberately. "Could we even survive an outage right now" is a question worth being able to ask while the lights are still on.

Both are separate keys for the same reason `ups.battery` is: they are independent axes, and an automation matching one must not stop matching because another moved. All four UPS keys are only published when a UniFi UPS is adopted, and `ups.runtime` and `ups.load` are additionally omitted when the UPS reports no runtime estimate or no usable power figures — a missing measurement is never turned into a value.

> ⚠️ `timeToRemain`'s unit is **inferred** to be seconds, from a single observation on a UPS that was not discharging. Nothing in Reactor depended on it before this key existed. Confirm it against a real outage before letting `ups.runtime: critical` shut anything down — [#7](https://github.com/robbeverhelst/unifi-reactor/issues/7) has the procedure.
