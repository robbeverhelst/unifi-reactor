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
| `api/stat-device-gateway.json` | `GET /proxy/network/api/s/<site>/stat/device` (gateway) | `wan1`/`wan2` (`is_uplink`, `up`, `ifname`, `speed`), `uplink`, `last_wan_status`, `isp` |
| `api/stat-device-ups.json` | same call, UPS record | `vbms_table` battery state, `outlet_table` |
| `api/stat-health.json` | `GET /proxy/network/api/s/<site>/stat/health` | per-subsystem `status`, WAN `uptime_stats` monitors, ISP |
| `api/integration-info.json` | `GET /proxy/network/integration/v1/info` | controller version, for the compatibility guard |
| `api/integration-sites.json` | `GET /proxy/network/integration/v1/sites` | site listing |

Both API families accept the same `X-API-KEY` header as of Network 10.5.

## WAN state

Captured with WAN1 (ethernet) active and WAN2 (SFP+) enabled but down. The provider derives `wan: primary | backup` from which port reports `is_uplink`.

> ⚠️ **This mapping is inferred, not observed.** No real failover has been captured yet, so which fields actually move during one is unconfirmed. See issue #34. `isp` and `last_wan_status` are captured partly as independent cross-checks.

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

`webhooks/` will hold captured Alarm Manager deliveries. `hack/webhook-logger.mjs` dumps incoming requests verbatim to `webhooks/raw/` (gitignored) — review, allowlist, and sanitize before committing anything from there.
