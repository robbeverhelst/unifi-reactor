---
title: "Chart values"
description: "Every value the Reactor Helm chart takes, its default, and the note written above it in values.yaml."
editUrl: false
tableOfContents:
  maxHeadingLevel: 2
---

:::note[Generated from source]
This page is generated from `charts/reactor/values.yaml` by `make docs`. Change it there — CI fails when this file and its source disagree.
:::

Every value the chart takes, with the default it ships and the note written above it in `values.yaml`. A key with no note is one whose name says all there is to say.

Values are grouped as they are nested. A mapping nobody has written a note about — and a mapping whose own keys come from your network rather than from the chart, like `unifi.debounce.keys` — is shown whole, as the YAML it defaults to.

## `crds`

### `crds.install`

Default: `true`

The Automation CRD ships as a chart template, not in crds/, because Helm
installs crds/ on first install only and never upgrades it. It carries
helm.sh/resource-policy: keep, so uninstalling the release leaves the CRD —
and every Automation stored under it — in place.
Set to false when the CRD is managed outside this release (GitOps, or a
cluster where only an admin may touch CRDs); the chart README documents
what to apply and when.

### `crds.adopt`

Default: `true`

Take over a CRD that belongs to no Helm release, which is what chart 0.3.0
and earlier left behind by shipping it under crds/. Without this the first
upgrade from those versions fails with "invalid ownership metadata" until
the CRD is labelled and annotated by hand.

It happens on exactly one upgrade per install, and only when there is
something to adopt: a hook Job — with its own ServiceAccount and a
ClusterRole granting get and patch on that single CRD name — is rendered
only in that case, and cleans itself up when it succeeds. A CRD owned by
another release is never adopted; that upgrade stops and says whose it is.

On that one upgrade the chart deliberately leaves the CRD out of the
release, because Helm checks ownership before it runs any hook, and the hook
puts the schema live in the same patch that takes ownership. So `helm get
manifest` for that revision carries no CustomResourceDefinition while `helm
template` renders one; the next upgrade puts it back for good. The chart
README explains it under "helm get manifest shows no CRD after that
upgrade".

Set to false to do it by hand instead
(https://reactor.robbeverhelst.com/troubleshooting/ has the two kubectl
commands). That is a promise to run them: until somebody does, the CRD
belongs to no release and Helm will not update it, so the upgrade stops and
repeats the commands back to you. Ignored when install is false, which
already says the CRD is somebody else's to manage.

## `image`

### `image.repository`

Default: `ghcr.io/robbeverhelst/unifi-reactor`

### `image.tag`

Default: `""`

Defaults to the chart's appVersion.

### `image.pullPolicy`

Default: `IfNotPresent`

## `unifi`

### `unifi.url`

Default: `""`

Base URL of the UniFi console, e.g. https://192.168.1.1

### `unifi.site`

Default: `default`

UniFi Network site (internal reference). Leave as "default" unless you use multiple sites.

### `unifi.pollInterval`

Default: `30s`

How often to poll WAN state. Polling is the source of truth for state triggers.

### `unifi.maxObservationAge`

Default: `""`

How old the observed state may get before every automation driven by this
provider reports Ready=False with reason ObservationStale, raises a Warning
Event, and counts reactor_stale_decisions_total.

Empty — the default — means unbounded, which is what every install had
before this value existed: if the console stops answering, automations go on
acting on the last state it reported and nothing on them says how old it is.
The only signal is the fleet-wide
`time() - reactor_last_observation_timestamp_seconds`, which needs metrics
enabled and somebody watching a graph.

This bounds what Reactor SAYS, never what it does. A stale observation does
not release a claim, does not run onExit, and does not change what any target
is held at — that would scale workloads back up during the outage that took
the console away, which is precisely the failure holding last known state
exists to prevent. It is the same rule as StateKeyUnavailable, applied to the
whole observation rather than to one key.

Set it against pollInterval and the debounce samples below rather than in
isolation. A changed value already takes up to pollInterval times its key's
sample count to be believed — 90s for `internet` at the defaults — so
anything under about four poll intervals reports a slow console rather than a
blind operator. 5m at the default 30s poll is a reasonable starting point.

