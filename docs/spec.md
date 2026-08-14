# UniFi Reactor — design specification

> This is the original design document the project was built from, kept as the
> record of *why* the architecture looks the way it does. For installation and
> usage, start at the [README](../README.md). Where the two disagree, the README
> describes what actually ships: notably the API group is
> `reactor.robbeverhelst.com`, and UPS state arrived as part of the UniFi
> provider rather than as a separate NUT provider, because UniFi consoles
> already report it.

**Event-driven automation for UniFi networks and Kubernetes.**

UniFi Reactor is an open-source Kubernetes Operator that allows UniFi events to trigger configurable actions in Kubernetes, external APIs, and other infrastructure.

The initial focus is UniFi-first: make it extremely easy for a homelab operator to react to events from a UniFi environment.

The architecture should be designed so additional event providers and action providers can be added later without rewriting the core automation engine.

## The problem

UniFi can detect important infrastructure events:

- WAN outages
- WAN failover
- WAN restoration
- UniFi device failures
- clients connecting/disconnecting
- network events
- potentially UniFi Protect events in the future

But detecting an event is only half the problem.

A homelab often needs to react to those events.

For example:

```text
Telenet WAN fails
        ↓
UDM switches to U5G backup
        ↓
UniFi emits failover event
        ↓
Reactor receives event
        ↓
qBittorrent pauses
large downloads stop
backup jobs stop
bandwidth-heavy workloads scale down
        ↓
Telenet returns
        ↓
Reactor receives recovery event
        ↓
normal workloads resume
```

Today this can be assembled using combinations of UniFi webhooks, Argo Events, n8n, Home Assistant, custom scripts, Kubernetes APIs, etc.

Reactor should make common infrastructure automations much simpler.

## Design risks and open questions

**Read this before implementing anything.**

### Risk 1: the webhook assumption

The entire UniFi provider design depends on what UniFi's Alarm Manager / webhook mechanism actually emits. UniFi webhooks are known to be limited in event coverage and payload detail, and it is not guaranteed that a clean, machine-usable WAN failover webhook exists at all.

Before writing any operator code, perform this experiment:

1. Configure a webhook receiver (a simple request-logging endpoint is enough).
2. Point UniFi's webhook/Alarm Manager configuration at it.
3. Physically trigger a real WAN failover (pull the primary uplink) and a recovery.
4. Capture exactly what UniFi sends, when, and with what payload.
5. Store the captured payloads in `testdata/` as the ground truth for the parser.

If UniFi does not emit a usable failover webhook, the provider design shifts to polling the UniFi Network API for WAN state (see the state-first design below), and webhooks become an optional fast path rather than the foundation.

### Risk 2: events are fragile, state is not

A missed webhook delivery (controller restart, network blip, UniFi not retrying) must not leave the cluster stuck in the wrong mode. This is why the design below treats observed UniFi state as the source of truth and webhooks as an optimization, not as the mechanism of record.

## Goals

### Primary goals

- Provide excellent UniFi integration.
- Make UniFi events usable inside Kubernetes.
- Provide declarative automation through Kubernetes CRDs.
- Make common actions extremely easy to configure.
- Provide reliable event processing.
- Make automations observable.
- Make the system safe by default.
- Keep the core architecture provider-agnostic.
- Make adding future providers straightforward.
- Remain lightweight enough for a homelab.

### Non-goals

Reactor is not intended to become:

- a replacement for UniFi Network
- a replacement for UniFi OS
- a general-purpose workflow platform like n8n
- a replacement for Argo Workflows
- a replacement for Home Assistant
- an arbitrary remote shell execution platform
- a huge enterprise workflow engine

The goal is focused infrastructure automation.

## Product philosophy

The core concept is:

```text
EVENT / STATE CHANGE
  ↓
MATCH / CONDITION
  ↓
ACTION
```

Example:

```text
UniFi WAN failover
        ↓
WAN = backup
        ↓
pause qBittorrent
```

There is an important distinction inside this model:

- Some things are **states**: `wan = primary | backup`, `device = online | offline`. They have a current value that can be observed at any time, and automations should reconcile against them.
- Some things are genuinely **events**: `client.connected`, a Protect motion detection. They happen at a moment in time and cannot be re-observed later.

Reactor supports both, but **state is the primary abstraction** for anything that has one. This is what makes the system self-healing: if a notification is missed, the next observation of state corrects the world.

The core engine should not know what UniFi is.

UniFi is a provider that converts UniFi-specific events and observed state into normalized Reactor events and state.

This distinction is important.

## Initial product

The first release should be **UniFi Reactor**, not a giant generic automation framework.

Initial provider:

```text
UniFi
```

Initial action targets:

```text
Kubernetes
HTTP/Webhooks
Notifications
```

The architecture should make future providers possible:

```text
UniFi
NUT
Proxmox
Prometheus
Home Assistant
Webhook
...
```

But these should not all be implemented before the first usable release.

## Architecture

High-level architecture:

```text
              UniFi
                │
   ┌────────────┴────────────┐
   │ webhook (fast path)     │ API poll (source of truth)
   ▼                         ▼
  ┌───────────────────────────────┐
  │        UniFi Provider         │
  │  parse / observe / normalize  │
  └──────────────┬────────────────┘
                 │
     normalized Event / State
                 │
                 ▼
        ┌─────────────────┐
        │ Reactor Engine  │
        │                 │
        │ Matching        │
        │ Conditions      │
        │ State           │
        │ Execution       │
        └────────┬────────┘
                 │
          normalized Action
                 │
   ┌─────────────┼──────────────┐
   ▼             ▼              ▼
Kubernetes      HTTP       Notification
 Provider     Provider       Provider
```

