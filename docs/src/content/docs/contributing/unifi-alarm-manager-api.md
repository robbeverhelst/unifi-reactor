---
title: "UniFi Alarm Manager API notes"
description: "Reverse-engineered notes on UniFi’s webhook configuration API: cookie plus CSRF authentication, the create-rule body that does not match the GET representation, and how sure we are."
---

Discovered against UniFi OS / Network **10.5.67** on a UDM Pro (2026-08-11) by sniffing the
Alarm Manager UI (`https://<udm>/network/default/alarm-manager`) and iterating on the API's
own validation errors. **This is not a documented Ubiquiti API** — treat it as version-fragile
and re-verify after UniFi OS updates. It is how outbound webhooks for Network events (internet
down, packet loss, failover, …) are configured programmatically; the official Integration API
(`X-API-KEY`) has no equivalent.

## Authentication

The alarms API lives at the UniFi OS layer (no `/proxy/network` prefix) and requires a
**cookie session + CSRF token** — the `X-API-KEY` header does NOT work here:

1. `POST /api/auth/login` with `{"username": "...", "password": "..."}` → sets `TOKEN` cookie (JWT).
2. The CSRF token is the `csrfToken` claim **inside the TOKEN JWT payload** (also returned as an
   `x-csrf-token` response header on login, but the JWT claim is what write requests must match).
3. Every mutating request needs the `x-csrf-token: <csrfToken>` header; otherwise 403 Forbidden.

## Endpoints

| Method + path | Purpose |
| --- | --- |
| `GET /api/v2/alarms/network` | list alarm rules (Network app scope) |
| `GET /api/v2/alarms/network/manifest` | catalog of trigger categories, action schemas (JSON Schema per action), scope schema |
| `POST /api/v2/alarms/network` | create rule (shape below) |
| `POST /api/v2/alarms/network/test` | test-fire (same body + an event-payload URL field we haven't pinned down; UI-only so far) |
| `GET /api/v2/alarms/profiles` | delivery profiles |
| `GET /proxy/network/v2/api/alarm-manager/scope/{sites,devices,clients,...}` | scope pickers |

## Create-rule body (the shape is NOT the same as the GET representation)

`triggers_data` and `actions_data` are **arrays of arrays** of `{id, data}` structs
(serde: sequence of sequences — a flat array of objects is rejected):

```json
{
  "title": "Reactor spike capture",
  "scope": { "mode": "include", "data": { "site_id": "ALL_ITEMS" } },
  "triggers_data": [[
    { "id": "network:internet_disconnected", "data": {} },
    { "id": "network:high_latency_detected", "data": {} },
    { "id": "network:packet_loss_detected", "data": {} }
  ]],
  "actions_data": [[
    {
      "id": "network:webhook",
      "data": {
        "url": "http://192.168.1.117:8080/webhooks/unifi",
        "method": "POST",
        "auth": { "variant": "none" }
      }
    }
  ]]
}
```

`201/200` returns the full rule (GET shape) including its `id`.

## Notes from the manifest (10.5.67)

- Internet trigger category (`network:category_internet`): `internet_disconnected`,
  `high_latency_detected`, `packet_loss_detected`, `data_limit`. No dedicated
  "failover" trigger was visible with a single WAN configured — re-check the manifest
  once the second WAN is installed (the system-log API knows a
  `INTERNET_OUTAGE_AND_FAILOVER` subcategory, so richer events likely appear).
- `network:webhook` action (Custom Webhook): `url` (http/https allowed, localhost/127.x
  rejected), `method` GET|POST, optional `auth` (`none` | `basic` | `bearer`), optional
  `headers` list, optional `custom_content`. Slack/ServiceNow variants exist with their
  own schemas.
- Trigger/action option schemas are self-describing JSON Schema in the manifest — an
  operator can validate config client-side before POSTing.

## What Reactor relies on, and how sure we are

`internal/providers/unifi/alarm.go` implements optional self-registration against this API.
It is off by default. Split by confidence:

**Observed on a real console (10.5.67), as recorded above:**

- cookie session from `POST /api/auth/login`, CSRF token from the `csrfToken` claim in the
  `TOKEN` JWT, echoed as `x-csrf-token` on writes
- `GET /api/v2/alarms/network/manifest`, `GET /api/v2/alarms/network`, `POST /api/v2/alarms/network`
- the arrays-of-arrays `triggers_data` / `actions_data` shape, and that a flat array is rejected
- the `network:webhook` action and the three Internet trigger IDs
- `auth` variants `none | basic | bearer` exist; localhost/127.x destinations are rejected

**Inferred, never confirmed against a console:**

- **the field name carrying the bearer value** — Reactor sends
  `"auth": {"variant": "bearer", "token": "<secret>"}`. The variant list is documented in the
  manifest; the key holding the value is a guess. If a console rejects the body, the error it
  returns is logged verbatim, and the rule can be created by hand in the UniFi UI with an
  `Authorization` header instead — the receiver also accepts `X-Reactor-Token`, which the
  Alarm Manager's custom-headers list can send.
- the shape of the manifest and of the rules list. Reactor never assumes a path through either:
  it searches the decoded document for the IDs it needs and for a rule carrying its own title,
  so a console that reorganizes its JSON degrades to "not offered" rather than to a decode error.

**Deliberately not used:**

- editing or deleting rules. `DELETE /api/v2/alarms/network/<id>` is an assumed verb, and `PUT`
  versus `PATCH` was never established. Reactor creates its rule and then leaves it alone
  forever; guessing wrong here means silently breaking somebody's alerting. A rule whose
  destination has gone stale is reported in the logs and left for a human to remove.

Every failure on this path is logged and swallowed. Polling remains the mechanism of record, so
the worst outcome of this API having moved is that Reactor reacts on the poll interval.

## Current state on the UDM

Rule **"Reactor spike capture"** (`019ff10d-5b3e-7930-8cd8-78745b579492`) exists,
pointing the three internet triggers at the spike capture logger
(`hack/webhook-logger.mjs` on `192.168.1.117:8080`). Delete it after the v0.0 spike:
`DELETE /api/v2/alarms/network/<id>` (with csrf header; verb assumed, verify first).