### `unifi.insecureSkipVerify`

Default: `true`

UniFi OS consoles serve a self-signed certificate by default.

### `unifi.ups`

#### `unifi.ups.lowBatteryPercent`

Default: `30`

Battery charge (%) at or below which ups.battery reports "low" / "critical".
Only meaningful when a UniFi UPS is adopted; the ups keys are absent otherwise.

#### `unifi.ups.criticalBatteryPercent`

Default: `10`

#### `unifi.ups.shortRuntimeSeconds`

Default: `600`

Remaining runtime (seconds) at or below which ups.runtime reports "short"
/ "critical". Runtime is a better shutdown trigger than charge because it
already accounts for load: 30% at 300W and 30% at 900W are very different
situations.

These are set against the debounce ups.runtime ships with, not in
isolation: 2 samples is 60s at the default pollInterval, so a critical
threshold of 180s leaves two minutes between Reactor believing the
reading and the UPS running out. Lower it and that headroom goes with it.

#### `unifi.ups.criticalRuntimeSeconds`

Default: `180`

#### `unifi.ups.highLoadPercent`

Default: `80`

Draw as a percentage of the UPS's power budget at or above which ups.load
reports "high".

### `unifi.devices`

#### `unifi.devices.perDeviceKeys`

Default: `false`

Publish a device.&lt;name> key per adopted device, alongside the aggregate
devices key that is always published.

Off by default, and this is the one setting here that changes how MUCH
Reactor publishes rather than what any of it means. Every other state key
is bounded by what is compiled in; a per-device key's name comes from your
network, so turning this on costs one state key, one transition series,
and one more key an Automation can hold state for PER ADOPTED DEVICE.
Forty devices is forty of each. The aggregate answers "is anything down"
on one series whatever the fleet size.

Names are slugified: "US 48" publishes device.us-48. Renaming a device on
the console makes its old key vanish, which Reactor holds state through
rather than treating as a recovery.

### `unifi.temperature`

#### `unifi.temperature.highCelsius`

Default: `75`

Reading (°C) at or above which the hottest adopted device makes
temperature report "high". The console's own overheating flag reports
"high" regardless of this number: the firmware knows what a model
tolerates and a default here does not.

Set against the debounce temperature ships with, not in isolation. UniFi
switches and APs normally sit at 40-60 °C, so 75 plus 3 samples means a
reading that held for 90s at the default pollInterval rather than a fan
spinning up late. Lower this towards the normal operating range and that
hysteresis stops meaning anything.

The per-device readings are in a debug log line — that is where to look
before choosing your own number.

### `unifi.poe`

#### `unifi.poe.maxUtilizationPercent`

Default: `90`

Share (%) of a switch's PoE budget at or above which poe reports
"insufficient" — meaning the headroom is gone, not that a port has
already been denied power. The worst switch in the fleet decides.

Set against the debounce poe ships with. PoE draw is an instantaneous
measurement like ups.load — an AP's radios coming up move it by tens of
watts within one poll — so it settles over 3 samples, and 90% leaves
roughly one powered device's worth of headroom during those 90s. Raise it
towards 100 and there is no headroom left to react in; lower it and a
switch with a full complement of APs reports insufficient forever.

### `unifi.wan`

#### `unifi.wan.quality`

Thresholds for wan.quality, which buckets how well the *live* uplink is
performing into "good" or "degraded". Both numbers are averages the
console keeps over its own uptime window — 24 hours on the hardware these
were captured from — so they describe a link that has been bad, not one
that spiked, and a long outage keeps the key degraded for the rest of
that window.

Only one link's numbers have ever been observed (100% available, 16ms),
so treat these as starting points and tune them against your own uplink.

#### `unifi.wan.quality.minAvailabilityPercent`

Default: `99`

Availability (%) below which the live uplink reports "degraded".
99% over a 24h window is roughly a quarter of an hour of loss.

#### `unifi.wan.quality.maxLatencyMs`

Default: `150`

Average round-trip latency (ms) above which it reports "degraded".

### `unifi.debounce`

