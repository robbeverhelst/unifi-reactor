---
title: "UPS outlet state keys"
description: "outlet.<n> reports whether a switchable UPS outlet is delivering mains — and is read-only, because nobody has confirmed whether the hardware switches an outlet or a whole relay group."
---

## `outlet.<n>` is read-only, and the relay grouping is why

A UniFi UPS reports one entry per switchable outlet, and Reactor publishes one
key each — `on` while the relay is closed and delivering mains, `off` when it is
not:

```yaml
  when:
    provider: unifi
    state:
      outlet.nas: off      # something cut power to the NAS
```

**Reactor will not switch an outlet.** Not behind a flag, not with an allowlist,
not at all — and that is not caution about writes in general, since Reactor
already power-cycles a PoE port. It is one specific unanswered question, visible
in the capture:

```json
{"index": 1, "name": "Outlet 1", "relay_state": true, "relay_group": 1}
```

Outlets 1–4 report `relay_group: 1` and outlets 5–8 report `relay_group: 2`. If
the relay **group** is what the hardware switches, then "turn off outlet 3" means
"cut outlets 1 to 4", and one of those may be carrying your gateway, your switch
or your storage. Nobody has confirmed which it is. The documented write path
([`outlet_overrides`](https://github.com/Art-of-WiFi/UniFi-API-client/blob/main/examples/modify_smartpower_pdu_outlet.php))
comes from the USP-PDU-Pro and USP-Strip, which expose per-outlet power and
current and have **no relay groups at all**, so it is documented for a different
device class and settles nothing here. Switching is tracked in
[#23](https://github.com/robbeverhelst/unifi-reactor/issues/23) and stays there
until it is answered.

Observing is what answers it, safely. Reactor prints the grouping when it first
sees it, and says what moved together whenever a relay does:

```sh
kubectl -n reactor-system logs deploy/reactor | grep unifi-outlets
# A UPS is reporting switchable outlets. Reactor only READS them ...
#   device=ups-2u relayGroups="1=[outlet.1 outlet.2 outlet.3 outlet.4] 2=[outlet.5 outlet.6 outlet.7 outlet.8]"
# Outlet state changed. If you are running the relay-group experiment on issue #60, this line is its readout
#   moved=outlet.5=on->off relayGroup=2 movedInGroup=1 outletsInGroup=4
#   verdict="outlets in this group moved independently of each other"
```

Toggle **one** outlet by hand in the UniFi UI — pick one in the bank carrying
nothing you care about — and that second line is the whole experiment:
`movedInGroup=1` of `4` means outlets switch individually, and `4` of `4` means
the relay group is the switching unit. Both are rehearsable without hardware; see
[the mock](/contributing/development/#running-without-a-udm).

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

Renaming an outlet makes its old key *vanish* rather than report `off`, which
Reactor treats as lost visibility — the last known state is held and `Ready=False`
reports `StateKeyUnavailable`, so nothing sheds load because you labelled a socket.
Two outlets addressed the same way publish **neither**, for the same reason two
devices sharing a slug do.

These keys appear only when an adopted device actually lists outlets. The captured
gateway reports `"outlet_table": []`, so having the field is not the same as having
outlets, and an outlet that reports no `relay_state` publishes nothing at all rather
than being read as off.

They are also **not** in `reactor_state_info`, and the reasoning is worth
separating from [`device.<name>`'s](/operations/metrics-and-alerts/#cardinality-on-purpose), because only half of
it carries over — see [Cardinality](/operations/metrics-and-alerts/#cardinality-on-purpose).
