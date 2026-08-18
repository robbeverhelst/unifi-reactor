---
title: "WAN and internet state keys"
description: "wan says which uplink is selected, internet says whether the outside world is reachable at all, and wan.quality says whether the link has been any good. Three different questions."
---

## `wan` covers every uplink the gateway reports

`wan` is derived from every `wanN` block in the gateway's device record — `wan1`, `wan2`, `wan3`, however many the console reports. The numbering carries exactly one meaning: `wan1` is the **primary**, and every uplink above it is a **backup**. The vocabulary stays two values no matter how many uplinks exist, so a gateway with a wired secondary *and* a cellular tertiary reads `backup` for either of them — `wan` says the primary is not carrying the traffic, not which link is or what it costs. Whether a cellular backup should be distinguishable from a wired one is tracked as [#105](https://github.com/robbeverhelst/unifi-reactor/issues/105).

A cellular backup is not a physical WAN port. The gateway reports it as a tunnel interface (`gre1` on the hardware this was verified against) whose `wanN` entry carries **no `is_uplink` field at all** — absent, not `false`. So on a failover to cellular the `is_uplink` signal names nobody, and the second signal is what resolves the key: the gateway names its own uplink interface, that name matches the cellular entry's `ifname`, and `wan` reads `backup`. The log line `is_uplink does not name a single live WAN port` accompanies this, and on cellular hardware it is the expected path rather than a fallback misfiring — see [the troubleshooting entry](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover).

```yaml
  when:
    provider: unifi
    state:
      wan: backup      # the primary uplink is not the one carrying traffic
```

## `internet` is the one `wan` cannot express

`wan` says which uplink is *selected*. It stays `primary` when the link is up, the uplink is unchanged, and there is no internet — the failure your gateway's own failover may never act on, because from the gateway's point of view nothing is wrong. `internet` is the key for that case, and it comes from a different place: the console's own `www` health subsystem, which is its judgement about reachability rather than about link state.

```yaml
  when:
    provider: unifi
    state:
      internet: down      # regardless of which uplink is carrying it
```

`internet` is [debounced at 3 samples](/concepts/settling-a-noisy-signal/), so at the default 30s `pollInterval` an outage takes about **90 seconds** to be believed — and a recovery the same. That is a deliberate trade for not shedding load on one bad probe round; if you need it faster, lower `pollInterval` rather than the debounce, because the three samples are what make the signal trustworthy. `wan` is different: it is a switch position rather than a probe, so it ships at the default of 1 sample and reacts on the first observation.

`wan.quality` answers a third question, over a different time horizon: not *is the internet there* but *has this uplink been any good*. It buckets the availability and average latency the console measures against its uptime monitors into two levels, using [thresholds you configure](/operations/configuration/). Those numbers are averages over the console's uptime window — 24 hours on the hardware they were captured from — so `wan.quality` describes a link that has been bad rather than one that spiked, and a long outage keeps it `degraded` for the rest of that window.

That is deliberate. A number cannot be a state value at all: `spec.when` matches strings, and a key whose values are continuous can never be exported as a metric label without one series per distinct reading. Bucketing is what makes it a state key, and the two levels are the whole vocabulary.

```yaml
  when:
    provider: unifi
    state:
      wan.quality: degraded   # don't start the big sync on a link that has been flaky
```

Keep them apart when you write automations. `internet: down` is an outage; `wan.quality: degraded` is a bad day; matching both in one `state` block means *both must hold*.

Together they also give the [unverified `wan` mapping](/concepts/when-reactor-cannot-see/#compatibility) something it has never had — a third opinion from a different endpoint. `stat/health` accumulates uptime per uplink, and uptime is traffic the console watched pass, where `is_uplink` and `uplink.name` are both statements about configuration. If uptime is accumulating on a port other than the one `wan` names, Reactor says so rather than quietly trusting either ([what to do about it](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover)).

## Tuning the quality thresholds

| Value | Default | Description |
| --- | --- | --- |
| `unifi.wan.quality.minAvailabilityPercent` | `99` | availability below this reports `degraded` |
| `unifi.wan.quality.maxLatencyMs` | `150` | average latency above this reports `degraded` |

Both numbers are averages the console keeps over its own uptime window — 24 hours
on the hardware they were captured from — so `wan.quality` describes a link that
*has been* bad rather than one that spiked, and a long outage keeps it `degraded`
for the rest of that window. Only one link's numbers have ever been observed
(100% available, 16 ms), so treat the defaults as starting points and tune them
against your own uplink.
