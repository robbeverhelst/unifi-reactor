---
title: "UPS outlet state keys"
description: "outlet.<n> reports whether a switchable UPS outlet is delivering mains — the key you match on, and the one you name before you ever cut power to it."
---

## `outlet.<n>` is the position of one mains relay

A UniFi UPS reports one entry per switchable outlet, and Reactor publishes one
key each — `on` while the relay is closed and delivering mains, `off` when it is
not:

```yaml
  when:
    provider: unifi
    state:
      outlet.nas: off      # something cut power to the NAS
```

Switching one is a separate thing with its own guards:
[`unifi.outlet.cut` and `unifi.outlet.restore`](/actions/unifi-console/#switching-a-ups-outlet).
Observing has no guards and needs none — this key appears the moment a UPS with
an outlet table is adopted, and reading a relay costs nothing.

### The relay grouping, and what it turned out to mean

The capture shows outlets 1–4 reporting `relay_group: 1` and outlets 5–8
reporting `relay_group: 2`:

```json
{"index": 1, "name": "Outlet 1", "relay_state": true, "relay_group": 1}
```

That grouping is why switching was deferred for as long as it was. If the relay
**group** had been what the hardware switches, then "turn off outlet 3" would
have meant "cut outlets 1 to 4", and one of those may be carrying your gateway,
your switch or your storage.

**It is not.** On 2026-08-15 outlet 8 was set to `off` on real hardware and
outlets 5, 6 and 7 — which share its relay group — stayed on. `outlet_caps`
decodes with one extra capability bit on outlets 1–4, exactly where the hardware
documents four battery-backed and four surge-only outlets. So `relay_group`
partitions outlets by **what they can do**, not by what switches together, and
the outlets switch individually.

Reactor still prints the grouping when it first sees it, and still says what
moved together whenever a relay does — because a second UPS model is not obliged
to behave like the first one:

```sh
kubectl -n reactor-system logs deploy/reactor | grep unifi-outlets
# A UPS is reporting switchable outlets. On the hardware this was tested against
# they switch INDIVIDUALLY and the relay group below is a capability split ...
#   device=ups-2u relayGroups="1=[outlet.1 outlet.2 outlet.3 outlet.4] 2=[outlet.5 ...]"
# Outlet state changed. If you are checking whether this ups switches an outlet
# or a whole bank, this line is the readout
#   moved=outlet.5=on->off relayGroup=2 movedInGroup=1 outletsInGroup=4
#   verdict="outlets in this group moved independently of each other"
```

**Confirm it on your own hardware before allowlisting anything for
`unifi.outlet.cut`.** Toggle one outlet by hand — pick one in the bank carrying
nothing you care about — and read that second line. `movedInGroup=1` of `4` is
the answer this hardware gave; `4` of `4` would mean your UPS switches a whole
bank and must not be pointed at these actions. Both are rehearsable without
hardware; see [the mock](/contributing/development/#running-without-a-udm).

### Name your outlets

Out of the box every outlet is called `Outlet 1` … `Outlet 8`, which is the index
spelled out rather than a name, so the key falls back to the index: `outlet.3`.
Name them in the UniFi UI and the key becomes the name — `NAS` publishes
`outlet.nas`.

Do it before writing anything against them. `outlet.3` means something different
the day somebody re-plugs the rack, and this is the same argument that made
`portName` **required** rather than optional for
[`unifi.poe.cycle`](/actions/unifi-console/#power-cycling-a-poe-port): hardware that carries mains power
should be addressed by what it is, not by where it happens to be plugged.

For switching, it is not advice: **an outlet still carrying the placeholder is
refused.** Naming it is the only moment anybody writes down what the socket
feeds, and the UPS itself will never tell you.

Renaming an outlet makes its old key *vanish* rather than report `off`, which
Reactor treats as lost visibility — the last known state is held and `Ready=False`
reports `StateKeyUnavailable`, so nothing sheds load because you labelled a socket.
Two outlets addressed the same way publish **neither**, for the same reason two
devices sharing a slug do.

These keys appear only when an adopted device actually lists outlets. The captured
gateway reports `"outlet_table": []`, so having the field is not the same as having
outlets, and an outlet that reports no `relay_state` publishes nothing at all rather
than being read as off.

If more than one adopted device reports an outlet table, only the first is
published — outlet indexes restart on every chassis, so merging them would put one
device's outlet under another's key. Switching an outlet on the second device still
works, because an outlet action names its UPS by MAC.

They are also **not** in `reactor_state_info`, and the reasoning is worth
separating from [`device.<name>`'s](/operations/metrics-and-alerts/#cardinality-on-purpose), because only half of
it carries over — see [Cardinality](/operations/metrics-and-alerts/#cardinality-on-purpose).
