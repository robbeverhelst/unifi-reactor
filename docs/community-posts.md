# Community posts — drafts, unposted

**Nothing in this file has been posted anywhere.** These are drafts for the maintainer to send, edit, or
discard. They are checked in rather than kept in a scratchpad so that the claims in them can be reviewed
against the code the same way any other documentation is, and so that a claim that stops being true shows
up in a diff.

Every factual statement below is meant to match what the repository actually ships. Two things in
particular are deliberately stated as limits rather than left out, because they are the first things a
reader who knows this hardware will ask about:

- a genuine WAN failover has never been observed against real hardware ([#34]), so `wan` is inferred from
  one capture in which only one uplink was live;
- three state keys — `firmware`, `temperature`, `poe` — are parsed against UniFi's documented field shape
  rather than a captured response, because no capture in the repository contains those fields.

If a draft is edited before sending, keep those two caveats in it. They are the reason the project reads
as honest rather than as marketing, and dropping them to make a post shorter is the one edit that would
make the post wrong.

## Before sending anything

- [ ] Read the [Rules of the road](#rules-of-the-road) at the bottom. Several of these communities have
      self-promotion rules that a first-time poster will otherwise trip.
- [ ] Check the current release tag and update any version mentioned.
- [ ] Do not describe your own network. Every example here uses documentation-safe values on purpose.
- [ ] Post to one place at a time and answer replies there before moving to the next.

---

## r/Ubiquiti

**Framing:** this audience owns the gear. What they want to know is what it talks to, what it can see, and
whether it does anything unsupported to their console. The Kubernetes half is the payload, not the hook.

**Title:** `Kubernetes operator that reads UniFi state — WAN uplink, UPS, PoE headroom, firmware — as a list of keys you can act on`

**Body:**

> I have been building an operator that polls the UniFi Network API and turns what the console already
> knows into a small vocabulary of state keys. It reads with an API key from Settings → Control Plane →
> Integrations, and everything on the read path is a normal `GET`.
>
> What it publishes today, one key per line, only when the matching hardware is adopted:
>
> - `wan` — `primary` / `backup`, which uplink the gateway is using
> - `wan.quality` — `good` / `degraded`, against configurable thresholds
> - `isp` — the carrier your console geolocated your public address to, as a slug
> - `internet` — `ok` / `degraded` / `down`, from the console's own `www` health subsystem
> - `ups`, `ups.battery`, `ups.runtime`, `ups.load` — a UniFi UPS on mains or on battery, plus charge,
>   remaining runtime and draw
> - `devices` and, opt-in, `device.<name>` — adopted devices reachable or not
> - `firmware` — whether anything adopted has an update waiting
> - `temperature` — the hottest adopted device, bucketed
> - `wifi` — the WiFi subsystem from the console's own AP counts
> - `poe` — PoE headroom on the worst switch
> - `outlet.<n>` — a switchable UPS outlet, read-only
>
> The first observation logs every key it can see, so those log lines are an inventory of what your
> console is willing to tell you. That part has been useful to me independently of the automation.
>
> Two honest caveats, because this sub will spot both.
>
> **A real WAN failover has never been observed.** `wan` is derived from which port reports `is_uplink`,
> inferred from a single capture in which only one uplink was live. Whether `is_uplink` follows the live
> traffic or just marks the port configured as primary is unconfirmed. It is not guessing silently — the
> gateway's own uplink interface is used as a second opinion, `isp` is compared against `wan` on every
> observation, and a disagreement is logged rather than resolved — but it is not the same as knowing. If
> you have a gateway with two working uplinks, there is a fifteen-minute capture runbook in the repo and
> one capture would close it.
>
> **`firmware`, `temperature` and `poe` are written to the shape UniFi's API documents, not to a capture.**
> I own a UDM Pro and a UniFi UPS 2U and neither reports those fields, so no committed capture contains
> them. Each fails by publishing nothing rather than by publishing a reassuring value: no thermals means
> no `temperature` key, not `temperature: normal`. One `stat/device` capture from a switch or an AP would
> settle all three.
>
> Verified against UniFi Network 10.5.67 on a UDM Pro with a UniFi UPS 2U. Other UniFi OS consoles are
> expected to work — it warns and carries on rather than refusing to start.
>
> There is a write path too — enable or disable a WLAN, cycle a PoE port — and I want to be blunt about
> it: those endpoints are inferred from an undocumented API and only the authentication has been seen
> working against real hardware. They are off by default, they need an explicit allowlist of exactly which
> SSIDs and which named ports may be touched, a switch's uplink port is never cycled whatever you
> allowlist, and every step reads the object and confirms it is the one you meant before it writes. A bug
> in the inference degrades to a refused action rather than a wrong one. That is the property the
> check-before-write discipline exists to give, and it is why I would rather describe it this way than
> ship it quietly.
>
> Apache 2.0, one static binary, no database and no UI. Repo: https://github.com/robbeverhelst/unifi-reactor

---

## r/homelab

**Framing:** the power-cut story. This audience runs the cluster and the UPS in the same rack, and has
personally watched a battery drain into a transcode.

**Title:** `The UPS knows it is on battery; the cluster does not. I wrote an operator that closes that gap.`

**Body:**

> The failure that started this: the power drops, the UPS starts counting down its runtime, and the
> cluster spends that runtime transcoding video and running the nightly backup. Nothing in Kubernetes
> knows anything happened. The UPS knows. The two just never talk.
>
> UniFi Reactor is a Kubernetes operator that polls the UniFi Network API and reconciles your cluster
> against what it observes. A UniFi UPS reports `ups: online` or `ups: on-battery`, plus remaining charge,
> remaining runtime and current load, and you write what should happen:
>
> ```yaml
> apiVersion: reactor.robbeverhelst.com/v1alpha1
> kind: Automation
> metadata:
>   name: shed-load-on-battery
>   namespace: media
> spec:
>   when:
>     provider: unifi
>     state:
>       ups: on-battery
>
>   actions:
>     - type: kubernetes.scale
>       target: { kind: Deployment, name: qbittorrent }
>       replicas: 0
>     - type: kubernetes.cronjob.suspend
>       target: { kind: CronJob, name: velero-backup, namespace: velero }
> ```
>
> Omit `onExit` and it puts things back the way it found them — the value each target held before Reactor
> first claimed it is recorded in an annotation on the target itself, so it survives a controller restart
> and you can read it with `kubectl get deploy -o jsonpath='{.metadata.annotations}'` at 3am.
>
> Some things I decided on purpose, because they are the parts that matter when it fires for real:
>
> **Runtime is a better trigger than charge.** "30% battery" means nothing without knowing the load —
> 30% carrying a light rack is twenty minutes and 30% carrying a heavy one is four. The UPS already
> computes remaining runtime against its actual current draw, so `ups.runtime` is `ample` / `short` /
> `critical` and that is the key I reach for.
>
> **Suspending a CronJob does not kill a Job already running.** Deliberate, and Reactor is granted no
> permission over Jobs at all, so it could not delete one if it tried. Declining to start more work is a
> very different act from killing work in flight, and killing work in flight is not a decision an outage
> should make for you.
>
> **There is no `kubernetes.drain`, and there will not be.** In a three-node homelab on one UPS, draining
> assumes somewhere else for the pods to go, and there is nowhere — they go `Pending`, so you lose the
> workload *before* the battery runs out instead of when it does. `kubernetes.cordon` gets the actual
> benefit, which is that replacements land on the node still on mains. Cordoning is opt-in and is the only
> permission it asks for that reaches outside the workloads you installed it to manage; nodes are
> cluster-scoped, so turning it on creates a `ClusterRole` even in a namespaced install.
>
> It does the metered-uplink case with the same shape — `wan: backup` instead of `ups: on-battery` — and
> point both at the same Deployment and it stays down until neither wants it down.
>
> Caveat worth stating up front for this crowd: the UPS path is the well-tested one, verified against a
> UniFi UPS 2U on a UDM Pro. A genuine WAN failover has never been observed on real hardware, so treat
> `wan` as less battle-tested than `ups`.
>
> One static binary in a distroless image, no database, no queue, no UI. Helm chart and image are both
> signed. Apache 2.0. https://github.com/robbeverhelst/unifi-reactor

---

## r/selfhosted

**Framing:** same power-cut story, but this audience cares more about what it costs to run and what it
can reach than about rack topology. Lead with the operational footprint.

**Title:** `Operator that pauses self-hosted workloads when the UPS goes to battery or the WAN fails over`

**Body:**

> Two failures that cost me something before I automated them: the WAN failing over to the 5G backup at
> 3am while qBittorrent kept seeding into a data cap, and the power dropping while the cluster spent the
> UPS's runtime on a transcode.
>
> UniFi Reactor polls the UniFi Network API and turns what your console observes — which uplink is live,
> which carrier is behind it, whether the UPS is on mains — into declarative actions on Kubernetes. One
> `Automation` resource says what to do while a condition holds and what it wants once it stops:
>
> ```yaml
> apiVersion: reactor.robbeverhelst.com/v1alpha1
> kind: Automation
> metadata:
>   name: pause-downloads-on-backup-wan
>   namespace: media
> spec:
>   when:
>     provider: unifi
>     state:
>       wan: backup
>
>   actions:
>     - type: kubernetes.scale
>       target: { kind: Deployment, name: qbittorrent }
>       replicas: 0
>
>   onExit:
>     - type: kubernetes.scale
>       target: { kind: Deployment, name: qbittorrent }
>       replicas: 1
> ```
>
> What it can do: scale Deployments and StatefulSets, suspend CronJobs, cordon nodes, roll a Deployment,
> send a notification (ntfy, Discord, Slack), call an arbitrary HTTP API, call a Home Assistant service,
> pause and resume qBittorrent, and switch a UniFi WLAN or cycle a PoE port.
>
> The part I would want to read about first if someone else had written this is the outbound story,
> because an operator that will make HTTP requests on demand is a confused deputy: it runs inside the
> cluster with a network position, and `spec.actions` is writable by anyone who can create an `Automation`
> in their own namespace. So destinations are an install-time allowlist, empty by default, with no
> per-`Automation` override and no way to widen it from a namespace. Redirects are never followed, because
> a redirect names a destination the allowlist never approved. Loopback and link-local — where cloud
> metadata services live — are refused in the dialer, on the address actually connected to, so a hostname
> that resolves somewhere other than it appears to is refused too. Private ranges are *not* blocked,
> because an ntfy box on your LAN is a first-class destination for this and no address rule can tell that
> apart from a `ClusterIP` Service; the allowlist is what draws that line, which is why it is default-deny.
> Credentials come from Secrets in the `Automation`'s own namespace only, and the Secret read permission
> is granted only if you actually enabled outbound actions.
>
> Footprint: one static Go binary in a distroless image, multi-arch, no database, no queue, no UI. It
> needs a UniFi API key with read access and RBAC over the workloads you point it at — not `cluster-admin`.
> Image and Helm chart are both cosign-signed keylessly from the release workflow, and the image carries an
> SBOM and build provenance.
>
> Honest limits: verified against UniFi Network 10.5.67 on a UDM Pro and a UniFi UPS 2U. A real WAN
> failover has never been observed, so `wan` is inferred rather than confirmed. Three keys — `firmware`,
> `temperature`, `poe` — are parsed against UniFi's documented field shape rather than a capture, and each
> publishes nothing rather than a reassuring default when the fields are absent. It is pre-1.0 and
> `v1alpha1`, so expect breaking changes between minor versions.
>
> Apache 2.0. https://github.com/robbeverhelst/unifi-reactor

---

## Ubiquiti Community forum

**Framing:** more formal than Reddit, and the audience skews toward people who will actually go and look
at what requests you make of their console. Lead with the integration surface and be explicit about read
versus write.

**Category:** UniFi Network → Feature/Integration discussion (check the current category list before posting)

**Title:** `Open-source Kubernetes operator built on the Network API — state keys from the console, and what I could not verify`

**Body:**

> I have written an open-source operator that consumes the UniFi Network API and reacts to what the
> console reports. Posting it here partly to share it and partly because there are three things I cannot
> verify from the hardware I own, and this is where the people who can are.
>
> **What it reads.** It authenticates with an API key created under Settings → Control Plane →
> Integrations and polls `stat/device` and `stat/health` on an interval (30s by default). It normalizes
> those into a small set of keys — which uplink is live, the carrier behind it, whether the outside world
> is reachable, UPS mains/battery plus charge, runtime and load, adopted devices reachable or not, pending
> firmware, the hottest device, the WiFi subsystem, PoE headroom, and the state of each switchable UPS
> outlet. Keys degrade one at a time: a console with no UPS still reports uplink state, and a console that
> answers one endpoint but not the other publishes whatever it can.
>
> It logs the Network version it finds and compares it against what the project was verified against, then
> carries on. Refusing to start against a console that would have worked fine seemed like a worse failure
> than a warning.
>
> **What it writes, and why that is separate.** There are three write actions: enable a WLAN, disable a
> WLAN, and cycle a PoE port. These go to the one console configured at install time, over the
> undocumented internal API, with credentials that are install configuration and cannot be supplied by a
> user of the operator. They are off by default and controlled by two allowlists that name exactly which
> SSIDs and which ports may be touched — there is deliberately no wildcard, because "any SSID" and "any
> port" are not choices worth offering. A PoE entry must name the switch *and* what the port is called,
> and that name is checked against the switch's own port table before anything is sent, so re-patching a
> rack turns into a refused action rather than a power cut to the wrong device. A switch's own uplink port
> is never cycled whatever you allowlist. Each action logs in, acts, and logs out; no session is cached.
>
> **What I could not verify, stated plainly.** Every endpoint on that write path is inferred rather than
> observed — only the authentication has been seen working against real hardware. The repository splits
> the two in a document rather than blurring them. Separately, `firmware`, `temperature` and `poe` are
> parsed against the field names UniFi's API documents, because nothing I own reports them: the UDM Pro
> capture carries no upgrade fields and the UPS 2U reports no thermals at all. And a genuine WAN failover
> has never been observed, so the derivation of `wan` from `is_uplink` is inferred from a capture with one
> live uplink.
>
> In every one of those cases the code fails by declining or by publishing nothing, never by publishing a
> confident wrong answer.
>
> If you run a UniFi switch or an AP, or a gateway with two working uplinks, there is a capture script in
> the repository that produces a sanitized JSON fixture. One of each would settle all four open questions
> above. Verified so far: UniFi Network 10.5.67, UDM Pro on gateway firmware 5.1.26, UniFi UPS 2U
> (`USWDA26`, firmware 1.6.1).
>
> Apache 2.0, no telemetry, nothing phones home: https://github.com/robbeverhelst/unifi-reactor

---

## Show HN

**Framing:** HN wants the design decision and the reasoning behind it, not the feature list. The two ideas
worth the post are *state, not events* and *arbitration across automations sharing a target*. Everything
else is context for those.

**Title:** `Show HN: UniFi Reactor – a Kubernetes operator that reconciles against state, not events`

**URL:** `https://github.com/robbeverhelst/unifi-reactor`

**First comment (post immediately after submitting):**

> The problem is small and specific: my UniFi gateway knows when it has failed over to the 5G backup and
> my UniFi UPS knows when it is on battery, and my Kubernetes cluster knows neither, so it keeps seeding
> torrents into a metered uplink and transcoding video on battery runtime.
>
> The obvious build is webhooks: the console fires an event, something in the cluster reacts. I built the
> other thing, and the two design decisions behind that are what I would actually like feedback on.
>
> **1. State, not events.** It polls and reconciles against what it observes. There is no event log and no
> queue. A dropped webhook, a network blip, or a controller restart cannot strand the cluster in the wrong
> mode, because the next observation corrects it. Webhooks exist in the project as a latency optimization
> and are never the mechanism of record — they cause an immediate poll rather than carrying information.
>
> The consequence that makes it worth doing: observing `wan: backup` fifty times does nothing forty-nine
> extra times. Scaling is expressed as a desired level, not as a command, so the result is a pure function
> of which conditions currently hold and is independent of the order they were observed in. Retrying is
> free. Crash recovery is not a code path — it is the same code path.
>
> **2. Arbitration, not last-write-wins.** qBittorrent genuinely should pause for both a metered uplink
> and a power cut, and those are two separate automations written by someone who was not thinking about
> the other one. The naive implementation has whichever automation saw a transition most recently write
> the replica count, which means the WAN recovering brings the workload back up while the building is
> still on battery.
>
> Instead, every automation pointing at a target declares a level, and the target is continuously
> reconciled to the most restrictive level anyone currently asks for. The workload comes back only when
> *no* automation wants it down. The automation that lost says so in its status, naming the one that
> outvoted it:
>
> ```
> {"ref":"Deployment/media/qbittorrent","desired":1,"effective":0,
>  "deferredBy":["media/shed-on-battery"]}
> ```
>
> "Most restrictive" is a total order and nothing more: for replicas it is the lower count, and for
> CronJob suspension and node cordoning it is the suspended and cordoned end. So suspended wins over
> running for exactly the same reason 0 replicas wins over 3, and there is no second rule to learn.
>
> **The line that fell out of this, which I did not design and only noticed afterward.** Some actions can
> be arbitrated and some cannot, and the discriminator is not whether the action expresses a level. Pausing
> a torrent plainly expresses a level; so does a WLAN being enabled. Both are edge actions anyway. What
> decides it is whether there is somewhere to record the value the target held *before* the operator
> claimed it — because without that, release cannot put it back, and an automation that cannot hand a
> target back has no business claiming it. For a Kubernetes object that place is an annotation on the
> object. For anything outside the cluster there is no answer, which is why every arbitrated action so far
> is a `kubernetes.*` one and everything that leaves the cluster is named as a verb and owns nothing.
>
> **What I deliberately did not build.** There is no `drain` action. Every other action declares a level
> that is a pure function of current conditions, and a drain has no such value — there is no state a node
> can be held at that means "drained", so reversal cannot express undoing it and a flapping input would
> empty the node again on every flap with nothing to correct it. The RBAC that would make it possible is
> not granted under any setting. Cordoning gets the real benefit in a small cluster, which is that
> replacement pods land on the node still on mains.
>
> **What is not verified, since it is the first thing I would want to know.** A genuine WAN failover has
> never been observed on real hardware — the uplink derivation is inferred from a single capture in which
> only one uplink was live. It is not silent about that: the gateway's own uplink interface is used as a
> second opinion, the observed carrier is compared against the uplink on every observation, and a
> disagreement is logged rather than resolved. Three state keys are parsed against the field shape
> UniFi's API documents rather than against a captured response, because the hardware I own does not
> report those fields, and each publishes nothing rather than a reassuring default when they are absent.
> Parsers here are written against real captured responses committed to the repository, and the three
> exceptions are listed as exceptions.
>
> Pre-1.0, `v1alpha1`, Apache 2.0. Go, one static binary, no database.

---

## Rules of the road

Checked before drafting; re-check before sending, since community rules change.

| Venue | The rule that matters | What it means here |
| --- | --- | --- |
| r/Ubiquiti | Self-promotion is tolerated for genuinely relevant open-source work, but low-effort link drops are removed. | Post the body above in full as a text post. Do not post a bare link. |
| r/homelab | Rule 3 forbids advertising and low-effort self-promotion; projects are welcome when the post explains the thing rather than selling it. | The body is the post. No link-only submission, no crossposting the same text to r/selfhosted the same day. |
| r/selfhosted | Self-promotion is allowed for free and open-source software, and the software must actually be self-hostable. | Both hold. Flair the post as a release/project if the sub uses flair. |
| Ubiquiti Community | Registered account required, and posts belong in a product-specific category. | Check the current UniFi Network category list; do not cross-post to several. |
| Hacker News | Show HN is for something people can try. No "we/our" marketing voice, and the submitter should be in the thread to answer. | Submit the repo URL, then post the first comment above immediately. Be around for a few hours. |

One more, which is not a written rule anywhere but is the thing most likely to go wrong: **do not describe
your own network.** No SSIDs, no addresses, no device names, no cluster context. Every example in this
file uses documentation-safe values, and a reply written quickly in a thread is where that slips.

[#34]: https://github.com/robbeverhelst/unifi-reactor/issues/34
