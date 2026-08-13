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
| `api/stat-device-gateway.json` | `GET /proxy/network/api/s/<site>/stat/device` (gateway) | `wan1`/`wan2` (`is_uplink`, `up`, `ifname`, `speed`), `uplink`, `last_wan_status`, `isp` (allowlisted from `active_geo_info.WAN.isp_name`) |
| `api/stat-device-ups.json` | same call, UPS record | `vbms_table` battery state, `outlet_table` |
| `api/stat-health.json` | `GET /proxy/network/api/s/<site>/stat/health` | per-subsystem `status`, WAN `uptime_stats` monitors, ISP |
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
    uptime: (.uptime_stats | to_entries | map({(.key): {up: .value.uptime, down: .value.downtime}}))
  }' "$f"
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
| What did Reactor log? | the `unifi-wan` lines | a disagreement warning at stage 3 means one of the signals is wrong; silence means they all moved together |

Only then commit anything, and only through the capture script:

```sh
UNIFI_URL=$UNIFI_URL UNIFI_API_KEY=$UNIFI_API_KEY ./hack/capture-unifi.sh
```

That script writes `testdata/unifi/api/` through the allowlist. **Do not hand-edit `/tmp/failover/*.json` into a fixture** — those files are whole device records containing `x_authkey`, syslog keys and adoption identifiers. That exact shortcut put a live credential in this repository's history once. Capturing the backup-live stage means running the script *while the failover is in progress* and saving its output under a new name, e.g. `stat-device-gateway-on-backup.json`, then adding a row to the file table above.

Post the comparison output on [#34](https://github.com/robbeverhelst/unifi-reactor/issues/34) even if you cannot commit fixtures. The five numbers per stage are the finding; the fixture is only how it gets tested.

## UPS state (`vbms_table`)

A UniFi UPS is reported as a switch-type device (`USWDA26`) carrying:

```json
{
  "is_battery_mode": false,
  "battpool": { "batteryLevel": 97, "ischarging": true, "timeToRemain": 1041 }
}
```

`is_battery_mode` is the authoritative mains-vs-battery signal and `batteryLevel` the remaining charge. `timeToRemain` appears to be seconds of runtime at the current load, but that is inferred from observation and nothing depends on it yet.

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