#### `unifi.debounce.default`

Default: `1`

How many consecutive observations a *changed* value needs before Reactor
acts on it. Each extra sample costs one pollInterval of reaction time,
so 1 (react on the first observation) is the default: a WAN failover or
a power cut is worth reacting to immediately and neither flaps.

#### `unifi.debounce.keys`

Default:

```yaml
ups.battery: 2
ups.runtime: 2
ups.load: 3
isp: 2
internet: 3
wan.quality: 3
devices: 2
device.*: 2
firmware: 3
temperature: 3
wifi: 2
poe: 3
outlet.*: 1
data.usage: 1
```

Per-key overrides. ups.battery is a threshold crossing, so a charge
hovering at the boundary would otherwise report low/normal/low; it
drains over minutes, so spending one more poll to be sure costs nothing.
isp is a geolocation lookup on the current public address rather than a
link state, so it can report "unknown" for a poll or two while a newly
assigned address is resolved — which is exactly during a failover.
Keys that flap by nature — client presence, once it exists — want more.

internet and wan.quality are measurements rather than switch positions,
and they are the two keys derived from probes to the outside world. A
single poll in which a probe target rate-limits or a resolver blips must
not shed a cluster's load, so both ship at 3 — 90s at the default poll
interval before either an outage or a recovery is believed.

ups.runtime matches ups.battery at 2 — the same escalation, and its
thresholds are set to leave headroom for exactly that delay. ups.load is
the only key derived from an instantaneous measurement that moves second
to second (a server spinning up shifts the draw), so it takes 3.
devices and device.* take 2. A device's state is a switch position like
wan, but it is the console's judgement about a heartbeat rather than a
wire it can see, and one missed beat on a busy console must not page
anyone. 60s at the default poll is nothing against the failure the key
exists for — a device that has been dead for days.

temperature takes 3 because it is a measurement that hovers: thermals
move with the room, the fan and the load, and a reading sitting on the
threshold would otherwise report high/normal/high. Its default threshold
is set against these 3 samples.

poe takes 3, the same argument as ups.load: an instantaneous measurement
that moves when a radio or a camera heater comes up. Its threshold is set
against these 3 samples.

wifi takes 2 for the same reason devices does, and from the same
underlying fact: an AP count is the console's judgement about heartbeats.

firmware takes 3, and nothing is lost by it: the key comes from the
console's lookup against Ubiquiti's release catalogue rather than from
your hardware, and no firmware update needs reacting to within 30
seconds. Extra samples are free when reaction time does not matter.

outlet.* is written down at 1 rather than left to the default, and it is
the one entry here whose number is an argument rather than a setting. A
relay is a switch position and not a measurement: it is where it is,
nothing about it hovers on a threshold, and there is no reading to
settle. It matches wan and ups, which are debounced at 1 for the same
reason. Stating it means raising `default` cannot quietly delay an outlet
by a poll, and it is a prefix because outlet keys are named outlet.&lt;index>
until somebody names the outlets, and outlet.&lt;name> afterwards.

data.usage is written down at 1 for outlet.*'s reason: there is nothing
here to settle. The console has already done the byte accounting and the
threshold comparison against the SIM's real plan before Reactor ever
sees the flags this key is read from, so an extra sample would be
second-guessing a judgement rather than settling a reading. It matches
wan, not ups.battery — the threshold crossing happened on the console's
side, where the hysteresis belongs.

An entry may end in "*", which matches every key with that prefix. It is
how a group whose key names come from your hardware gets settled at all:
no list here could name them. Exact keys win over patterns, and the
longest prefix wins between patterns, so `device.ap-attic: 5` pulls one
device out of the group.

### `unifi.existingSecret`

Default: `unifi-reactor-credentials`

Name of an existing Secret containing the key UNIFI_API_KEY.
Create it with:

```
kubectl -n <namespace> create secret generic unifi-reactor-credentials \
  --from-literal=UNIFI_API_KEY=<your API key>
```

### `unifi.webhook`

Webhook fast path. UniFi's Alarm Manager posts here and Reactor re-observes
state straight away instead of waiting for the next poll.