The UniFi provider maintains an observed-state model (initially just WAN state) by polling the UniFi Network API on a short interval. An incoming webhook does not drive actions directly; it triggers an immediate re-observation of state. This gives low latency when webhooks work and correctness when they do not.

## Kubernetes architecture

Reactor should be implemented as a proper Kubernetes Operator.

The controller should watch Reactor custom resources.

Initial CRDs:

```text
Automation
```

Potential future CRDs:

```text
EventSource
ActionProvider
Automation
```

Do not introduce unnecessary CRDs during the first implementation.

Start with a single `Automation` CRD and keep the provider configuration simple.

## The Automation API: two trigger kinds from day one

The `v1alpha1` API distinguishes state triggers from event triggers from the start, because migrating users from an event-shaped API to a state-shaped API later would be a breaking change for the flagship use case.

> **Decided, and revised:** the *split* is kept — `when` is state-shaped and always will be — but `spec.trigger` is **not** in the `v1alpha1` schema. It shipped through v0.3.0 as a field the engine never processed, which is not a defensible thing to carry into v1. It returns in `v1alpha2` once (a) a real Alarm Manager delivery has been captured in `testdata/`, so a payload matcher is written against observed ground truth rather than an assumed shape, and (b) at least one *edge* action exists for it to run — every action type today declares a level and is arbitrated continuously across the automations sharing a target, which is not something an occurrence can contribute to. The argument in this section is about `when` never having to migrate, and that is satisfied without `trigger` being present but inert. See the README's Stability section.

### State trigger (preferred for anything that has a state)

```yaml
apiVersion: reactor.robbeverhelst.dev/v1alpha1
kind: Automation
metadata:
  name: degrade-on-backup-wan
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: qbittorrent
      replicas: 0

  onExit:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: qbittorrent
      replicas: 1
```

A state trigger fires its `actions` when the state enters the condition and its `onExit` actions when the state leaves it. This replaces the pattern of manually authoring inverse automation pairs. `onExit` is optional; automatic reversal is explicit, never inferred.

### Event trigger (for genuine point-in-time events)

```yaml
apiVersion: reactor.robbeverhelst.dev/v1alpha1
kind: Automation
metadata:
  name: notify-on-client-connect
spec:
  trigger:
    provider: unifi
    event: client.connected

  actions:
    - type: http.request
      request:
        method: POST
        url: https://example.internal/hook
```

An `Automation` specifies exactly one of `when` (state) or `trigger` (event). Exact API naming can be refined during implementation, but the two-kind split itself should be kept.

> As shipped, `v1alpha1` accepts only `when`; the example above is the shape `trigger` will have when it returns, not a schema that exists today.

## Events and state

Reactor should normalize provider-specific events.

A normalized event should contain at minimum:

```text
id
provider
type
timestamp
payload
metadata
```

Conceptually:

```json
{
  "id": "01J...",
  "provider": "unifi",
  "type": "wan.failover",
  "timestamp": "2026-08-11T10:00:00Z",
  "payload": {},
  "metadata": {}
}
```

Provider-specific data should remain available through `payload`.

Normalized state is a flat, provider-scoped key/value model:

```json
{
  "provider": "unifi",
  "state": {
    "wan": "backup"
  },
  "observedAt": "2026-08-11T10:00:00Z"
}
```

State changes are derived by the provider comparing observations, and the engine reconciles Automations against current state. Missed intermediate transitions are acceptable by design; the current observation always wins.

## UniFi provider

UniFi is the first-class integration.

The provider has two inputs:

### 1. State polling (source of truth)

The provider polls the UniFi Network API for WAN state on a configurable interval (default in the range of 15 to 60 seconds). This is what actually drives state-based Automations. Polling makes the system:

- self-healing after controller restarts
- immune to missed webhook deliveries
- naturally deduplicated (state is compared, not counted)

### 2. Webhooks (fast path)

The provider exposes a local HTTP endpoint for UniFi to send events to.

```text
POST /webhooks/unifi
```

The first implementation should use UniFi's supported webhook/event mechanisms rather than relying on undocumented internal APIs unless absolutely necessary.

For state-shaped events (like WAN failover), a webhook does not directly execute automations. It triggers an immediate out-of-band poll so state converges in seconds instead of waiting for the next interval. For genuine events (client connects etc.), the webhook payload is normalized into an event as-is.

The endpoint must:

- authenticate the webhook
- validate the payload
- assign/generate an event ID if necessary
- normalize the event or schedule a state re-observation
- enqueue/process the event
- return quickly to UniFi
- avoid performing long-running actions synchronously inside the webhook handler

Do not make UniFi wait for an automation to complete.

## UniFi event mapping

The initial taxonomy should be small and useful.

State keys:

```text
wan: primary | backup
```

Potential events:

```text
wan.failover        (also mapped to state)
wan.restored        (also mapped to state)
device.online       (candidate for state in a later release)
device.offline
client.connected
client.disconnected
```

The implementation must be based on the actual captured UniFi payloads from the experiment described in "Design risks" above, and on the actual UniFi Network API responses for WAN state.

**Do not invent payload formats.**

Create a mapping layer:

