# UniFi ground-truth captures

Real responses from a UDM Pro (UniFi Network **10.5.67**, gateway firmware 5.1.26, with a UniFi UPS 2U). Parsers are written and tested against these files — never against assumed formats.

## Capturing

```sh
UNIFI_URL=https://192.168.1.1 UNIFI_API_KEY=<key> ./hack/capture-unifi.sh
```

That script is the policy. **It keeps an explicit allowlist of fields and discards everything else**, then replaces the few remaining sensitive values with placeholders (public IPs become TEST-NET-3, MACs and site IDs become fixed dummies). Adding a field to a parser means adding it to the allowlist, deliberately, one at a time.

The allowlist matters more than it looks. `stat/device` returns entire device records — roughly 8 KB each, containing device management keys (`x_authkey`), syslog keys, adoption identifiers, and full topology tables. The parser needs about a dozen fields. An earlier version of these fixtures was produced by *removing* the sensitive fields someone thought of rather than *keeping* only the needed ones, and a live credential reached this repository's history as a result. Allowlist, not denylist.

`hack/verify-testdata.sh` runs as part of `make test` and rejects unredacted secret fields, routable IPs outside documentation ranges, and MACs outside the placeholder prefix. It is the safety net, not the mechanism.

## Files

| File | Endpoint | What it documents |
| --- | --- | --- |
| `api/stat-device-gateway.json` | `GET /proxy/network/api/s/<site>/stat/device` (gateway) | `wan1`/`wan2` (`is_uplink`, `up`, `ifname`, `speed`), `uplink`, `last_wan_status`, `isp` (allowlisted from `active_geo_info.WAN.isp_name`), `state`/`adopted`/`name` (the `devices` key) |
| `api/stat-device-ups.json` | same call, UPS record | `vbms_table` battery state, `outlet_table`, `state`/`adopted`/`name` |
| `api/stat-health.json` | `GET /proxy/network/api/s/<site>/stat/health` | per-subsystem `status` (the `internet` key), WAN `uptime_stats` monitors (the `wan.quality` key), ISP |
| `api/integration-info.json` | `GET /proxy/network/integration/v1/info` | controller version, for the compatibility guard |
| `api/integration-sites.json` | `GET /proxy/network/integration/v1/sites` | site listing |

Both API families accept the same `X-API-KEY` header as of Network 10.5.

## WAN state

Captured with WAN1 (ethernet) active and WAN2 (SFP+) enabled but down. Four fields in that one capture say something about which uplink is live, and they all agree — which is exactly why the capture cannot settle anything:

| Field | In the capture | What the provider does with it |
| --- | --- | --- |
| `wan1.is_uplink` / `wan2.is_uplink` | `true` / `false` | derives `wan: primary \| backup`. **The signal of record**, and the unverified one |
| `uplink.name` | `eth8`, matching `wan1.ifname` | independent second opinion, matched against each port's `ifname`. Used when `is_uplink` names no single live port; otherwise only to report disagreement |
| `isp` | `Telenet` | published as the `isp` state key, slugified. Compared with `wan` across observations: if one moves and the other does not, that is logged |
| `last_wan_status` | `{"WAN": "online"}` | never derived from — only `online` has ever been seen, so the failed value is unknown. Used only to notice the live uplink not calling itself online |

> ⚠️ **The mapping is inferred, not observed.** No real failover has been captured, so which of these fields actually moves during one is unconfirmed — including whether `is_uplink` means "is carrying traffic" or "is configured as the uplink". See issue #34, and the runbook below for how to settle it.

### Rehearsing a failover without hardware

`hack/mock-unifi` serves the capture and rewrites it according to each hypothesis about what a failover looks like. `GET /wan` lists them; each names what it says moves and what it would mean:

```sh
make dev-mock

curl -X POST 'http://localhost:9443/wan?link=backup&variant=clean'             # everything moves together
curl -X POST 'http://localhost:9443/wan?link=backup&variant=is-uplink-only'    # only is_uplink moves
curl -X POST 'http://localhost:9443/wan?link=backup&variant=is-uplink-pinned'  # is_uplink never moves
curl -X POST 'http://localhost:9443/wan?link=backup&variant=both-uplinks'      # both ports claim it
curl -X POST 'http://localhost:9443/wan?link=backup&variant=no-uplink'         # neither does, mid-switchover
curl -X POST 'http://localhost:9443/wan?link=primary'                          # recovery
```

