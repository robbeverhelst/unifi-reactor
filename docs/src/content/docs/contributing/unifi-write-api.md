---
title: "Writing to a UniFi console"
description: "What the unifi.* actions send, split honestly into what was observed against real hardware and what is inferred — including the read-modify-write the WLAN endpoint forces."
---

The `unifi.*` actions are the first things Reactor **changes** on a UniFi console. Everything
else the provider does is read-only, apart from creating its own Alarm Manager rule.

This page exists for one reason: to say plainly which parts of that write path have been seen
working against real hardware and which have not. The short version is that the **authentication**
is observed and **every endpoint below it is inferred**. Read [the Alarm Manager
notes](/contributing/unifi-alarm-manager-api/) first — the auth is the same, and this page does not repeat it.

> ⚠️ **No write in this document has ever been made to a real console from this repository.**
> The paths and field names come from how UniFi's own web UI is understood to drive the Network
> application, not from a capture. Treat this as version-fragile, and see the LIVE VERIFICATION
> section of the pull request that added it.

## Authentication — OBSERVED

Identical to the Alarm Manager path, and reached through the same client in
`internal/providers/unifi/alarm.go` rather than a second implementation of it:

1. `POST /api/auth/login` with `{"username": ..., "password": ...}` → a `TOKEN` cookie (a JWT).
2. The CSRF token is the `csrfToken` claim **inside** that JWT.
3. Every mutating request carries `x-csrf-token: <csrfToken>`; without it, 403.

Two things follow, and both are decisions rather than details:

- **The API key does not write.** `X-API-KEY` reads `stat/device` and `stat/health` perfectly well
  (Network 10.5), and the write path needs a UniFi OS local account instead. That is why
  `unifi.actions.*` requires the same `UNIFI_USERNAME` / `UNIFI_PASSWORD` pair the Alarm Manager
  registration does.
- **No session is held.** Each action logs in, does its work, and `POST /api/auth/logout`s. A UniFi
  OS session cookie is a bearer of the same authority as the password that made it, so caching one
  across reconciles would be exactly what this project refuses to do with the password itself. The
  cost is one extra round trip per action. **The logout verb is INFERRED**; a console that does not
  offer it simply lets the session age out, which is what would have happened anyway.

## The endpoints — ALL INFERRED

Unlike the alarms API, these sit under the Network application's `/proxy/network` prefix — the same
prefix the poller reads through — while authenticating at the UniFi OS layer above it.

| Method + path | Used by | Confidence |
| --- | --- | --- |
| `GET /proxy/network/api/s/<site>/rest/wlanconf` | `unifi.wlan.*`, the read half | inferred |
| `PUT /proxy/network/api/s/<site>/rest/wlanconf/<_id>` | `unifi.wlan.*`, the write | inferred |
| `GET /proxy/network/api/s/<site>/stat/device` | `unifi.poe.cycle`, the port check | **observed** as a read; the `port_table` fields below are not |
| `POST /proxy/network/api/s/<site>/cmd/devmgr` | `unifi.poe.cycle`, the command | inferred |
| `POST /api/auth/logout` | ending the session | inferred |

`stat/device` is the one endpoint here the poller already reads on every cycle, so *that it answers*
is observed. Which fields a **switch** record carries is not: every committed capture is of a
gateway or a UPS, and `testdata/unifi/README.md` lists them.

### Fields read, and what happens when one is missing

| Field | Read by | If absent |
| --- | --- | --- |
| `_id` | WLAN write | refused — there is no address to PUT to |
| `name` (WLAN) | WLAN lookup | the WLAN is not found, and the refusal does not list the ones that are |
| `enabled` | WLAN read and write | refused — the state this action assumes is not the one the console describes |
| `mac` | PoE device lookup | the device is not found |
| `port_table[].port_idx` | PoE port lookup | the port is not found |
| `port_table[].name` | PoE drift check | **refused** |
| `port_table[].is_uplink` | PoE uplink floor | **refused** |
| `port_table[].port_poe` | PoE capability floor | **refused** |
| `port_table[].poe_enable` | PoE state check | allowed — capability is the load-bearing check |