A delivery only ever triggers a poll — its payload is never read and can
never set state — so polling stays the source of truth. Leaving this off
costs reaction latency and nothing else.

#### `unifi.webhook.enabled`

Default: `false`

#### `unifi.webhook.port`

Default: `9090`

Port the receiver listens on inside the pod.

#### `unifi.webhook.path`

Default: `/webhooks/unifi`

#### `unifi.webhook.existingSecret`

Default: `unifi-reactor-webhook`

Secret holding the shared secret every delivery must present, as
"Authorization: Bearer &lt;token>" or "X-Reactor-Token: &lt;token>". Nothing is
accepted without it. Create it with:

```
kubectl -n <namespace> create secret generic unifi-reactor-webhook \
  --from-literal=UNIFI_WEBHOOK_TOKEN="$(openssl rand -hex 32)"
```

#### `unifi.webhook.tokenKey`

Default: `UNIFI_WEBHOOK_TOKEN`

#### `unifi.webhook.minObserveInterval`

Default: `500ms`

Floor between two observations. A real outage fires several triggers at
once and a retrying console repeats them; this is what keeps a burst of
deliveries — or a flood from whoever finds the endpoint — from becoming a
burst of requests to your gateway.

#### `unifi.webhook.service`

Default:

```yaml
enabled: true
type: ClusterIP
port: 9090
annotations: {}
loadBalancerIP: ""
```

The receiver is NOT reachable from your console by default: this Service
is a ClusterIP, and your UniFi gateway is not in the cluster. Give it a
type your console can reach (LoadBalancer on a LAN-routable address, or
NodePort), or point your own Ingress at it.

#### `unifi.webhook.registration`

Optional: let Reactor create its own Alarm Manager rule on the console,
instead of you creating it in the UniFi UI.

This writes to your gateway on every start, over an API that is
undocumented, reverse-engineered and version-fragile. It fails soft and
never blocks polling, and Reactor only ever creates its rule — it never
edits or deletes one. Off by default for good reason; creating the rule
by hand in the UniFi UI is the conservative option.

#### `unifi.webhook.registration.enabled`

Default: `false`

#### `unifi.webhook.registration.publicURL`

Default: `""`

The URL your console should POST to. Reactor cannot work this out: only
you know how the pod is exposed. Must be reachable from the console and
must not be a loopback address.

#### `unifi.webhook.registration.ruleTitle`

Default: `unifi-reactor`

#### `unifi.webhook.registration.existingSecret`

Default: `unifi-reactor-console`

Secret holding UNIFI_USERNAME and UNIFI_PASSWORD for a UniFi OS local
account. The Alarm Manager API sits at the UniFi OS layer and rejects
the API key the poller uses, so this is a second, separate credential.
unifi.actions below uses the same Secret, for the same reason.

### `unifi.actions`

What Reactor may CHANGE on your console, as opposed to observe.

Everything else the UniFi provider does is read-only, apart from creating
its own Alarm Manager rule. These actions are different in kind: they turn
things off for people who are not running the cluster. So the decision about
what may be touched is yours and is taken here, at install time — not in an
Automation, which anyone who can create one in their own namespace can
write. There is no per-Automation override.

Empty is the default and it means "nothing": every unifi.* action is refused
with a reason in the Automation's status naming the value to add here.

#### `unifi.actions.allowedWlans`

Default: `[]`

SSIDs that unifi.wlan.enable and unifi.wlan.disable may switch, matched
exactly as the console spells them. Anything not listed is refused,
including your main network — which is the point. Reactor has no way to
know which SSID carries its own path to the controller.

```
allowedWlans:
  - Guest
```

Two things to know before you list one. These actions are NOT arbitrated:
two Automations disabling the same SSID do not resolve to one claim, and
whichever enables it first enables it. And nothing hands a WLAN back — if
the exit transition never arrives because the Automation was deleted or
Reactor was uninstalled, the network stays as Reactor last left it until
a human changes it. The README explains why, and it is not an oversight.

#### `unifi.actions.allowedPoePorts`

Default: `[]`