The same hypotheses are asserted in `internal/providers/unifi/wan_test.go`, derived there from the committed capture in code, with each transformation written out. **None of them is a fixture.** A file in `testdata/` claims to have come off a console; a hypothesis has not, and the two must never be confusable. The runbook below is the only thing that produces a real one.

## Internet reachability and link quality (`stat/health`)

Two keys come from this capture, and they answer different questions over
different time horizons.

`internet` is the `www` subsystem's `status`, mapped `ok` → `ok`,
`warning` → `degraded`, `error` → `down`. Anything else — including `unknown`,
which the capture shows the `vpn` subsystem using for "nothing configured
here" — publishes no key at all, so an unfamiliar status holds whatever an
Automation last matched instead of being read as "fine".

> ⚠️ **The failure values are inferred.** `ok`, `warning` and `unknown` all
> appear in this one capture — on `wan`, `wlan` and `vpn` respectively — so the
> status *vocabulary* is observed. But the `www` subsystem has only ever been
> seen saying `ok`, so which value it takes when the internet is actually
> unreachable is unconfirmed, and `error` has never been captured on any
> subsystem. See the runbook below.

`wan.quality` comes from the `wan` subsystem's `uptime_stats`, keyed by uplink
exactly as `last_wan_status` is. Two things in that block are worth knowing
before reading the parser:

| Field | In the capture | What the provider does with it |
| --- | --- | --- |
| `availability`, `latency_average` | `100.0` and `16` on `WAN`; **absent** on `WAN2` and `WAN3` | bucketed into `good`/`degraded` against configurable thresholds |
| `monitors[]`, `alerting_monitors[]` | present on all three uplinks, each with its own `availability` | averaged as a fallback when the uplink-level fields are missing |
| `time_period` | `86400` on `WAN` only | never derived from — logged, because it is what makes a threshold interpretable |
| `uptime` | `98787` on `WAN` only | a third, independent opinion on which uplink is live (see below) |

The absence pattern is the load-bearing observation. The console **omits**
these fields rather than reporting zero — `WAN2` has been down for the whole
window and carries `downtime` but no `availability` at all — so every number in
the parser is decoded into a pointer. Reading an absent field as `0` would turn
a truncated response into "this link is 0% available", which is the difference
between holding state and shedding a cluster's load.

