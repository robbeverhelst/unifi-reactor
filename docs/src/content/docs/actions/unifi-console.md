---
title: "UniFi console actions"
description: "Switching a WLAN off on a metered uplink, and power-cycling a PoE port. The two actions that change your network rather than read it — and every guard that stands in front of them."
---

## Changing things on your UniFi console

Everything above reaches *out* of the cluster to an address you allowlisted. The two actions here reach *back at the console Reactor watches*, and they are a different kind of risk: they are the first things Reactor changes on your network rather than reads from it, and the people they affect are not running the cluster.

> ⚠️ **Nothing here has ever been run against a real console.** The way Reactor authenticates a write was worked out against a live UDM Pro, but every endpoint under it is inferred from how UniFi's own web UI is understood to work. [Writing to a UniFi console](/contributing/unifi-write-api/) says exactly which is which. Everything is exercised against `hack/mock-unifi`, and a mock proves the wiring, not the protocol.

Three properties hold for both, and they are what make them safe enough to ship at all:

- **You decide what may be touched, at install time.** `unifi.actions.allowedWlans` and `unifi.actions.allowedPoePorts` are Helm values, both empty by default, and empty refuses everything with a reason naming the value to add. There is no per-automation override — `spec.actions` is writable by anyone who can create an `Automation` in their own namespace, and turning the WiFi off is not a decision that belongs there.
- **Every step checks before it writes.** Read the object, confirm it is the one the automation meant, then act. A check that fails abandons the action and says what did not match; it never writes anyway and it never writes something else.
- **Attempted exactly once.** No retry, in either direction. See [when an action fails](/concepts/arbitration/#when-an-action-fails) — the next transition corrects a miss, and nothing corrects a duplicate.

They need a **UniFi OS local account**, because the API key the poller reads with does not write:

```sh
kubectl -n reactor-system create secret generic unifi-reactor-console \
  --from-literal=UNIFI_USERNAME=reactor \
  --from-literal=UNIFI_PASSWORD='...'
```

That is the same Secret the [Alarm Manager registration](/operations/webhook-fast-path/) uses, and it is the same credential — same layer, same session, same CSRF token. Reactor holds no session: it logs in, acts, and logs out, once per action.

### Switching a wireless network off

On a metered 5G uplink, guest WiFi is pure cost:

```yaml
  when:
    provider: unifi
    state: {wan: backup}
  actions:
    - type: unifi.wlan.disable
      wlan: {name: Guest}
  onExit:
    - type: unifi.wlan.enable
      wlan: {name: Guest}
```

```yaml
# values.yaml — without this, the action above is refused
unifi:
  actions:
    allowedWlans:
      - Guest
```

Only `enabled` is ever changed. The write is a read-modify-write, because `rest/wlanconf` offers nothing narrower: Reactor reads the WLAN, changes that one key, and PUTs back **the record it just read**, so it never invents a value for a field it does not understand. It also writes nothing at all when the WLAN is already where you asked for it. What it cannot avoid is that a change you make in the UniFi UI in the two-request window between the read and the write is lost.

#### It is a level, and an edge action, and this one bites

A WLAN being enabled is a level in exactly the way [pausing torrents is](/actions/external-services/#it-is-a-level-in-the-world-and-an-edge-action-here), and it is an edge action for exactly the same reason: there is nowhere to record what it was before Reactor touched it. Writing that into the WLAN's own configuration is the torrent-tag mistake — it is your config, you can edit it, and the write carrying it has no concurrency control. And releasing a WLAN would mean a credentialed write to the console, which the pre-delete sweep during an uninstall is *designed* to be incapable of.

So, two limitations, and the second is louder here than anywhere else in this README:

- **It is not arbitrated.** Two automations disabling the same SSID do not resolve to one claim; whichever enables it first enables it.
- **Nothing hands it back.** If the exit transition never arrives — you delete the automation, you uninstall Reactor, the state key stops being observable — **the network stays off until a human turns it back on.** There is no baseline, no release, and no pre-delete sweep that can reach it.

Point it at a network whose absence is an inconvenience, not at the one carrying your phones, your cameras, or Reactor's own path to the controller. Reactor has no way to know which is which, which is why the allowlist is yours to write and is empty until you do.

### Power-cycling a PoE port

The classic fix for a wedged access point or camera, and the natural partner of a `device.<name>: offline` key:

```yaml
  actions:
    - type: unifi.poe.cycle
      poe:
        device: aa:bb:cc:00:11:22   # the switch's MAC
        port: 7
        portName: hallway-ap        # what that port is called, checked first
```

```yaml
# values.yaml
unifi:
  actions:
    allowedPoePorts:
      - aa:bb:cc:00:11:22/7
```

**This is the action where a wrong target does visible damage** — the wrong port drops an access point, a camera, or the switch uplink carrying your cluster — and the console will accept the wrong one exactly as readily as the right one. So a port is identified by three things that must all agree with the switch's own port table, checked immediately before the command is sent:

| | Why |
| --- | --- |
| `device`, a **MAC** | A device name is a label. Renaming a switch would silently repoint the action; a MAC identifies the hardware. |
| `port`, an index | What the console addresses. |
| `portName`, **required** | The one that does the real work. An index alone means "whatever is in slot 7 *now*", and after somebody re-patches a rack that is a different thing. Naming what is supposed to be there turns a re-patch into a refused action with a sentence, instead of a power cut to something else. |

Both halves are required in the allowlist too, for the same reason: `aa:bb:cc:00:11:22/7`, never just `7`.

Three refusals apply **whatever you allowlist**, in the same way the [outbound dialer refuses loopback](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#outbound-actions) whatever the destination allowlist says:

- **The switch's own uplink is never cycled.** That port carries everything behind the switch — quite possibly including Reactor's path to the console.
- **A port the switch does not report as PoE-capable is never cycled.** There is nothing there to cut, and the identity is probably wrong.
- **A switch that does not report those fields at all is refused**, rather than assumed safe. A guard that silently stops applying is worse than one that declines out loud.

#### The loop, and why debounce is the answer rather than a cooldown

[#25](https://github.com/robbeverhelst/unifi-reactor/issues/25) warns about the obvious disaster: an AP fails to come back, the automation stays matched, and something keeps bouncing the port. **The shape of this engine is what prevents it.** Reactor acts on *transitions*, and an AP that never comes back leaves the automation **matched** — matched is not a transition, so nothing fires again. There is no retry either, in the reconcile or across reconciles.

What can still drive it repeatedly is a **flapping** key, and there the answer is the same one [`kubernetes.restart` uses](/actions/kubernetes/#restart-is-why-debounce-matters) — the engine's debounce, not a cooldown inside the action:

```yaml
unifi:
  debounce:
    keys:
      device.hallway-ap: 3    # three observations before Reactor believes it is down
```

A cooldown was considered and not built. It would be a second, weaker debounce living inside one action, invisible to the engine and to every other action beside it, and it would swallow a *legitimate* second cycle as readily as a pathological one. Reactor already has a mechanism for "do not believe this too quickly", and adding a private one next to it would make both harder to reason about. If a key drives a power cut, raise its debounce and accept one `pollInterval` per extra sample.

## The install values, and what they do not add

With both lists empty — the default — every `unifi.*` action is refused, and the Automation says which value to add. There is no `*`: "any SSID" and "any port" are not choices this chart offers.

**These need a second credential.** The API key the poller reads with does not write; the write path uses a UniFi OS local account, the same one Alarm Manager self-registration uses:

```bash
kubectl -n reactor-system create secret generic unifi-reactor-console \
  --from-literal=UNIFI_USERNAME=reactor \
  --from-literal=UNIFI_PASSWORD='...'
```

Setting either list injects that Secret. **No new RBAC** — a console write goes to your gateway, not to the API server — and no new outbound destination, since the console's address is `unifi.url` and not something an `Automation` chooses.