The three bold rows are the design decision worth arguing with. A missing field could have been
treated as "not an uplink" and "probably fine", and it is treated as a refusal instead, because a
safety check that silently stops applying on some firmware is worse than one that declines out
loud. If a real console turns out not to report them, the error says which field was missing — that
is a code change and a bug report, not something to work around.

## The WLAN write is a read-modify-write, and that is a real limitation

`rest/wlanconf` offers no field-level update and no version to compare against. So the action reads
the WLAN record, changes exactly one key, and PUTs the whole thing back.

Two things bound what that shape can do wrong:

- **Reactor sends back the object it just read**, so it never invents a value for a field it does
  not understand. The mock enforces this: `hack/mock-unifi` rejects a PUT whose body differs from
  the stored record in anything other than `enabled`.
- **It does not write at all when the WLAN is already where the automation wants it**, which is the
  common case for a repeated transition.

What it cannot bound: **a change made in the UniFi UI between the read and the write is lost.** That
window is two adjacent requests wide. There is nothing in this API to make it smaller, and
pretending otherwise would be worse than saying so.

The write is checked afterwards, too — the console answers a write with the object it stored, and a
`200` that did not take is reported as a failure rather than assumed to be a success. That is the
failure mode an undocumented endpoint is most likely to have.

## The PoE command

```json
{"cmd": "power-cycle", "mac": "<switch mac>", "port_idx": 7}
```

Addressed by the console's own port index rather than by a position in the table, because the table
is not guaranteed to be ordered or complete.

Almost all of `cyclePoEPort` is the check rather than the command, and that is the right
proportion: the console will accept a cycle of the wrong port exactly as readily as the right one,
and Reactor would never hear about the difference. See the `PoEPort` type in `api/v1alpha1` for why
a port is identified by a MAC, an index **and** a name.

## Rehearsing it without hardware

`hack/mock-unifi` serves and enforces all of the above:

```sh
make dev-mock

curl http://localhost:9443/wlan                                   # what the console holds
curl -X POST 'http://localhost:9443/wlan?name=mock-guest&enabled=false'
curl http://localhost:9443/poe                                    # ports, and every cycle so far

# break each identity check on purpose, and watch Reactor refuse
curl -X POST 'http://localhost:9443/poe?port=7&name=re-patched'
curl -X POST 'http://localhost:9443/poe?port=7&uplink=true'
curl -X POST 'http://localhost:9443/poe?port=7&poe=false'
```

The WLAN records and the switch there are **not captures** and are labelled as such in the mock's
own output. They are built from the field names on this page. Registration working against the mock
means Reactor sends what this page describes; it does not mean a console accepts it.

One deliberate asymmetry: **the mock does not refuse a cycle of the uplink.** A real console would
accept that command without complaint, so a mock that refused it would let Reactor's own refusal rot
untested — and on real hardware, Reactor's is the only guard there is.

## What would settle this

The equivalent of the failover runbook in [`testdata/unifi/README.md`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md),
and it needs a console somebody is willing to change. In rough order of value:

1. Does `PUT rest/wlanconf/<id>` accept a full record and apply only the changed field?
2. Does `cmd/devmgr` accept `power-cycle` with `mac` + `port_idx`, and on which device types?
3. Does a switch's `port_table` actually carry `is_uplink`, `port_poe` and `poe_enable` as booleans?
4. Does `POST /api/auth/logout` end the session, and does the console mind being logged in and out
   once per action?

Nothing here should be captured into `testdata/` by hand. If a fixture is ever wanted for these,
it goes through `hack/capture-unifi.sh` with the allowlist extended deliberately, one field at a
time — a live device credential reached this repository's history through a fixture shortcut once.