Whether that omission is the console suppressing zero values (a Go
`omitempty`, which the monitors' own `availability: 0.0` argues against) or
something about how a dead uplink is summarized is not settled by one capture,
and the parser does not need it to be: a missing field is treated as missing
either way.

### WiFi health (`wlan` subsystem)

The third key from this capture, and the only one in the `device.<name>`/
`firmware`/`temperature`/`wifi`/`poe` batch that needed no new field at all:

| Field | In the capture | What the provider does with it |
| --- | --- | --- |
| `num_adopted` | `3` | the denominator: how many APs this site owns |
| `num_disconnected` | `1` | some → `warning`, all → `error`, none → `ok` |
| `num_ap` | `2` | never derived from — logged, because `2 + 1 = 3` is the arithmetic that says `num_adopted` is the right denominator |
| `status` | `warning` | never derived from either. Cross-checked against the counts, and a mismatch is counted as `wifi-status-disagrees` |

`wifi` comes from the counts rather than from `status`, and issue
[#9](https://github.com/robbeverhelst/unifi-reactor/issues/9) asked for that to be
documented rather than left mysterious. The counts can be explained — "1 of 3
access points is disconnected" — and they make `error` *derivable*, where mapping
the vendor string through would have been inference: no capture has ever shown
any subsystem saying `error`, on any subsystem. Here the two agree exactly, which
is what makes the counts a sharpening of the console's verdict rather than a
disagreement with it.

Both counts decode into pointers. Zero adopted APs is a site with no WiFi and
publishes no key; a subsystem that reports no counts is not a site with no APs.

### A third opinion on the `wan` mapping

`uptime` is the only field the capture shows exclusively on the live uplink,
and it is qualitatively different from the other WAN signals: it is traffic the
console watched pass, where `is_uplink` and `uplink.name` are both statements
about *configuration*. So the provider counts a
`reactor_provider_signal_disagreements_total{signal="wan-health-disagrees"}`
when uptime is accumulating on an uplink other than the one `wan` names.

It is deliberately narrow — nothing fires unless *some* uplink has uptime and
the believed one does not, which is the shape of "the mapping is pointing at
the wrong port" rather than of "this link is having a bad day". It also does
not yet *resolve* anything: `wan` is still derived from `is_uplink`, because
promoting a new signal of record needs a real failover to have been observed
and one has not been. Add the health comparison to the deliverable below and
it might.

There is deliberately **no** disagreement signal for `internet: down` while
`wan: primary`. That is not a contradiction — it is precisely the failure mode
`internet` was added to observe, and counting it would fire the metric on
exactly the case the key exists for.

## Capturing a real failover

This is the open half of issue #34. It needs a gateway with two working uplinks — for this project, a U5G with a SIM in it — and about fifteen minutes. Nothing here is dangerous; the worst case is a brief outage on the primary uplink.

**Before you start**, know that the point is not to get a `backup` reading. It is to find out *which fields moved*, and specifically whether `is_uplink` follows the traffic or stays where it was configured. Capture all four stages even if the state looks right at stage 3 — the interesting failures are silent.

Set up once, in a shell with the credentials in it:

```sh
export UNIFI_URL=https://<your console>
export UNIFI_API_KEY=<key from Settings → Control Plane → Integrations>
export UNIFI_SITE=default

# Raw, timestamped, gitignored. NOT fixtures — these hold whole device records.
mkdir -p /tmp/failover && cd /path/to/unifi-reactor

snap() {   # snap <stage>
  for endpoint in stat/device stat/health; do
    curl -sk --fail -H "X-API-KEY: $UNIFI_API_KEY" \
      "$UNIFI_URL/proxy/network/api/s/$UNIFI_SITE/$endpoint" \
      > "/tmp/failover/$1-$(basename $endpoint).json"
  done
  echo "captured $1"
}
```

Run Reactor against the console while you do this, at `log.level=debug`, and keep its logs. What it *said* at each stage is half the evidence:

```sh
kubectl -n reactor-system logs -f deploy/reactor | grep -E 'state transition|unifi-wan'
```

| Stage | What to do | What to capture | Wait for |
| --- | --- | --- | --- |
| 1. Baseline | nothing — both uplinks up, primary live | `snap 1-baseline` | — |
| 2. Mid-switchover | unplug the primary WAN cable (or disable the port in the UniFi UI) | `snap 2-switching` **immediately**, then again ~10s later as `snap 2b-switching` | don't wait; this stage is the one that disappears |
| 3. On backup | — | `snap 3-on-backup` once traffic is flowing over the backup | the UniFi UI showing the backup as active, and a working ping from behind the gateway |
| 4. Recovering | plug the primary back in | `snap 4-recovering` immediately | — |
| 5. Recovered | — | `snap 5-recovered` once the primary is live again | the UI showing the primary active |

Stage 2 is the one worth being quick about: the switchover window is where `is_uplink` may name neither port, and it is over in seconds. Two snapshots are better than one.

Then compare. This is the actual deliverable — five numbers per stage:

```sh
cd /tmp/failover
for f in *-device.json; do
  echo "== $f"
  jq -c '.data[] | select(.wan1 != null) | {
    wan1_is_uplink: .wan1.is_uplink, wan1_up: .wan1.up,
    wan2_is_uplink: .wan2.is_uplink, wan2_up: .wan2.up,
    uplink: .uplink.name,
    last_wan_status,
    isp: .active_geo_info.WAN.isp_name
  }' "$f"
done

for f in *-health.json; do
  echo "== $f"
  jq -c '.data[] | select(.subsystem == "wan") | {
    status, wan_ip, isp_name, isp_organization,
    uptime: (.uptime_stats | to_entries | map({(.key): {
      up: .value.uptime, down: .value.downtime,
      av: .value.availability, lat: .value.latency_average
    }}))
  }' "$f"
  # The www subsystem is the whole of the internet key. Its status during a
  # real outage is the single most valuable unknown in this runbook.
  jq -c '[.data[] | {subsystem, status}]' "$f"
done
```

What each answer settles:

| Question | How to read it | What it changes |
| --- | --- | --- |
| Does `is_uplink` move from wan1 to wan2 at stage 3? | compare stage 1 and stage 3 | **If no, the current mapping is wrong** and `uplink.name` or `isp` has to become the signal of record |
| Do both ports report `is_uplink` at any stage? | stages 2 and 4 especially | tells us `is_uplink` means "configured", and the tie-break has to be `uplink.name` |
| Does *neither* report it at stage 2? | stage 2 / 2b | confirms the switchover window is real, and how long the `wan` key would be missing for |
| Does `uplink.name` follow the live port? | every stage | promotes or demotes the second signal |
| What does `last_wan_status` say for a downed uplink? | stage 2 onward | **the unknown value.** Only `online` has ever been seen; whatever appears here is what the provider can start trusting |
| Does `isp_name` change, and how many polls late? | stages 3 and 5 | sets the `isp` debounce, currently 2 samples on the assumption of one blank poll |
| Does `uptime` move to the backup's key, and how fast? | every stage | **promotes or demotes the third signal.** Uptime is passed traffic rather than configuration, so if it tracks the live uplink it is the best candidate to become the signal of record |
| What does the `www` subsystem say while the primary is down? | stage 2 especially | **the biggest unknown in `internet`.** Only `ok` has ever been seen. Whatever appears is what `down` and `degraded` should actually map to |
| Does `availability` fall, and over what timescale? | stages 2–5 | says whether the `wan.quality` threshold is reactive or a next-day verdict, and whether the default of 99% is anywhere near right |
| What did Reactor log? | the `unifi-wan` lines | a disagreement warning at stage 3 means one of the signals is wrong; silence means they all moved together |

Only then commit anything, and only through the capture script:

```sh
UNIFI_URL=$UNIFI_URL UNIFI_API_KEY=$UNIFI_API_KEY ./hack/capture-unifi.sh
```

That script writes `testdata/unifi/api/` through the allowlist. **Do not hand-edit `/tmp/failover/*.json` into a fixture** — those files are whole device records containing `x_authkey`, syslog keys and adoption identifiers. That exact shortcut put a live credential in this repository's history once. Capturing the backup-live stage means running the script *while the failover is in progress* and saving its output under a new name, e.g. `stat-device-gateway-on-backup.json`, then adding a row to the file table above.

Post the comparison output on [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34) even if you cannot commit fixtures. The five numbers per stage are the finding; the fixture is only how it gets tested.

## Fleet health (`state`, `adopted`)

Both committed device records carry `"state": 1` and `"adopted": true`, so the
`devices` key is derived entirely from fields that are already here — the only
key in this batch that needed nothing new.

| Field | In the captures | What the provider does with it |
| --- | --- | --- |
| `state` | `1` on both records | `1` → `online`, `0` → `offline`, **anything else → no value at all** |
| `adopted` | `true` on both records | gates the fleet keys: an unadopted device is not your fleet |
| `name` | `Dream Machine Pro`, `UPS 2U` | slugified into the `device.<name>` key |
| `disconnection_reason` | **absent** — both devices were connected | never derived from; a `V(1)` diagnostic naming why the console lost a device |

`state` is decoded as a **pointer**, for the reason the whole of this file keeps
repeating: `0` is a real value here and means offline, so an absent `state` read
as `0` would report one truncated record as a dead device and take the fleet to
`degraded`. `adopted` is a pointer too, and `nil` is read as "not known to be
adopted" — the direction that publishes fewer keys rather than more.

> ⚠️ **Only states 0 and 1 have been observed**, both of them `1`. UniFi
> documents others — pending adoption, provisioning, upgrading, heartbeat missed,
> isolated — and none has been captured, so none is mapped. A device in one of
> them counts towards neither key and logs a line asking for a report, because
> reading "upgrading" as `offline` would turn a firmware update into a fleet
> outage. `disconnection_reason`'s field name comes from UniFi's own API rather
> than from a capture; it is diagnostics only, so a wrong name costs a log field
> and nothing else.

The two captured names are the console's defaults for that hardware, which is
what makes them safe to use as documentation examples. Anything derived from a
device name in a fixture or a doc example must be a placeholder of that kind.

## Firmware (`upgradable`) — parsed, not captured

The `firmware` key is derived from `upgradable`, and **no capture contains that
field**. `version` and `displayable_version` are here; nothing about upgrades is.

| Field | In the captures | What the provider does with it |
| --- | --- | --- |
| `version` | `5.1.26.33914`, `1.6.1.413` | diagnostics: the "from" in the log line |
| `upgradable` | **absent** | the only field the key is derived from; a pointer, so absent publishes no key |
| `upgrade_to_firmware` | **absent** | diagnostics: the "to" |
| `required_version`, `safe_for_autoupgrade` | **absent** | diagnostics |
| `model_in_eol`, `model_in_lts` | **absent** | counted in the log line; deliberately not keys — an inventory fact does not transition |

All six are now in the allowlist in `hack/capture-unifi.sh`, so the next real
capture settles them. Until then the parser is written to the shape UniFi's own
API documents and to the field names issue [#12](https://github.com/robbeverhelst/unifi-reactor/issues/12)
lists, and `internal/providers/unifi/firmware_test.go` asserts the decode of
that documented shape **in code**. It is a hypothesis, and it lives in a test
rather than in this directory for the reason the failover hypotheses do: a file
here claims to have come off a console.

> ⚠️ **Unverified.** If the field is named something else on your firmware, the
> failure mode is that `firmware` never appears — not that it wrongly reports
> `current`. That is the direction a pointer buys you, and it is why absent is
> never read as "up to date".

## Temperature — parsed, not captured

**No committed capture carries a single thermal field.** The UPS 2U reports
`has_fan: false` and no temperatures, and the gateway record was allowlisted
before any of this was parsed, so the `temperature` key is written entirely to
the shape UniFi's API documents and the field names issue
[#11](https://github.com/robbeverhelst/unifi-reactor/issues/11) lists.

| Field | In the captures | What the provider does with it |
| --- | --- | --- |
| `has_temperature` | **absent** | the device says it does thermal reporting; keeps the key alive when it publishes no number |
| `overheating` | **absent** | the console's own verdict, trusted over the configured threshold |
| `temperatures[]` (`name`, `type`, `value`) | **absent** | the hottest sensor's `value` is the device's reading |
| `general_temperature` | **absent** | the single-value form, used when there is no table |
| `has_fan` | **absent** | never derived from; a hot fanless device and a hot device with a dead fan are different problems |

Every `value` is a pointer. A sensor reporting nothing is not a sensor at 0 °C,
and reading it as one would make the rack look *coldest* exactly when a sensor
stops answering — the wrong direction for the only key whose job is to notice
heat.

All of them are now in the allowlist in `hack/capture-unifi.sh`, and the script
now also writes `stat-device-switch.json` and `stat-device-ap.json` — see
[what is not captured yet](#what-is-not-captured-yet) — because a switch or an
AP is the hardware that would settle this. `internal/providers/unifi/temperature_test.go`
asserts the decode of both documented forms **in code**, as a hypothesis rather
than as a fixture.

> ⚠️ **Unverified, including the unit.** Nothing confirms these readings are
> Celsius. If a firmware reported Fahrenheit, the 75 default would trip on a
> perfectly cool switch — which is the one failure mode here that is loud rather
> than silent, and the reason the debug line prints the raw number. Compare it
> against what the UniFi UI shows for the same device before trusting the key.

## PoE — parsed, not captured

**No switch record exists in this repository at all.** The UPS 2U is reported as
a switch-type device (`USWDA26`) and carries neither a `port_table` nor a PoE
budget, so the `poe` key is written entirely to the shape UniFi's API documents
and the field names issue [#14](https://github.com/robbeverhelst/unifi-reactor/issues/14)
lists.

| Field | In the captures | What the provider does with it |
| --- | --- | --- |
| `total_max_power` | **absent** | the switch's whole PoE budget in watts: the denominator |
| `port_table[].poe_enable` | **absent** | whether the port is delivering power at all |
| `port_table[].poe_power` | **absent** | the watts it is delivering, summed across powered ports |
| `port_table[].poe_class` | **absent** | never derived from; names a port in the diagnostic line |
| `port_table[].port_idx` | **absent** | never derived from; the same |

Two decisions in the parser exist because of what is *not* known:

`poe_power` is decoded by a type that accepts **a JSON number or a numeric
string**. UniFi is documented to report it as `"3.90"` on several firmwares
while other endpoints use a number, and committing to either would make an
entire switch's draw unreadable on the half of the world that reports it the
other way. Null, empty and unparseable all mean "no reading" — never 0 W.

A port that is **powering something and reports no wattage makes the whole
switch unreadable**, rather than contributing zero. This is "absent is not zero"
at the one place on this key where it hides a failure: under-counting the draw
reports headroom that is not there, which is exactly what #14 exists to catch.

The whole `port_table` is projected down to those four fields in the capture
script — a real one carries per-port names, MACs and client counts — and
`internal/providers/unifi/poe_test.go` asserts the decode of the documented
shape **in code**, as a hypothesis rather than as a fixture.

> ⚠️ **Unverified.** A `stat-device-switch.json` from a US-8-60W or a US-48 is
> the single capture that would settle this key, the PoE half of `temperature`,
> and the firmware flags at once — **and the write path's PoE guard with them**,
> which reads the same `port_table` for `is_uplink`, `port_poe` and a port name.
> The allowlist projection carries both readers' fields for exactly that reason:
> keeping only one set would make the first switch capture useless to the other
> half, and look like evidence that its fields do not exist. Port names are
> replaced with their index, because a port is usually named after the room or
> the person on the end of it.

## What is not captured yet

`hack/capture-unifi.sh` writes two files that **do not exist in this
repository**, because no capture has been taken since they were added:

| File it would write | Why it matters |
| --- | --- |
| `api/stat-device-switch.json` | the ground truth for `poe` and for `temperature` on a switch, and the only device type carrying a `port_table` |
| `api/stat-device-ap.json` | `temperature` on a fanless device, and the firmware flags on a second device type |

Both are written with the device's `name` replaced by a placeholder, unlike the
gateway and UPS records: those two kept the console's own default names for that
hardware, while a switch or an AP is usually named after a room or a person. A
device name is API-shaped — it *is* the `device.<name>` state key — so its shape
belongs in a fixture; whose room it is does not.

If the site has no such device adopted, the script says so and writes nothing.
Run it and commit whatever it produces; every ⚠️ above and below is waiting on
one of those two files.

## UPS state (`vbms_table`)

A UniFi UPS is reported as a switch-type device (`USWDA26`) carrying:

```json
{
  "is_battery_mode": false,
  "battpool": { "batteryLevel": 97, "ischarging": true, "timeToRemain": 1041 }
}
```

`is_battery_mode` is the authoritative mains-vs-battery signal and `batteryLevel` the remaining charge.

Two more keys come from the same block. `battpool` is captured whole — it is pure battery telemetry with no identifiers — so every field below was already committed before any of this was parsed.

| Field | In the capture | What the provider does with it |
| --- | --- | --- |
| `timeToRemain` | `1043` | bucketed into `ups.runtime`: `ample` / `short` / `critical` |
| `device_total_power_output` | `310` | `ups.load`, as a share of the budget |
| `device_total_power_budget` | `1000` | the denominator of that share |
| `battery_avr_time` | `-1` | never read — but it is the evidence that `-1` is this block's "unknown" |

> ⚠️ **`timeToRemain`'s unit is inferred.** Seconds is the only reading consistent with 1043 on a UPS 2U drawing 310W of a 1000W budget, but the capture was taken on mains with the battery charging, so the value has never been watched actually count down. Confirm it during a real outage before anything is allowed to shut down on `ups.runtime: critical`. See [#7](https://github.com/robbeverhelst/unifi-reactor/issues/7).

Zero and negative both mean "no estimate" and publish no `ups.runtime` key: `battery_avr_time: -1` in this same block is what says `-1` is used that way, and an absent field decodes to `0`, which is not a runtime anything should act on either. The two power figures decode into pointers for the same reason the health numbers do — an absent output read as `0W` would report a fully loaded UPS as idle.

UniFi also has a `network:ups_overload_detected` alarm trigger, which corroborates `ups.load` but is not read: nothing derives state from a delivery payload.

## Outlets (`outlet_table`) — captured, and read-only on purpose

Every field the `outlet.<n>` keys need is in `api/stat-device-ups.json`, taken off a real console:

```json
{"index": 1, "name": "Outlet 1", "relay_state": true, "relay_group": 1}
```

Eight outlets. Outlets 1–4 report `relay_group: 1`; outlets 5–8 report `relay_group: 2`.

| Field | In the capture | What the provider does with it |
| --- | --- | --- |
| `index` | `1`–`8` | the key's address while the outlet is unnamed: `outlet.3` |
| `name` | `Outlet 1` … `Outlet 8` | the key's address once somebody names it: `outlet.nas`. `Outlet <number>` is treated as the index spelled out, not as a name |
| `relay_state` | `true` on all eight | the value: `on` / `off`. A pointer, because absent is not off |
| `relay_group` | `1` or `2` | **nothing.** Never derived from, only reported — see below |

One more thing this capture settles, and it is easy to miss: the **gateway** record carries
`"outlet_table": []`. Having the field is not having outlets, so a device is the outlet-bearing one
when it lists outlets, never when it merely mentions the field.

> ⚠️ **`relay_group` is the reason there is no write path.** If the relay group is what the hardware
> switches, then asking for outlet 3 to go off takes outlets 1–4 with it. The documented write path —
> `outlet_overrides` via `PUT rest/device` — comes from the **USP-PDU-Pro and USP-Strip**, which
> expose `outlet_caps`, `outlet_power`, `outlet_current` and `cycle_enabled` per outlet and have no
> `relay_group` at all. This UPS exposes none of those and does have relay groups, so the documented
> path is documented for a different device class and confirms nothing here. Switching is
> [#23](https://github.com/robbeverhelst/unifi-reactor/issues/23).

The experiment that settles it needs no capture and no code — it needs the console. With Reactor
running and this key published, toggle **one** outlet by hand in the UniFi UI, in a bank carrying
nothing that matters, and read the line Reactor logs: `movedInGroup=1` of `4` means outlets switch
individually, `4` of `4` means the relay group is the switching unit. That is hypothesis H1 on
[#60](https://github.com/robbeverhelst/unifi-reactor/issues/60). While in the UI, **name the
outlets** — the second half of the same visit, and what turns `outlet.3` into `outlet.nas`.

`hack/mock-unifi`'s `/outlets` rehearses both answers (`switching=individual` and `switching=group`)
so the parser is exercised against each. Unlike the WLAN table and the PoE switch there, this
endpoint rewrites the real capture rather than inventing a shape.

## What the write path has, which is nothing

`unifi.wlan.*` and `unifi.poe.cycle` write to the console, and **no capture backs any of it**. There
is no `rest/wlanconf` response here and no switch record — every device capture above is a gateway
or a UPS — so the fields those actions read are inferred rather than observed.

> Since the state batch, `hack/capture-unifi.sh` writes a `stat-device-switch.json` when a switch is
> adopted, and its `port_table` projection carries the write path's three fields (`is_uplink`,
> `port_poe`, and a port name replaced by its index) alongside the `poe` state key's. One capture
> would therefore give the PoE half of both paths its first ground truth at once. The WLAN half
> still has none, and `wlanconf` is the record that carries pre-shared keys. See
[Writing to a UniFi console](https://reactor.robbeverhelst.com/contributing/unifi-write-api/), which splits the two, and note that
`hack/mock-unifi` builds its WLAN table and its PoE switch **in code**, clearly labelled, precisely
so nothing in `testdata/` claims to have come off a console when it did not.

If a capture is ever wanted for those endpoints it goes through `hack/capture-unifi.sh` with the
allowlist extended one field at a time, like everything else here. A `wlanconf` record carries
pre-shared keys and RADIUS secrets, and a switch record carries the same management keys the
gateway one does — which is exactly the material the allowlist policy exists for.

## Webhooks

`webhooks/` will hold captured Alarm Manager deliveries. **It is still empty**: capturing one requires a real console configured to post to a real receiver, and that has not been done yet.

Nothing depends on it. The receiver never reads a delivery body — a delivery only ever asks for a re-observation, and the observation decides the state — so there is no parser here waiting for ground truth. These fixtures are for the event triggers that come later, where a payload *is* the data.

Capturing one is two steps, and the second is not optional:

```sh
node hack/webhook-logger.mjs 8080                     # dumps verbatim to webhooks/raw/ (gitignored)

./hack/sanitize-webhook-capture.sh --paths webhooks/raw/<file>.json
./hack/sanitize-webhook-capture.sh webhooks/raw/<file>.json <name> alarm.trigger,alarm.title
```

`--paths` prints every field path in the body; the second command keeps the ones named and discards everything else, along with every header except `content-type`.

Dropping the headers is the point. A delivery arrives with Reactor's own shared secret in an `Authorization` header, so a fixture made by keeping *the request* rather than *these fields of the request* publishes the credential that protects the endpoint. `hack/verify-testdata.sh` rejects leftover credential material in this directory, but as with the API captures, that is the safety net and the script is the mechanism.