```text
UniFi payload / API response
      ↓
UniFi parser / observer
      ↓
Reactor normalized event / state
```

Keep the parser isolated from the automation engine.

## Event matching

Automations need to be able to match events.

Simple:

```yaml
trigger:
  provider: unifi
  event: client.connected
```

More specific:

```yaml
trigger:
  provider: unifi
  event: device.offline
  match:
    deviceId: "..."
```

Do not initially build an unnecessarily complex expression language.

Start with:

- provider
- event type or state key/value
- simple field matching

A future condition engine can support:

```yaml
conditions:
  all:
    - ...
    - ...
```

but this should only be implemented when necessary.

## Actions

Actions should be represented by an interface similar to:

```text
ActionProvider
    Execute(ActionContext) error
```

Potential providers:

```text
kubernetes
http
notification
```

The core engine should not contain provider-specific action implementations.

### Kubernetes actions

Initial Kubernetes actions:

#### Scale

```yaml
- type: kubernetes.scale
  target:
    kind: Deployment      # or StatefulSet
    name: qbittorrent
  replicas: 0
```

Reads and writes go through the `scale` subresource rather than the kind's own
`spec.replicas`. `/scale` is the interface that says "this object has a replica
count" without saying where it is kept, so one executor serves every scalable
kind: adding one is an entry in the handler registry, an entry in the CRD enum
and an RBAC rule, and no new executor code.

`target.kind` stays a **closed enum** anyway, which is the deliberate half of
the trade-off. The gain from opening it would be imaginary: a kind is only
reachable if the chart granted RBAC for it, and RBAC has to name resources
explicitly, so an open field would accept a kind the operator cannot touch and
report a typo as a `Forbidden` during the incident rather than as a rejected
write at admission. The enum is the same decision as the chart's rule list,
written where the API server can enforce it.

#### Restart

Shipped, as an **edge** action. There is no value a workload can be held at
that means "restarted", so it declares no level, participates in no
arbitration, and fires on its own Automation's transition:

```yaml
- type: kubernetes.restart
  target:
    kind: Deployment      # or StatefulSet
    name: qbittorrent
```

It stamps `kubectl.kubernetes.io/restartedAt` on the pod template — the same
annotation `kubectl rollout restart` writes, so a workload Reactor restarted
and one restarted by hand are indistinguishable afterwards, and the workload
controller rolls the pods under the update strategy and disruption budget the
workload already declares. Reactor never deletes a pod.

**At-most-once, unconditionally.** This is the first non-idempotent action, and
it is the reason #33's retry policy is per-type rather than global. Every
execution rolls the workload, so retrying after an ambiguous failure is a
second outage rather than a correction, and the failures that actually occur —
a conflict, a Forbidden — are not ones a retry fixes. It is attempted once per
transition, recorded in `status.edgeActions`, and never retried: not within the
reconcile, and not across reconciles, because the transition is committed to
status before the action runs and a later reconcile therefore sees no edge.

**It is also what makes #30's debounce load-bearing.** The engine acts on
transitions, so a steady condition never restarts twice — but a flapping key is
a stream of transitions and each one is a real rollout. Scaling made flapping
harmless; this does not. The default debounce of 1 was chosen for
`kubernetes.scale`, and a key that drives a restart should be raised above it,
at a cost of one poll interval of reaction time per extra sample.

#### Suspend CronJob

Shipped. Desired-state, and the first action whose level is a switch rather
than a count:

```yaml
- type: kubernetes.cronjob.suspend
  target:
    kind: CronJob
    name: large-backup
  suspended: true          # optional; true is the default
```

`engine.Resolve` was deliberately *not* generalised over an ordered type
parameter to accommodate it. A switch is a two-element lattice, and embedding
it in the integers as "suspended is 0, running is 1" is order-preserving, so
the meet stays `min` and "most restrictive wins" stays "suspended wins" without
the engine learning a second kind of value. A target has exactly one kind, so
two levels in different units never meet in the first place — the generality
would have had no case to serve.

The baseline is recorded under `reactor.robbeverhelst.com/baseline-suspend`,
not under `baseline-replicas`. That annotation is a compatibility promise about
replica counts as of v1.0, and a reader — a person, or a script over `kubectl
get -o custom-columns` — is entitled to keep reading `"1"` there as one
replica. A kind whose level is not a count records it under its own name.

Suspending does not stop a Job already running, and deliberately grants no
permission over Jobs at all: declining to start more work is a categorically
safer act than killing work in flight, and deleting in-flight Jobs is not
something an outage should decide on the operator's behalf.

#### Cordon — and why there is no drain

`kubernetes.cordon` is shipped, as a desired-state action behind an explicit
chart opt-in:

```yaml
- type: kubernetes.cordon
  target:
    kind: Node
    name: worker-03
  cordoned: true         # optional; true is the default
```

`spec.unschedulable` is a level, ordered so that cordoned is the restrictive
answer, so it folds like every other level and needs no new rule. It is the
first cluster-scoped target, which is why `target.namespace` is rejected on a
`Node` rather than defaulted.

`kubernetes.drain` — proposed in #18 alongside it — is **deliberately not
implemented**. Not deferred behind a flag, not implemented with a timeout: not
built. The reasoning, because a well-evidenced "no" is worth more here than a
dangerous feature:

1. **An eviction cannot be reversed, so it cannot be a level.** Every action in
   this design declares a value that is a pure function of which conditions
   currently hold — that is what makes the outcome independent of reconcile
   order, a controller restart harmless, and `onExit` expressible. A drain has
   no such value. There is no state a node can be held at that means "drained",
   nothing for `spec.reversal` to declare, and nothing for a later reconcile to
   correct. A flapping key would empty the node once per flap.
2. **It inverts its own goal on a small cluster.** Draining assumes spare
   capacity elsewhere. On three nodes behind one UPS the evicted pods do not
   move, they go Pending — so the workload is lost at the moment of the drain
   rather than at the moment the battery dies. Cordoning delivers the actual
   benefit (new pods land on the node still on mains) without that cost.
3. **It can evict the operator performing it**, if Reactor's own pod is on the
   node. Nothing else here can kill the thing doing and reporting the work,
   mid-action.
4. **It hangs by design.** Eviction respects PodDisruptionBudgets, and a
   single-replica workload with `minAvailable: 1` blocks indefinitely. A
   timeout bounds the call; it does not bound a half-drained node.

The RBAC follows the decision rather than merely reflecting it:
`rbac.allowNodeActions` grants `nodes` and nothing over `pods` or
`pods/eviction`, under any setting. An operator whose plan genuinely needs a
drain should pair `kubernetes.cordon` with a notification and run `kubectl
drain` by hand — an irreversible cluster-wide decision at 3am is the right
place for a human.

#### Patch resource

Potential future action:

```yaml
- type: kubernetes.patch
  target:
    kind: Deployment
    name: example
```

Do not initially expose unrestricted arbitrary Kubernetes operations.

Actions should be explicit and auditable.

### HTTP actions

A generic HTTP action is extremely useful.

Example:

```yaml
- type: http.request
  request:
    method: POST
    url: https://example.internal/hook
```

Support:

- GET
- POST
- PUT
- PATCH

Credentials should come from Kubernetes Secrets rather than being stored directly in Automation resources.

> **As shipped.** `http.request` and `notification.ntfy` / `notification.discord` / `notification.slack` exist, and are the first *edge* actions — they fire on a transition rather than declaring a level, so they take no part in target arbitration. Two things about them are narrower than this section implies, and deliberately so:
>
> - **Destinations are allowlisted at install time and refused by default.** `spec.actions` is writable by anyone who can create an `Automation`, and the request goes out with the operator's network position rather than the author's, so which hosts that is worth is not a per-Automation decision. The loopback interface and link-local addresses are refused whatever the allowlist says, and redirects are not followed.
> - **Only the body is templated.** The URL and the headers are literal, because letting observed state edit the destination would give back the choice the allowlist exists to take away.
>
> Credentials come from a Secret in the `Automation`'s own namespace — never inline, never cross-namespace. See [SECURITY.md](../SECURITY.md#outbound-actions) for the threat model and the [README](../README.md#telling-you-what-happened) for the shape.

#### Named integrations over the generic action

`homeassistant.service` is shipped, and is the first action to sit *on top of*
`http.request` rather than beside it. This section's instinct — prefer the
generic action, do not build a provider per service — is right, and the shape
that follows from taking it seriously is not "no integration" but "an
integration that is a shape over the one transport".

There is exactly one outbound HTTP client. A named integration decides two
things — what the URL is and what the body is — and inherits the destination
allowlist, the address floor in the dialer, the redirect refusal, the
origin-only reporting and the Secret rules. Adding a second client would be the
actual mistake this section is warning about, because each of those is a place
to get security wrong once per client.

The bar for naming an integration, from the two that cleared it:

1. **It constrains the request.** `homeassistant.service` builds its path from a
   `domain` and a `service`, each a bare slug. That is strictly less reach than
   the `http.request` the same allowlist entry already permits, and it makes the
   action state what it is.
2. **Or the exchange is not one request.** See qBittorrent below.

Neither "it is convenient" nor "it is popular" is on that list. An integration
that would be a `url` and a `body` an author could have written themselves is a
documented example, not an action type.

### qBittorrent

Do not initially create a qBittorrent-specific action provider unless it is necessary.

The first implementation can use the generic HTTP provider or a small integration later.

However, qBittorrent is an important reference use case.

The desired behavior:

```text
WAN → backup
    ↓
pause torrents

WAN → primary
    ↓
resume torrents
```

This should be demonstrated in documentation using a single state-triggered Automation with `onExit`.

