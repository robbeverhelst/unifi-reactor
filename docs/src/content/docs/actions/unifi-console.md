---
title: "UniFi console actions"
description: "Switching a WLAN off on a metered uplink, power-cycling a PoE port, and cutting mains to a UPS outlet. The actions that change your network rather than read it — and every guard that stands in front of them."
---

## Changing things on your UniFi console

Everything above reaches *out* of the cluster to an address you allowlisted. The actions here reach *back at the console Reactor watches*, and they are a different kind of risk: they are the things Reactor changes on your network rather than reads from it, and the people they affect are not running the cluster.

> ⚠️ **Almost nothing here has ever been run against a real console.** The way Reactor authenticates a write was worked out against a live UDM Pro, and one outlet write was made to real hardware — see [Switching a UPS outlet](#switching-a-ups-outlet), including what that write did *not* prove. Every other endpoint is inferred from how UniFi's own web UI is understood to work. [Writing to a UniFi console](/contributing/unifi-write-api/) says exactly which is which. Everything is exercised against `hack/mock-unifi`, and a mock proves the wiring, not the protocol.

Three properties hold for all of them, and they are what make them safe enough to ship at all:

- **You decide what may be touched, at install time.** `unifi.actions.allowedWlans`, `unifi.actions.allowedPoePorts` and `unifi.actions.allowedOutlets` are Helm values, all empty by default, and empty refuses everything with a reason naming the value to add. There is no per-automation override — `spec.actions` is writable by anyone who can create an `Automation` in their own namespace, and turning the WiFi off is not a decision that belongs there.
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

### Switching a UPS outlet

The reason this project was started: on battery, the cluster can cut power to hardware it does not need and buy itself runtime, instead of only stopping software.

```yaml
  when:
    provider: unifi
    state: {ups: on-battery}
  actions:
    - type: unifi.outlet.cut
      outlet:
        device: aa:bb:cc:00:11:44   # the UPS's MAC
        index: 5                    # the socket, as the console numbers it
        name: bench                 # what that socket is called, checked first
  onExit:
    - type: unifi.outlet.restore
      outlet: {device: aa:bb:cc:00:11:44, index: 5, name: bench}
```

```yaml
# values.yaml
unifi:
  actions:
    allowedOutlets:
      - aa:bb:cc:00:11:44/5/bench
```

> ⚠️ **Nothing has established that the relay physically opens.** A write to real hardware on 2026-08-15 was accepted, and the console then reported the new position back — but the outlet under test was **empty**. A console that recorded the override without driving the relay would look exactly the same from here. **Plug a lamp into an allowlisted outlet, drive a transition, and watch it go dark** before you trust this with anything that matters. Until you have done that, this is a capability you *believe* you have.

#### This is the action Reactor can help you with least

Every other dangerous action has something the hardware will tell Reactor. A switch reports which of its ports is its own uplink, so `unifi.poe.cycle` can refuse that one absolutely, whatever you allowlist. **A UPS reports nothing at all about what is plugged into an outlet** — not the device, not whether it is your gateway, not whether anything is there. Cutting the wrong outlet drops whatever was on it, and Reactor never hears about the difference.

So the identity is stricter than anywhere else, and it takes three things that must all agree with the UPS's own outlet table, checked immediately before the relay moves:

| | Why |
| --- | --- |
| `device`, a **MAC** | A device name is a label. Renaming the UPS would silently repoint the action. |
| `index` | The socket, as the console numbers it. |
| `name`, **required** | The only part that is a *thing* rather than a position. If it stops matching, the action is refused with a sentence instead of cutting power to whatever is there now. |

**The allowlist entry carries all three** — `aa:bb:cc:00:11:44/5/bench`, never `aa:bb:cc:00:11:44/5`. That is one more part than [`allowedPoePorts`](#power-cycling-a-poe-port) asks for, and the reason is worth stating: two of them are a position. An allowlist of MAC and index means you agreed to *whatever is in outlet 5*, and after somebody re-plugs the rack that is something else — even if the automation naming it is perfectly correct.

#### Name your outlets first, and Reactor will not proceed until you have

Out of the box these outlets are called `Outlet 1` … `Outlet 8`. That is the index spelled out, not a name, and **an outlet still carrying it is refused** — by the API server when you write the automation, by the chart values when you list it, and by Reactor against the console before it writes.

That is not bureaucracy. Naming an outlet in UniFi is the only moment anybody writes down what this socket feeds, and it is the entire defence described above. It is also what turns the [`outlet.<n>` state key](/state-keys/outlets/) into something readable: name outlet 5 `bench` and it publishes as `outlet.bench`.

#### The battery-backed bank takes a second, separate switch

This UPS splits its outlets into a **battery-backed** bank and a **surge-only** bank, and Reactor reads which is which from the UPS itself. Listing a battery-backed outlet is not enough — it is refused, whatever the allowlist says, until you also set:

```yaml
unifi:
  actions:
    allowBatteryBackedOutlets: true
```

Two values rather than one, because they are two decisions: *which sockets* are on the table, and *whether the ones that keep running during a power cut* are among them. Cutting a battery-backed outlet mid-outage is the most damaging thing Reactor can be configured to do.

And it is unavoidably the only cut that helps. **A surge-only outlet is already dark when the mains are**, so shedding it during an outage saves nothing at all — if load-shedding to extend runtime is why you are here, you need this switch, and turning it on is you saying you know what is on that bank. That is why it is a consent rather than a floor: a floor would have made the feature pointless.

An outlet whose bank Reactor *cannot* read is refused outright and always, the same way a switch that will not say which port is its uplink is. A guard that silently stops applying is worse than one that declines out loud.

#### What it writes, and the two limitations

The write is narrower than the WLAN one: Reactor sends back the outlet override array the console just gave it with **exactly one entry's relay position changed**, and nothing else touched. It writes nothing at all when the outlet is already where you asked for it.

The same two limitations as `unifi.wlan.*` apply, and on mains power they are louder:

- **It is not arbitrated.** Two automations cutting the same outlet do not resolve to one claim; whichever restores it first restores it.
- **Nothing hands it back.** Delete the automation, uninstall Reactor, or lose sight of the state key mid-outage, and **the outlet stays open until a human closes it.** There is no baseline and no pre-delete sweep that can reach a relay.

`restore` is named as the pair of `cut` and means only that the relay is closed again. It restores nothing that was recorded, because nothing was.

One more thing worth knowing: **the UniFi UI offers no outlet control for this device.** Reactor is doing something the vendor's own interface does not, which means there is no second opinion when something behaves unexpectedly, and no button to undo a mistake with.

## The install values, and what they do not add

With every list empty — the default — every `unifi.*` action is refused, and the Automation says which value to add. There is no `*`: "any SSID", "any port" and "any outlet" are not choices this chart offers.

**These need a second credential.** The API key the poller reads with does not write; the write path uses a UniFi OS local account, the same one Alarm Manager self-registration uses:

```bash
kubectl -n reactor-system create secret generic unifi-reactor-console \
  --from-literal=UNIFI_USERNAME=reactor \
  --from-literal=UNIFI_PASSWORD='...'
```

Setting any of the lists injects that Secret. **No new RBAC** — a console write goes to your gateway, not to the API server — and no new outbound destination, since the console's address is `unifi.url` and not something an `Automation` chooses.

`unifi.actions.allowBatteryBackedOutlets` is the one value here that allows nothing on its own. It only qualifies `allowedOutlets`, so setting it by itself turns nothing on — not even the credential.