Switch ports that unifi.poe.cycle may power-cycle, as "&lt;switch MAC>/&lt;port
index>". Anything not listed is refused.

```
allowedPoePorts:
  - aa:bb:cc:00:11:22/7
```

Both halves are required, and that is the point of the format. A port
index on its own means something different after somebody re-patches a
rack — slot 7 is still slot 7, and what is plugged into it is not — so an
allowlist written in indices would go on allowing whatever ends up there.
For the same reason the automation must name the port, and Reactor checks
that name against the switch before cutting power.

Two refusals apply whatever you list here: the switch's own uplink port is
never cycled, because it carries everything behind the switch (possibly
including Reactor's path to the console), and neither is a port the switch
does not report as supplying PoE.

This is a power cut to whatever is on the port, so it is worth pairing
with a debounce on whichever state key drives it — see unifi.debounce
above and the README. A flapping key is a stream of transitions, and each
one is a real power cut.

#### `unifi.actions.allowedOutlets`

Default: `[]`

UPS outlets that unifi.outlet.cut and unifi.outlet.restore may switch, as
"&lt;UPS MAC>/&lt;outlet index>/&lt;outlet name>". Anything not listed is refused.

```
allowedOutlets:
  - aa:bb:cc:00:11:22/5/nas
```

This is the largest blast radius in Reactor and the place it can help you
least. A switch tells Reactor which of its ports is the uplink, so a PoE
cycle can refuse that one absolutely. A UPS tells Reactor nothing at all
about what is plugged into an outlet — not the device, not whether it is
your gateway. The only thing standing between an automation and the wrong
socket is this list, and the name in it.

All three parts are required, which is one more than allowedPoePorts asks
for, and the extra one is deliberate. Two of them are a position; only the
name is a thing. "aa:bb:cc:00:11:22/5" would mean you agreed to whatever
is in outlet 5, and after somebody re-plugs the rack that is something
else. Reactor checks all three against the UPS immediately before writing,
so a re-plug becomes a refused action with a sentence.

Out of the box these outlets are called "Outlet 1" … "Outlet 8", which is
the index spelled out rather than a name. An outlet still carrying that
placeholder is REFUSED — here, in the automation, and against the console.
Name the outlet in UniFi after what is plugged into it first. That is not
bureaucracy: it is the only moment anybody writes down what this socket
feeds, and everything downstream depends on it. (An outlet name containing
a comma cannot be listed, the same limitation an SSID has.)

Two limitations, and on mains power they are louder than anywhere else.
These actions are NOT arbitrated: two Automations cutting the same outlet
do not resolve to one claim, and whichever restores it first restores it.
And nothing hands an outlet back — if the exit transition never arrives
because the Automation was deleted or Reactor was uninstalled, the outlet
stays open until a human closes it. There is no baseline and no pre-delete
sweep that can reach a relay.

One thing nobody has proved: that writing the relay actually opens it. The
write was accepted on real hardware and the console reported the new
position back, but the outlet under test was empty. Plug a lamp into an
outlet, drive a transition, and watch it go dark before you trust this
with anything that matters.

#### `unifi.actions.allowBatteryBackedOutlets`

Default: `false`

Whether the outlets above may include battery-backed ones. Off by default,
and while it is off a battery-backed outlet is refused whatever you list.

This UPS splits its outlets into a battery-backed bank and a surge-only
bank, and Reactor reads which is which from the UPS itself. Cutting a
battery-backed outlet during a power cut is the most damaging thing here
and the least likely to be what somebody meant.

It is also, unavoidably, the only kind of cut that extends runtime: a
surge-only outlet is already dark when the mains are, so shedding it saves
nothing. So this is a consent rather than a prohibition — if load-shedding
on battery is why you are here, you need it, and turning it on is you
saying you know which outlets are on that bank.

On its own it allows nothing. It only qualifies allowedOutlets.

#### `unifi.actions.existingSecret`

Default: `unifi-reactor-console`

Secret holding UNIFI_USERNAME and UNIFI_PASSWORD. Writing to the console
needs a UniFi OS local account: the API key the poller reads state with
does not write. This defaults to the same Secret the Alarm Manager
registration uses, because it is the same credential.

## `actions`

### `actions.allowedDestinations`

Default: `[]`

Where Reactor may send the outbound edge actions — http.request, the
notification.* types, and the named integrations (homeassistant.service,
qbittorrent.pause and qbittorrent.resume). They all go through one client,
so this one list governs every one of them, and every leg of an action that
takes more than one request is checked against it separately.

Empty is the default and it means "nowhere": every outbound action is
refused with a reason in the Automation's status naming the destination to
add here. That is deliberate. An Automation is writable by anyone who can
create one in their own namespace, and the request it asks for goes out from
inside the cluster with this operator's network position — reaching
ClusterIP Services, your gateway, and anything else this pod can route to.
Deciding which destinations that is worth is yours, not theirs.

Entries are scheme, host and optional port. No port means the scheme's
default port only, so an unusual port has to be written out. One leading
"*." label is allowed. The single entry "*" allows any host and is an
explicit choice to run without this control.

```
allowedDestinations:
  - https://ntfy.example.com
  - https://discord.com
  - http://hooks.example.com:8080
  - https://*.example.com
```

Two things this never permits, whatever is listed: the loopback interface,
and link-local addresses (169.254.0.0/16, fe80::/10) — where cloud instance
metadata services and the credentials they hand out live. Redirects are
never followed, for the same reason.

Setting this also grants the operator "get" on Secrets, because action
credentials come from Secrets in the Automation's own namespace. Leave it
empty and that permission is not granted at all.

## `log`

### `log.level`

Default: `info`

Verbosity: "debug", "info", "error", or a V-level number ("1", "2").
debug turns on the per-observation lines you want while working out why a
trigger did or did not fire.

### `log.format`

Default: `console`

"console" for human-readable logs, "json" for a log collector.

## `metrics`

What Reactor observed, what matched, what it did, and how fast — on the
metrics endpoint controller-runtime already serves. Reactor deliberately does
not re-export UniFi telemetry; a UniFi exporter covers that better.

Off by default, because enabling it opens a port on the pod and creates a
Service, and an upgrade must never do either on its own. The manifest bundle
(install.yaml) turns it on; everything below renders the same shape in both
paths, so the only difference between them is this default.

### `metrics.enabled`

Default: `false`

### `metrics.secure`

Default: `true`

Serve over HTTPS behind the API server's authn/authz filter, so a scraper
must present a bearer token whose ServiceAccount is allowed to GET /metrics.
Set false to serve plain HTTP on the same port instead — only sensible when
something else already restricts who can reach the pod.

### `metrics.port`

Default: `8443`

### `metrics.service`

Default:

```yaml
enabled: true
port: 8443
annotations: {}
```

### `metrics.reader`

Default:

```yaml
create: true
```

A ClusterRole granting GET on /metrics, created with no binding: nothing
gains access until you bind it to your Prometheus ServiceAccount.

### `metrics.serviceMonitor`

Requires the Prometheus Operator's ServiceMonitor CRD.

#### `metrics.serviceMonitor.enabled`

Default: `false`

#### `metrics.serviceMonitor.interval`

Default: `""`

Leave empty to inherit the Prometheus instance's defaults.

#### `metrics.serviceMonitor.scrapeTimeout`

Default: `""`

#### `metrics.serviceMonitor.labels`

Default: `{}`

Extra labels, for a Prometheus whose serviceMonitorSelector is not empty.
Optional on purpose: a selector of {} scrapes every ServiceMonitor and
needs none of these.

#### `metrics.serviceMonitor.annotations`

Default: `{}`

#### `metrics.serviceMonitor.insecureSkipVerify`

Default: `true`

The controller generates a self-signed certificate for the metrics server
unless --metrics-cert-path points at a real one, so verification is off by
default. Issue a certificate and set serverName to turn it on.

#### `metrics.serviceMonitor.serverName`

Default: `""`

### `metrics.rules`

Alert rules, as a PrometheusRule. Requires the Prometheus Operator CRD.

#### `metrics.rules.enabled`

Default: `false`

#### `metrics.rules.labels`

Default: `{}`

#### `metrics.rules.observationStaleSeconds`

Default: `90`

How long without a successful observation counts as blind. Three times
unifi.pollInterval, in seconds — raise this if you raise that.

#### `metrics.rules.reactionLatencySeconds`

Default: `60`

p95 observation-to-action latency, in seconds, above which reacting is
taking longer than it should. Comfortably above one poll interval.

#### `metrics.rules.informational`

Default: `true`

Informational rules that fire on observed state rather than on a fault —
the UPS running on battery, the WAN on its backup uplink. They name UniFi
state keys, so they are only rendered when unifi.url is set.

### `metrics.dashboard`

The overview dashboard, as a grafana-operator GrafanaDashboard. The same
JSON is in the chart at dashboards/reactor.json if you import by hand.

#### `metrics.dashboard.enabled`

Default: `false`

#### `metrics.dashboard.instanceSelector`

Default:

```yaml
matchLabels:
    dashboards: grafana
```

Which Grafana instances grafana-operator should install it into.

#### `metrics.dashboard.folder`

Default: `""`

Grafana folder to file it under. Empty means the instance's default.

#### `metrics.dashboard.allowCrossNamespaceImport`

Default: `false`

#### `metrics.dashboard.resyncPeriod`

Default: `10m`

## `uninstall`

### `uninstall.releaseClaims`

Default: `true`

Before the operator is removed, hand every workload it is holding back to
what your Automations want once nothing claims them. Helm does not delete
the Automation CRD or your Automation resources on uninstall, so without
this nothing would ever release them and anything scaled to 0 stays there.
Disabling this leaves workloads exactly as they are; each one still records
its pre-Reactor value in a reactor.robbeverhelst.com/baseline-replicas
annotation.

### `uninstall.timeoutSeconds`

Default: `120`

Hard bound on the release Job, so a release that cannot finish delays the
uninstall rather than blocking it. Use `helm uninstall --no-hooks` to skip
the hook entirely.

## `safety`

### `safety.dryRun`

Default: `false`

Run the whole install as a dry run: every automation is evaluated and
arbitrated exactly as it otherwise would be, and nothing is written. Each
automation reports what its targets would be held at in
status.targets[].effective, says Applied=False with reason DryRun, and sends
no notification, no http.request and no restart.

This is the mode to roll a new cluster out in. Turning it on ALSO withholds
every permission that could write to a target — patch on the workload kinds
and update on their scale subresources — so that "it will not touch your
workloads" is enforced by the API server rather than promised by a flag.

Two things worth knowing before turning it on:

- It is install-wide and is NOT the same thing as an automation's own
  spec.dryRun. That one takes a single automation out of force so it
  cannot perturb the live ones beside it; this one stops the whole
  operator writing. Use spec.dryRun to try one policy on a working
  install, and this to bring up an install that has never acted.
- Turning it on for an install that is already holding workloads down
  freezes them there: releasing a claim is a write too. Suspend or delete
  those automations first, or uninstall with the pre-delete hook, which
  hands every target back.

### `safety.detectHPA`

Default: `false`

Notice a HorizontalPodAutoscaler driving a target and decline to fight it.

Reactor writes spec.replicas; an HPA computes one from metrics and writes it
back. Neither is wrong and neither wins — the workload just oscillates,
every reconcile against every HPA sync, until one of them is removed.
Arbitration cannot help, because it resolves claims between Automations and
an HPA is a claimant Reactor cannot see.

With this on, Reactor lists the HPAs in a target's namespace before claiming
it. If one points at the target it writes nothing, reports Applied=False
with reason TargetManagedByHPA naming the HPA, raises a Warning Event and
counts reactor_arbitrations_total{outcome="declined"}. The Automation stays
Ready: it is correctly configured, it just cannot act there. A target
Reactor was ALREADY holding when an HPA appeared is handed back to its
recorded baseline first — an HPA will not scale a workload up from zero, so
simply going quiet would strand it.

It grants list on autoscaling/horizontalpodautoscalers, and nothing else.
That is a read of an autoscaling policy: no write to one, and no permission
over anything the HPA manages. Reactor declines to act and deliberately does
not suspend the HPA on your behalf.

Off by default because it changes what an install already in that fight
does, and because it costs a permission nothing else here needs. Turn it on:
losing a fight silently is worse than plainly declining to start one, and
there is no automation this makes worse. If the permission is missing while
this is on, a claim FAILS rather than proceeding blind, and says so.

## `rbac`

### `rbac.clusterWide`

Default: `true`

cluster-wide: the operator may act on any namespace, enabling the optional

```
Automation spec.actions[].target.namespace field.
```
When false, RBAC is namespace-scoped to the release namespace: Automations

```
can only target resources in that same namespace.
```

### `rbac.allowNodeActions`

Default: `false`

Let Automations use kubernetes.cordon, which closes a node to new
scheduling while a condition holds — a worker running on a UPS whose
battery is draining, say, so that replacement pods land on the node still
on mains.

Off by default, and it is the one permission here that reaches outside the
workloads you installed Reactor to manage. Nodes are cluster-scoped, so
enabling this creates a ClusterRole even when rbac.clusterWide is false —
a namespaced Role cannot grant node access at all. Read that as a real
widening of the operator's reach, not a formality.

What it grants: get and patch on nodes. Kubernetes has no way to narrow
patch to one field, so this also permits writing node labels and
annotations. What it does NOT grant, at all and under any setting: pods or
pods/eviction. Reactor cannot drain a node — that action is not implemented
on purpose, because an eviction cannot be reversed and so has no level to
arbitrate and no reversal to declare. See the README.

Leaving this false does not break an Automation that uses kubernetes.cordon;
it reports the node as unreachable and names this value.

## `serviceAccount`

Default:

```yaml
create: true
name: ""
annotations: {}
```

## `replicaCount`

Default: `1`

## `resources`

Default:

```yaml
requests:
    cpu: 10m
    memory: 64Mi
limits:
    cpu: 500m
    memory: 128Mi
```

## `podDisruptionBudget`

### `podDisruptionBudget.enabled`

Default: `false`

Off by default. With a single replica a budget cannot protect anything:
minAvailable: 1 turns a node drain into a hang instead. Enable it alongside
replicaCount: 2 — leader election keeps exactly one instance acting, so the
second is a warm standby that makes drains safe.

### `podDisruptionBudget.minAvailable`

Default: `1`

Set one of the two; leave the other empty.

### `podDisruptionBudget.maxUnavailable`

Default: `""`

## `networkPolicy`

### `networkPolicy.enabled`

Default: `false`

Off by default so enabling the chart never changes what the pod can reach.
When enabled, nothing may reach the operator and egress is whatever the
rules below allow — narrow them to your API server, DNS, and console.

### `networkPolicy.ingress`

Default: `[]`

Nothing needs to reach the operator by default: it serves only kubelet
health probes, which most CNIs deliver from the node outside policy
enforcement.

The exception is unifi.webhook.enabled, where the console does need to
reach it. This list is NOT widened for you when the webhook is on — a
policy that opens a port you did not write here would be a bad surprise —
so add the rule yourself, or deliveries are dropped and Reactor quietly
falls back to the poll interval:

```
ingress:
  - from:
      - ipBlock:
          cidr: 192.0.2.1/32     # your console
    ports:
      - protocol: TCP
        port: 9090
```

### `networkPolicy.egress`

Default:

```yaml
- to:
    - ipBlock:
        cidr: 0.0.0.0/0
```

Unrestricted by default, so enabling the policy cannot silently break
polling or the operator's connection to the API server. See the chart
README for a narrowed example.

## `nodeSelector`

Default: `{}`

## `tolerations`

Default: `[]`

## `affinity`

Default: `{}`

## `podAnnotations`

Default: `{}`

## `annotations`

Default: `{}`

Annotations on the Deployment itself. Rotation of the UniFi API key needs no
restart (the key is re-read from the mounted Secret), but if you would rather
restart on change and already run reloader:

```
annotations:
  secret.reloader.stakater.com/reload: unifi-reactor-credentials
```

## `imagePullSecrets`

Default: `[]`
