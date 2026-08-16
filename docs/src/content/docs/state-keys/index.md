---
title: "State keys"
description: "The whole vocabulary the UniFi provider publishes — wan, internet, ups, devices, firmware, temperature, wifi, poe, outlets — with the values each key can hold."
---

## State keys

Each key is published only when the matching hardware is adopted by your controller.

| Key | Values | Meaning |
| --- | --- | --- |
| `wan` | `primary`, `backup` | which uplink the gateway is currently using |
| `wan.quality` | `good`, `degraded` | how well that uplink has been performing, against the configured thresholds |
| `isp` | a slug, e.g. `example-telecom`, or `unknown` | the carrier behind the live uplink |
| `internet` | `ok`, `degraded`, `down` | whether the outside world is reachable at all |
| `ups` | `online`, `on-battery` | whether a UniFi UPS is on mains or running on battery |
| `ups.battery` | `normal`, `low`, `critical` | remaining charge against the configured thresholds |
| `ups.runtime` | `ample`, `short`, `critical` | how long the UPS says it can carry its current load |
| `ups.load` | `normal`, `high` | draw as a fraction of the UPS's power budget |
| `devices` | `all-online`, `degraded` | whether every adopted device is reachable, or at least one is not |
| `device.<name>` | `online`, `offline` | one adopted device, by slugified name. **Opt-in** — see below |
| `firmware` | `current`, `updates-available` | whether the console has an update waiting for anything adopted |
| `temperature` | `normal`, `high` | the hottest adopted device against the configured threshold |
| `wifi` | `ok`, `warning`, `error` | the WiFi subsystem as a whole, from the console's AP counts |
| `poe` | `ok`, `insufficient` | PoE headroom on the worst switch, against the configured threshold |
| `outlet.<n>` | `on`, `off` | one switchable UPS outlet, by index or by name. **Read-only** — see below |

`isp` is the one key whose values are not a closed set: it is the carrier name your console geolocated your public address to, lowercased with everything non-alphanumeric turned into a hyphen. Look it up before matching on it —

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'key=isp'
# INFO state transition provider=unifi key=isp from= to=example-telecom
```

— and use it when *who* is carrying your traffic is what matters rather than which port it leaves by, which is usually the case for anything metered:

```yaml
  when:
    provider: unifi
    state:
      isp: unknown        # or your backup carrier's slug
```

It exists for a second reason. `wan` and `isp` are independent answers to "did the uplink change", so Reactor compares them: if one moves and the other does not, it says so rather than quietly trusting either. Those lines are worth reading — see [`wan` and `isp` disagree](/troubleshooting/state-keys/#10-wan-and-isp-disagree-about-a-failover).