> **As shipped**, as `qbittorrent.pause` and `qbittorrent.resume`. This section's
> instruction was to prefer the generic HTTP action, and the first step was to
> prove whether it can express this. It can only under one configuration, and
> the reason it cannot in general is what justifies the action.
>
> **The generic-HTTP attempt.** qBittorrent's WebUI authenticates with a session
> cookie from `POST /api/v2/auth/login`, not with a static token. `http.request`
> is one request: it holds no cookie jar, follows no redirect, and there is no
> value that could be put in a Secret ahead of time, because the `SID` does not
> exist until the login happens. So:
>
> - With authentication **bypassed** for the calling subnet, a plain
>   `http.request` — `POST {base}/api/v2/torrents/pause`, body `hashes=all` —
>   works, and is documented as the right answer for that setup.
> - With authentication **on**, which is the default and the only sensible
>   posture for something reachable from the cluster, it cannot be expressed at
>   all. Not awkwardly: at all.
>
> That is the bar the [named integrations](#named-integrations-over-the-generic-action)
> section states — the exchange is not one request — and it is met. What was
> *not* built is a second HTTP client: `Session` is a field on the existing
> request, so the login, the action and the logout all go through the same
> allowlist check, the same dialer floor, the same redirect refusal and the same
> origin-only reporting.
>
> **The session and the credential rule.** A `SID` is a bearer of the same
> authority as the password, so caching one across reconciles would be exactly
> what this project refuses to do with the password. There is no session cache.
> The login happens inside the one action, the cookie lives in a local variable,
> and a logout ends the session on the far end rather than leaving it to expire.
> A retry re-runs the whole exchange.

#### Pausing is a level, and this is an edge action anyway

The interesting part of #21 is not the HTTP. Paused-versus-running is a level,
and a level is the thing this design arbitrates — so the action *looks* like a
desired-state action and is not one.

What makes a desired-state action possible is not the fold. It is that the
target is a Kubernetes object, so the value it held before Reactor claimed it
can be recorded as an annotation **on that object**, where it outlives the
Automation, outlives Reactor, and is readable by the pre-delete sweep during an
uninstall. That baseline is what makes release possible, and release is what
makes a claim legitimate.

A qBittorrent instance reached over HTTP has no Kubernetes identity to arbitrate
over, no annotation to hold a baseline, and nothing the uninstall hook could
reach if it did — that hook runs with no credentials and no destination
allowlist, by design. The alternatives were considered and rejected in the
[README](../README.md#it-is-a-level-in-the-world-and-an-edge-action-here):
status dies with the Automation, a tag written into the user's torrent client
is both editable by them and unreadable by a client that never parses a response
body, and a synthesized identity would arbitrate on string equality of a URL.

So it ships as an edge action, named as a verb, with two limitations stated
rather than hidden: it is not arbitrated, and `resume` resumes torrents that
were paused by hand because nothing recorded which those were.

**The general conclusion, which is the durable part.** Non-Kubernetes
desired-state targets need a design that does not exist yet: somewhere to keep a
baseline and a claim for a thing with no object to hang them on, and some way
for the uninstall path to reach it. Until that exists, the rule is that a
desired-state action targets a Kubernetes object, and anything else is an edge
action named as a verb. That is a rule about the whole action taxonomy, not
about torrents.

## State

State is the primary abstraction, not a future feature.

An event:

```text
wan.failover
```

means:

> The WAN failover event occurred at some moment.

State:

```text
wan = backup
```

means:

> The WAN is currently operating through the backup connection.

Reactor reconciles Automations against current observed state, with the following properties:

- On controller startup, state is observed immediately and Automations converge, even if failover happened while the controller was down.
- Repeated identical observations are no-ops.
- `onExit` actions run when a previously-matching state condition stops matching.
- Automatic reversal is only ever what the user declared in `onExit`, and each `onExit` execution is recorded in status for auditability. Reversal is powerful and can be dangerous; it must always be explicit and observable.

Genuine point-in-time events remain supported through `trigger`, but nothing that has an observable current value should be modeled as an event only.

## Reliability

This is an infrastructure automation system.

Reliability matters.

The state-first design already provides much of this for free for stateful automations: missed deliveries, duplicate deliveries, and controller restarts all resolve on the next observation. The mechanisms below mostly protect the event path and action execution.

### Event IDs

Every event should have a unique ID.

### Deduplication

Duplicate webhook deliveries should not cause duplicate destructive actions. For state triggers this is inherent (state comparison). For event triggers, deduplicate on event ID within a bounded window.

### Retry

Failed actions should be retried with bounded exponential backoff.

> **As shipped**, the policy differs by action kind, because the kinds fail differently. A desired-state action is idempotent by construction, so the reconcile loop is itself the retry, bounded at five consecutive failures. An edge action fires on an occurrence that has already passed, so it is never retried across reconciles — a later reconcile has no new transition, and re-sending there would be a duplicate rather than a retry. Whether it may be repeated *within* its one reconcile is decided per type: notifications are publishes and retry three times, and an `http.request` retries only when the method is idempotent (`GET`, `PUT`) or the author declares it so.

### Timeouts

Actions must have configurable timeouts.

### Idempotency

Actions should be designed to be safe when retried.

For example:

```text
scale deployment → replicas=0
```

is preferable to:

```text
send "scale down" command
```

because repeating the first operation has predictable behavior.

### Persistence

State observations are re-derived from the source on startup, so no external database is needed for the core loop.

Consider whether event processing needs persistence across controller restarts. Kubernetes resources/status and a lightweight internal queue should be preferred initially. Do not introduce an external database unless necessary.

## Security

Security is a major requirement.

The webhook endpoint must not be an unrestricted remote command interface.

Never allow:

```yaml
action:
  command: "rm -rf ..."
```

or arbitrary shell execution by default.

Kubernetes actions must use a dedicated ServiceAccount with minimal RBAC.

For example, if an Automation only needs to scale Deployments, the controller should not require cluster-admin.

Secrets must be stored in Kubernetes Secrets. This includes the UniFi API credentials used for state polling.

Webhook authentication should be supported.

Potential mechanisms:

```text
shared secret
HMAC signature
authorization header
```

Use the mechanism supported by UniFi where possible.

## RBAC

The operator should follow least privilege.

If the controller needs:

```text
get deployments
patch deployments
```

give it exactly those permissions.

As shipped, a target kind costs `get` and `patch` and nothing else. Targets are
read as unstructured objects through the manager's uncached client, so no
target kind starts an informer — which is what would have made `list` and
`watch` necessary, and would have held every object of that kind in the
operator's memory to answer a question asked about one of them every fifteen
seconds.

Do not ship:

```text
cluster-admin
```

unless there is a compelling reason.

Anything reaching outside the workloads Reactor was installed to manage is
opt-in rather than default, and is a separate object so that turning it on is a
visible decision. `kubernetes.cordon` is the only such permission today: nodes
are cluster-scoped, so `rbac.allowNodeActions` creates a ClusterRole even in a
namespace-scoped install, and the chart says so where the value is set. The
generated `config/rbac/role.yaml` deliberately does not carry it, so the
manifest bundle grants no node access at all.

Document how users can restrict Reactor's permissions.

## Observability

The controller should be easy to debug.

Provide:

### Kubernetes status

Example:

```yaml
status:
  phase: Ready
  observedState:
    wan: backup
  lastTransition:
    from: primary
    to: backup
    time: "2026-08-11T10:00:00Z"
  lastExecution:
    status: Success
```

### Logs

Structured logs.

Example:

```text
INFO state observed provider=unifi wan=backup
INFO state transition provider=unifi key=wan from=primary to=backup
INFO automation matched automation=degrade-on-backup-wan
INFO executing action type=kubernetes.scale target=deployment/qbittorrent
INFO action completed status=success
```

### Metrics

Eventually expose Prometheus metrics such as:

```text
reactor_events_total
reactor_state_transitions_total
reactor_automation_matches_total
reactor_actions_total
reactor_action_failures_total
reactor_action_duration_seconds
```

Do not block the first MVP on an extensive metrics system.

## Status and conditions

Automation resources should expose useful Kubernetes conditions.

For example:

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: Valid
```

Invalid configuration should result in:

```yaml
status:
  conditions:
    - type: Ready
      status: "False"
      reason: InvalidConfiguration
```

The controller should not repeatedly attempt to execute invalid automations.

## Installation

The project should eventually support:

```bash
helm repo add reactor ...
helm install reactor ...
```

and ideally:

```bash
kubectl apply -f https://...
```

Installation should create:

- CRDs
- ServiceAccount
- RBAC
- Deployment
- Service
- webhook endpoint

A Helm chart should be maintained from the beginning.

## CI and releases

The project must be installable from public artifacts from the first tagged release. GitHub Actions is the assumed platform.

### CI (every PR and push to main)

```text
lint          golangci-lint
test          unit + envtest
build         controller binary
docker-build  image build (no push)
chart-lint    helm lint + helm template
manifests     verify generated CRDs/deepcopy are committed (make manifests produces no diff)
```

CI must be green before anything can be released.

### Release pipeline (on version tag)

Tagging `vX.Y.Z` should produce, automatically:

```text
1. multi-arch container image (amd64 + arm64, homelabs run on both)
       → ghcr.io/<owner>/unifi-reactor:vX.Y.Z
2. Helm chart with matching appVersion
       → pushed as OCI artifact to ghcr.io/<owner>/charts/reactor
3. install manifest bundle (install.yaml) attached to the GitHub Release
       → enables: kubectl apply -f https://github.com/.../releases/download/vX.Y.Z/install.yaml
4. GitHub Release with generated changelog
```

Notes:

- Prefer OCI chart hosting on GHCR over a gh-pages chart repo; it is one less moving part and `helm install oci://...` is well supported now.
- Use release-please (or equivalent) with conventional commits to automate version bumps and changelogs, matching the workflow already used for existing open-source packages.
- Image and chart versions must always move together; the chart's `appVersion` is set from the tag in the pipeline, never by hand.
- Sign images and charts (cosign keyless via GitHub OIDC) once the pipeline is stable; do not block v0.1 on it.
- No release steps run on a developer machine. If it is not reproducible from a tag in Actions, it is not the release process.

## Repository structure

Suggested initial repository:

```text
unifi-reactor/
├── .github/
│   └── workflows/
│       ├── ci.yaml
│       └── release.yaml
│
├── api/
│   └── v1alpha1/
│       ├── automation_types.go
│       └── zz_generated.deepcopy.go
│
├── internal/
│   ├── controller/
│   │   └── automation_controller.go
│   │
│   ├── engine/
│   │   ├── engine.go
│   │   ├── matcher.go
│   │   ├── state.go
│   │   └── executor.go
│   │
│   ├── events/
│   │   ├── event.go
│   │   └── bus.go
│   │
│   ├── providers/
│   │   ├── unifi/
│   │   ├── kubernetes/
│   │   └── http/
│   │
│   └── webhook/
│       └── server.go
│
├── config/
│   ├── crd/
│   ├── rbac/
│   └── manager/
│
├── charts/
│   └── reactor/
│
├── examples/
│   ├── unifi-wan-failover/
│   ├── kubernetes-scale/
│   └── ...
│
├── testdata/
│   └── unifi/          # real captured UniFi payloads and API responses
│
├── test/
│
├── Dockerfile
├── Makefile
├── go.mod
└── README.md
```

Use standard Kubernetes Operator conventions.

A Go implementation using Kubebuilder/controller-runtime is recommended unless there is a strong technical reason otherwise.

## Development principles

### Keep the core generic

The engine must not contain code like:

```go
if event.Provider == "unifi" {
    ...
}
```

Instead:

```text
Provider → normalized Event / State → Engine
```

UniFi-specific logic belongs in the UniFi provider.

### Don't over-generalize the API

Do not design 20 CRDs before implementing the first use case.

Start with:

```text
Automation
```

and a clean provider/action abstraction.

Add abstractions only when a real second provider requires them.

## MVP

The first usable version should support exactly this:

```text
UniFi WAN state (poll + webhook fast path)
      ↓
state normalization
      ↓
Automation matching (when: state)
      ↓
Kubernetes action (+ onExit)
```

Specifically:

### Trigger

```text
unifi state wan = backup
```

### Action

```text
scale Deployment (with onExit reversal)
```

Example:

```yaml
apiVersion: reactor.robbeverhelst.dev/v1alpha1
kind: Automation
metadata:
  name: pause-qbittorrent-on-backup-wan
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: qbittorrent
      replicas: 0

  onExit:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: qbittorrent
      replicas: 1
```

This single resource covers both failover and recovery. It should work end-to-end before implementing additional features.

## MVP 2

Add:

```text
event triggers (trigger:)
HTTP actions
Secrets
retry
event deduplication
status
metrics
```

## Future providers

Once the UniFi integration is mature, add providers based on real demand.

Potential providers:

### NUT

For UPS state:

```text
ups: online | on-battery | low-battery | critical
```

Example:

```text
UPS enters battery mode
        ↓
Reactor
        ↓
scale nonessential workloads to zero
```

Note that UPS status is a textbook state, which is another reason state is the primary abstraction.

### Proxmox

Potential events/state:

```text
VM started / stopped
node: online | offline
```

### Prometheus

Allow alerts to trigger Reactor automations. Prometheus alerts are themselves stateful (firing/resolved), which maps cleanly onto `when` / `onExit`.

### Webhook

A generic webhook provider makes almost anything integratable.

### Home Assistant

Useful for homelab and home infrastructure events.

## Future actions

Potential actions:

```text
Kubernetes
├── scale
├── restart
├── patch
├── suspend CronJob
└── rollout

HTTP
├── GET
├── POST
├── PUT
└── PATCH

Notifications
├── Discord      (shipped)
├── Slack        (shipped)
├── ntfy         (shipped)
├── Telegram
└── Email

Infrastructure
├── Proxmox
├── qBittorrent
├── Home Assistant
└── ...
```

These should only be added when they solve real use cases.

## Example automations

### Pause downloads while on backup WAN (with automatic resume)

```yaml
apiVersion: reactor.robbeverhelst.dev/v1alpha1
kind: Automation
metadata:
  name: pause-downloads-on-backup-wan
spec:
  when:
    provider: unifi
    state:
      wan: backup

  actions:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: qbittorrent
      replicas: 0

  onExit:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: qbittorrent
      replicas: 1
```

### Stop a heavy Kubernetes workload during UPS battery mode

Future NUT provider:

```yaml
apiVersion: reactor.robbeverhelst.dev/v1alpha1
kind: Automation
metadata:
  name: reduce-load-on-ups
spec:
  when:
    provider: nut
    state:
      ups: on-battery

  actions:
    - type: kubernetes.scale
      target:
        kind: Deployment
        name: heavy-workload
      replicas: 0
```

### Send a notification when a client connects

```yaml
apiVersion: reactor.robbeverhelst.dev/v1alpha1
kind: Automation
metadata:
  name: notify-client-connect
spec:
  trigger:
    provider: unifi
    event: client.connected

  actions:
    - type: http.request
      request:
        method: POST
        url: https://example.com/webhook
```

## Future: modes

A possible future abstraction:

```text
normal
degraded
emergency
maintenance
```

For example:

```text
WAN primary
    → normal

WAN backup
    → degraded

UPS < 30%
    → emergency
```

Automations could then react to infrastructure modes. Modes are a natural extension of the state model: a mode is derived state computed from provider state.

This is a future concept, not an MVP requirement.

## Design for idempotency

Automations may observe the same state repeatedly or receive duplicate events.

For example:

```text
wan=backup observed
wan=backup observed
wan=backup observed
```

must not cause repeated action execution.

Prefer desired-state actions:

```text
replicas = 0
```

rather than imperative actions whenever possible. The engine only executes actions on state transitions, never on repeated identical observations.

## Testing

The project must have automated tests.

At minimum:

### Unit tests

- state observation and transition detection
- event normalization (against real captured payloads in `testdata/`)
- event matching
- condition evaluation
- action execution (including `onExit`)
- deduplication
- retries

### Controller tests

Use envtest/controller-runtime testing.

### Integration tests

Test:

```text
UniFi state change (simulated API + webhook)
    ↓
Automation
    ↓
Kubernetes Deployment
```

The test should verify that a state transition causes the expected Kubernetes state, that repeated observations are no-ops, and that exiting the state runs `onExit`.

## Local development

Provide a simple development workflow.

Ideally:

```bash
make test
make lint
make build
make docker-build
make install
make deploy
```

For local Kubernetes testing, support a lightweight cluster such as:

```text
kind
```

or:

```text
k3d
```

Document how to send a fake UniFi webhook locally:

```bash
curl -X POST \
  http://localhost:8080/webhooks/unifi \
  -H 'Content-Type: application/json' \
  -d @testdata/unifi/wan-failover.json
```

and how to run against a mock UniFi API for state polling.

## Documentation

Documentation should focus heavily on practical examples.

The first page should show:

```text
UniFi WAN failover
        ↓
pause qBittorrent
        ↓
UniFi WAN restored
        ↓
resume qBittorrent
```

achieved with a single copy/pasteable Automation resource.

Documentation should cover:

- Installation
- UniFi API credentials and webhook configuration
- Authentication
- First Automation
- State triggers and `onExit`
- Event triggers
- Kubernetes actions
- HTTP actions
- Secrets
- Troubleshooting
- Security
- Adding providers
- Adding actions

## Project roadmap

### v0.0 (spike, before any operator code)

- [ ] Capture real UniFi webhook payloads for WAN failover/recovery
- [ ] Capture real UniFi Network API responses for WAN state
- [ ] Commit both to `testdata/`
- [ ] Decide final shape of the UniFi provider based on findings

### v0.1

UniFi-first MVP:

- [ ] Kubernetes Operator
- [ ] Automation CRD (state trigger + `onExit`, event trigger schema reserved)
- [ ] UniFi WAN state poller
- [ ] UniFi webhook endpoint (fast-path re-observation)
- [ ] State normalization and transition detection
- [ ] Kubernetes scale action
- [ ] Basic status (observed state, last transition)
- [ ] Helm chart
- [ ] CI workflow (lint, test, build, chart-lint, manifest drift check)
- [ ] Release workflow (multi-arch image to GHCR, OCI chart, install.yaml, changelog)
- [ ] Example automation
- [ ] Tests

### v0.2

Reliability and events:

- [ ] webhook authentication
- [ ] event triggers
- [ ] event IDs
- [ ] deduplication
- [ ] retries
- [ ] action timeouts
- [ ] better status conditions
- [ ] Prometheus metrics
- [ ] structured logging
- [ ] HTTP action
- [ ] image/chart signing (cosign keyless)

### v0.3

More UniFi:

- [ ] device online/offline
- [ ] client events
- [ ] additional Network events
- [ ] richer matching
- [ ] better UniFi documentation

### v0.4+

Expand providers based on demand:

- [ ] NUT
- [ ] Proxmox
- [ ] generic webhook
- [ ] Prometheus
- [ ] Home Assistant

Do not implement every possible provider just because it is listed here.

## Long-term vision

The long-term vision is:

> Reactor is an event-driven infrastructure automation layer for homelabs and Kubernetes.

It should allow infrastructure to react to infrastructure.

Examples:

```text
UniFi WAN failure
        ↓
degraded network mode
        ↓
pause downloads
```

```text
UPS on battery
        ↓
reduce workload
        ↓
UPS critical
        ↓
shutdown nonessential infrastructure
```

```text
Proxmox node failure
        ↓
react
        ↓
restart workload elsewhere
```

```text
Prometheus alert
        ↓
automation
        ↓
restart or scale service
```

The common abstraction remains:

```text
State / Event
  ↓
Match
  ↓
Action
```

## Important implementation instruction

**Build the smallest useful system first.**

Do not spend the first implementation phase building:

- a UI
- a complicated expression language
- 15 providers
- a database
- a workflow DAG engine
- arbitrary shell execution
- an enormous plugin framework

The first milestone is:

```text
UniFi WAN state
  ↓
poll + webhook
  ↓
Reactor
  ↓
Automation CRD
  ↓
Kubernetes scale action (+ onExit)
```

If this works reliably, expand from there.

The project should be simple to understand, simple to install, and simple to extend.

## Branding

Project name:

> **UniFi Reactor**

Future umbrella name:

> **Reactor**

Suggested repository:

```text
unifi-reactor
```

Suggested tagline:

> Event-driven automation for UniFi networks and Kubernetes.

Suggested short description:

> An open-source Kubernetes Operator that turns UniFi events into infrastructure actions.

The word **Reactor** should remain part of the branding. It describes the core concept: infrastructure events cause infrastructure reactions.

## License

Use a permissive open-source license suitable for community contributions, preferably Apache-2.0 unless there is a specific reason to choose another license.

## First task for the coding agent

Before implementing anything:

1. Run the v0.0 spike: capture real UniFi webhook payloads and Network API WAN-state responses by triggering an actual failover, and commit them to `testdata/`. Everything below depends on what this reveals. Do not proceed on assumed payload formats.
2. Inspect this specification.
3. Choose a standard Kubernetes Operator stack, preferably Go + controller-runtime/Kubebuilder.
4. Create the repository structure.
5. Implement the Automation CRD with the two trigger kinds (`when` state trigger with `onExit` implemented first; `trigger` event schema defined but may be stubbed).
6. Implement the controller.
7. Implement the normalized state and event models.
8. Implement the UniFi WAN state poller using the captured API responses.
9. Implement the UniFi webhook receiver as a fast-path re-observation trigger, using the captured payloads.
10. Implement state matching and transition detection.
11. Implement Kubernetes Deployment scaling.
12. Add the first end-to-end test.
13. Add a Helm chart.
14. Add a working example YAML.
15. Add documentation explaining how to configure UniFi credentials and the webhook.
16. Keep the implementation small and clean.

Do not implement future providers until the UniFi → Automation → Action path is working reliably.

The first success criterion, with a single Automation resource:

```text
UniFi WAN → backup
        ↓
Reactor observes transition
        ↓
Automation matches
        ↓
qbittorrent Deployment replicas → 0
```

Then:

```text
UniFi WAN → primary
        ↓
Reactor observes transition
        ↓
onExit runs
        ↓
qbittorrent Deployment replicas → 1
```

Everything else comes afterwards.
