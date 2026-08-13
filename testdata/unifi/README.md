# UniFi ground-truth captures

Real responses captured from a UDM Pro (UniFi Network **10.5.67**, UniFi OS, gateway firmware 5.1.26.33914) on 2026-08-11, per the v0.0 spike in the project spec. **Do not invent payload formats** — parsers are built and tested against these files.

Sanitization: public IPs → TEST-NET-3 (`203.0.113.x`), MAC addresses → `aa:bb:cc:00:11:22`, geo coordinates/city and device serials redacted. Field structure is untouched.

## `api/` — Network API responses (auth: `X-API-KEY` header, works on both API families)

| File | Endpoint | Notes |
| --- | --- | --- |
| `integration-info.json` | `GET /proxy/network/integration/v1/info` | official Integration API; application version |
| `integration-sites.json` | `GET /proxy/network/integration/v1/sites` | official Integration API; site list (`internalReference: "default"`) |
| `stat-health.json` | `GET /proxy/network/api/s/default/stat/health` | legacy API; `wan` subsystem: `status`, `wan_ip`, `isp_name`, per-WAN `uptime_stats` (`WAN`, `WAN2`) with monitor availability |
| `stat-device-gateway.json` | `GET /proxy/network/api/s/default/stat/device` (UDMPRO record only) | `wan1`/`wan2` port state (`is_uplink`, `up`, `ip`), `uplink.name`, `last_wan_status`, `active_geo_info` (ISP of active WAN) |
| `stat-device-ups.json` | `GET /proxy/network/api/s/default/stat/device` (UniFi UPS 2U record only) | `vbms_table` battery state — see below. Captured on mains power at 100% charge |

## UPS state (`vbms_table`)

A UniFi UPS (UPS 2U, model `USWDA26`) is reported by `stat/device` as a
switch-type device carrying a `vbms_table` block:

```json
{
  "is_battery_mode": false,
  "battpool": {
    "batteryLevel": 100,
    "ischarging": true,
    "timeToRemain": 1071,
    "device_total_power_budget": 1000,
    "device_total_power_output": 300
  }
}
```

`is_battery_mode` is the authoritative mains-vs-battery signal; `batteryLevel`
is the remaining charge percentage. `timeToRemain` appears to be seconds of
runtime at the current load (~18 minutes at 300 W of a 1000 W budget), but this
is inferred and not yet confirmed against a real outage — the provider does not
depend on it.

The UPS record also carries `nut_client_table` / `nut_client_ips`, so the UPS
can additionally serve Network UPS Tools clients. Reactor does not use this: the
state is already in the device poll it performs anyway.

## WAN state candidates (to confirm during the failover capture)

Captured while **only WAN1 (Telenet, eth8) is connected**; WAN2 (eth9, SFP+) is enabled but down — the backup uplink was not yet installed. Candidate signals for `wan: primary | backup`:

- `stat/device` gateway `.wan1.is_uplink` / `.wan2.is_uplink` — expected to flip on failover
- `stat/device` gateway `.uplink.name` (`eth8` = WAN1)
- `stat/device` gateway `.active_geo_info.WAN.isp_name` (Telenet vs backup ISP)
- `stat/health` `wan` subsystem `.wan_ip` / `.isp_name`
- `stat/health` `.uptime_stats.WAN.availability` vs `.uptime_stats.WAN2.availability`

## `webhooks/` — captured Alarm Manager webhook deliveries (pending)

To be captured during the real WAN failover/recovery test. The Integration API has no endpoint for configuring outbound Alarm Manager webhooks — they are configured in the UniFi UI (Alarm Manager → New Alarm → action: Webhook).
