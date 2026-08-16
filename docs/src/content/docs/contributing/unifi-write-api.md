---
title: "Writing to a UniFi console"
description: "What the unifi.* actions send, split honestly into what was observed against real hardware and what is inferred — including the one write that has been made to a real console, and what it did not prove."
---

The `unifi.*` actions are the first things Reactor **changes** on a UniFi console. Everything
else the provider does is read-only, apart from creating its own Alarm Manager rule.

This page exists for one reason: to say plainly which parts of that write path have been seen
working against real hardware and which have not. The short version is that the **authentication**
is observed, the **outlet write is observed**, and everything else is inferred. Read [the Alarm
Manager notes](/contributing/unifi-alarm-manager-api/) first — the auth is the same, and this page
does not repeat it.

> ⚠️ **Only the outlet write has ever been made to a real console.** Every other path and field
> name here comes from how UniFi's own web UI is understood to drive the Network application, not
> from a capture. Treat those as version-fragile, and see the LIVE VERIFICATION section of the pull
> request that added them.
>
> And the outlet write proves less than it looks like it does — see
> [The outlet write](#the-outlet-write--observed-once-and-what-that-is-worth).

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

## The endpoints

Unlike the alarms API, these sit under the Network application's `/proxy/network` prefix — the same
prefix the poller reads through — while authenticating at the UniFi OS layer above it.

| Method + path | Used by | Confidence |
| --- | --- | --- |
| `GET /proxy/network/api/s/<site>/rest/wlanconf` | `unifi.wlan.*`, the read half | inferred |
| `PUT /proxy/network/api/s/<site>/rest/wlanconf/<_id>` | `unifi.wlan.*`, the write | inferred |
| `GET /proxy/network/api/s/<site>/stat/device` | `unifi.poe.cycle`, the port check | **observed** as a read; the `port_table` fields below are not |
| `POST /proxy/network/api/s/<site>/cmd/devmgr` | `unifi.poe.cycle`, the command | inferred |
| `GET /proxy/network/api/s/<site>/stat/device` | `unifi.outlet.*`, the outlet check | **observed** as a read, and the outlet fields below with it |
| `PUT /proxy/network/api/s/<site>/rest/device/<_id>` | `unifi.outlet.*`, the write | **observed** — accepted with HTTP 200 on 2026-08-15 |
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
| `_id` (device) | outlet write | refused — there is no address to PUT to |
| `outlet_table[].index` | outlet lookup | the outlet is not found |
| `outlet_table[].name` | outlet drift check | **refused** |
| `outlet_table[].relay_state` | outlet position | **refused** — nothing to compare the wanted position against |
| `outlet_table[].outlet_caps` | battery-backed floor | **refused** |
| `outlet_overrides` | outlet write | **refused** — Reactor will not compose one |
| `outlet_overrides[].index` | outlet write | **refused** |

The bold rows are the design decision worth arguing with. A missing field could have been
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

## The outlet write — observed once, and what that is worth

This is the only write in this document that has been made to real hardware, so it is worth being
precise about what was learned and what was not.

**What was done**, on 2026-08-15:

```
PUT /proxy/network/api/s/<site>/rest/device/<_id>
{"outlet_overrides": [ ...all eight outlets, one with relay_state changed... ]}
→ HTTP 200
```

**What it established.** The endpoint exists, accepts a body of exactly `outlet_overrides`, and
answers 200. Setting outlet 8 to `relay_state: false` changed **only** outlet 8 — outlets 5, 6 and
7, which share its `relay_group`, stayed on. `relay_group` is therefore a capability partition
(battery-backed versus surge-only, corroborated by `outlet_caps`) and not a switching bank. It was
restored afterwards and verified.

**What it did not establish, and this is the important half.** The outlet under test was **empty**.
So the only evidence that anything happened is the console reporting back the value that was
written to it. A console that recorded the override without ever driving the relay would produce
exactly the same result. **Nothing here proves the relay physically opens.** The lamp test — plug
something in, drive a transition, watch it go dark — is the first item in that pull request's LIVE
VERIFICATION, and until somebody runs it this is a capability the operator *believes* they have.

**One further gap.** That write authenticated with a plain `X-API-KEY` header, not with the cookie
session this page describes. Reactor uses the session anyway, because a second authentication
posture inside one write path is a second thing to get wrong, and because every check lives on the
session path already. That the *session* is accepted by `rest/device` is therefore inferred, not
observed — a UniFi OS session is strictly more privileged than an API key, so it should be, but
"should be" is what this page exists to flag.

### The body, and why it is narrower than the WLAN one

Unlike `rest/wlanconf`, this write is a single field. Reactor sends back the `outlet_overrides`
array the console just served, with **exactly one entry's `relay_state` changed** — every other
outlet's entry, and every other key on the addressed one, carried through untouched. It never
composes the array itself: a UPS reporting no `outlet_overrides`, or none for the outlet addressed,
is a refusal, because a document Reactor invented would state a position for every relay rather
than for the one asked about.

`hack/mock-unifi` enforces precisely that. It rejects a PUT whose body is not the array just read
with one `relay_state` changed, and names what else differed — which is the one check a real
console cannot make for you, and the change nobody would notice in review.

The identity is checked against `outlet_table` before any of it: see the `Outlet` type in
`api/v1alpha1` for why an outlet is named by a MAC, an index **and** a name, and why one still
called `Outlet 5` is refused outright.

## Rehearsing it without hardware

`hack/mock-unifi` serves and enforces all of the above:

```sh
make dev-mock

curl http://localhost:9443/wlan                                   # what the console holds
curl -X POST 'http://localhost:9443/wlan?name=mock-guest&enabled=false'
curl http://localhost:9443/poe                                    # ports, and every cycle so far
curl http://localhost:9443/outlets                                # outlets, banks, and every write so far

# break each identity check on purpose, and watch Reactor refuse
curl -X POST 'http://localhost:9443/poe?port=7&name=re-patched'
curl -X POST 'http://localhost:9443/poe?port=7&uplink=true'
curl -X POST 'http://localhost:9443/poe?port=7&poe=false'

# the outlet floors: a bank Reactor cannot read, and nothing to modify
curl -X POST 'http://localhost:9443/outlets?caps=false'
curl -X POST 'http://localhost:9443/outlets?overrides=false'
curl -X POST 'http://localhost:9443/outlets?outlet=5&label=bench'  # name one, so it can be switched
```

The WLAN records and the switch there are **not captures** and are labelled as such in the mock's
own output. They are built from the field names on this page. Registration working against the mock
means Reactor sends what this page describes; it does not mean a console accepts it.

The outlet table **is** a capture; `_id`, `outlet_caps` and `outlet_overrides` are not, and are the
shape read off the real UPS on the date above. `hack/capture-unifi.sh` now projects all three, so
the next capture will carry them.

One deliberate asymmetry: **the mock does not refuse a cycle of the uplink**, an unnamed outlet, a
battery-backed one, or anything an allowlist would have stopped. A real console would accept all of
them without complaint, so a mock that refused them would let Reactor's own refusals rot untested —
and on real hardware, Reactor's are the only guards there are.

## What would settle this

The equivalent of the failover runbook in [`testdata/unifi/README.md`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md),
and it needs a console somebody is willing to change. In rough order of value:

1. Does `PUT rest/wlanconf/<id>` accept a full record and apply only the changed field?
2. Does `cmd/devmgr` accept `power-cycle` with `mac` + `port_idx`, and on which device types?
3. Does a switch's `port_table` actually carry `is_uplink`, `port_poe` and `poe_enable` as booleans?
4. Does `POST /api/auth/logout` end the session, and does the console mind being logged in and out
   once per action?
5. **Does the relay actually open?** Plug a lamp into an allowlisted outlet, drive a transition,
   watch it go dark. This one is cheap, and it is the only thing that turns the outlet write from
   "the console agreed with itself" into a working feature.
6. Does `rest/device` accept the **cookie session** as well as the API key? The one observed write
   used `X-API-KEY`; Reactor sends the session.

Nothing here should be captured into `testdata/` by hand. If a fixture is ever wanted for these,
it goes through `hack/capture-unifi.sh` with the allowlist extended deliberately, one field at a
time — a live device credential reached this repository's history through a fixture shortcut once.
