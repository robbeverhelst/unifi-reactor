# Adding a provider

A provider answers one question: **what is true right now?** It converts vendor-specific reality — an API response, a socket, a scrape — into a flat `map[string]string` of normalized state. Everything else is the engine's job.

The central architectural claim of this project is that the engine is provider-agnostic, and that a new provider arrives without touching it. This page is the test of that claim. Adding one means:

- a new package under `internal/providers/<name>/` — the vocabulary and the observer,
- a poller that feeds the shared `StateStore` — the same shape as the UniFi one,
- about twenty lines of wiring in `cmd/main.go`, plus chart values,
- fixtures captured under the same allowlist policy as UniFi's.

`internal/engine/` does not change. If your provider needs it to, something has gone wrong — see [When you need the engine to change](#when-you-need-the-engine-to-change).

There is no plugin registry and no dynamic loading. Providers are compiled in and wired by hand. That is deliberate for v1: two providers do not justify a registry, and hand-wiring keeps the seam visible.

## The seam, in three types

Everything crossing from a provider into the engine is one of these.

**`events.Observation`** (`internal/events/event.go`) — a provider's complete state at a point in time:

```go
type Observation struct {
    Provider   string
    State      map[string]string
    ObservedAt time.Time
}
```

*Complete* is the load-bearing word. An observation is not a delta. Whatever it contains is treated as the whole truth for that provider, and a key absent from it is understood as "not currently observable", never as "unchanged".

**`engine.Transition`** (`internal/engine/state.go`) — a derived change of one key between two consecutive observations. Providers never construct these; the store does.

**`engine.StateStore`** — holds the latest observation per provider and derives transitions by comparing consecutive ones. `Observe` is the only write path:

```go
func (s *StateStore) Observe(o events.Observation) []Transition
```

Repeated identical observations produce no transitions, which is what makes polling cheap to do often. The first observation for a provider reports every key as a transition from `""`.

Note what is *not* in the seam: no callbacks into the engine, no way for a provider to run an action, no provider-specific fields. A provider produces a map. That is the entire surface.

## Walking the UniFi provider

Four files, ~350 lines including tests. Read them in this order.

### 1. `internal/providers/unifi/state.go` — the vocabulary

The whole file is constants:

```go
const (
    // ProviderName is how Automations refer to this provider.
    ProviderName = "unifi"

    stateKeyWAN        = "wan"
    stateKeyUPS        = "ups"
    stateKeyUPSBattery = "ups.battery"

    wanPrimary = "primary"
    wanBackup  = "backup"

    upsOnline    = "online"
    upsOnBattery = "on-battery"
    // ...
)
```

`ProviderName` is exported because `cmd/main.go` and the poller need it. **Everything else is unexported on purpose.** Key and value strings are user-facing API — they appear verbatim in `spec.when.state` — so they live in exactly one file, and nothing outside the package can spell one by accident.

Start your provider here, before writing any client code. The vocabulary is the hard part and the part you cannot change later without breaking people's Automations.

### 2. `internal/providers/unifi/client.go` — the observer

The contract is one method:

```go
func (c *Client) Observe(ctx context.Context) (map[string]string, error)
```

Called on a ticker. Returns the current state, or an error. It must be safe to call repeatedly and concurrently with nothing else happening, and it must not block indefinitely — the UniFi client sets a 10s HTTP timeout in addition to honouring `ctx`.

Note the shape of `stateFromDevices`: the parse is split from the transport so it can be tested against a captured fixture without a server. Do the same. The transport half is boring; the derivation half is where the bugs are, and it is the half you want covered by real payloads.

Two decisions in that function are worth copying:

**Omit what you cannot see.** The UPS keys are only set when a device with a `vbms_table` is present. No UPS adopted means no `ups` key at all — not `ups: unknown`. This is what lets the reconciler distinguish "the condition ended" from "I lost sight of it" and hold the last known state under `StateKeyUnavailable` instead of running `onExit`. Inventing a placeholder value for missing hardware defeats that mechanism and will scale someone's workloads back up mid-outage.

**Fail loudly when you observe nothing at all.** When no key can be derived, `Observe` returns an error rather than an empty map. An empty observation would look like every key disappearing at once.

### 3. `internal/controller/unifi_poller.go` — the loop

`UniFiPoller` is a `manager.Runnable`. Its `Start` does four things in a ticker loop:

```go
state, err := p.Client.Observe(ctx)
observation := events.Observation{Provider: unifi.ProviderName, State: state, ObservedAt: time.Now()}
transitions := p.Store.Observe(observation)
if len(transitions) > 0 {
    p.wake(ctx)
}
```

Three details are not obvious and all three matter:

**`NeedLeaderElection() bool { return true }`.** Only the active manager polls. Without it, every replica hammers the upstream API and races on the store.

**An observation failure logs and continues.** It does not return an error, because returning one from a `Runnable` takes down the manager. A console that is briefly unreachable must not restart the operator; the next tick corrects it. This is the state-first design working: there is no missed-event backlog to recover.

**`wake` is non-blocking on purpose.** After a transition, the poller enqueues the affected Automations so they reconcile immediately instead of waiting out the reconciler's periodic re-evaluation. The send has a `default:` case that drops the wake and logs at V(1):

```go
select {
case p.Events <- event.GenericEvent{Object: automation}:
case <-ctx.Done():
    return
default:
    // Never let a saturated queue stall observation: the periodic
    // re-evaluation still picks this Automation up.
}
```

A full wake channel must never stall observation. The wake is an optimization; the reconciler's periodic re-evaluation is the mechanism of record. Copy this exactly — a blocking send here couples your provider's liveness to reconciler throughput.

`wake` filters the Automation list by `Spec.When.Provider`, so your poller only wakes its own. That filter is currently written once per poller. When the second provider lands, the obvious refactor is lifting it into a shared helper parameterized by provider name — a change inside `internal/controller/`, not `internal/engine/`.

**Do not confuse `Events` with `Nudge`.** They point in opposite directions and only one is required. `Events` is outbound — the poller telling the reconciler that something changed, as above. `Nudge` is inbound and optional: something outside the loop asking for an observation to happen *sooner*, which is how the UniFi webhook fast path works. It is a separate `<-chan struct{}` in the same `select`, and a nil channel simply means the provider has no fast path:

```go
select {
case <-ctx.Done():
    return nil
case <-ticker.C:
case <-p.Nudge:
    // A nudge says "look now". It never says what changed.
    if !waitFor(ctx, minInterval-time.Since(observedAt)) {
        return nil
    }
}
```

Three properties make that safe to copy. The nudge channel holds a **single slot** with a non-blocking send at the other end, so a burst of requests coalesces into one pending observation rather than one per request. `MinObserveInterval` floors the gap between two observations, so whoever is sending nudges — including someone who should not be — cannot turn them into unbounded upstream traffic. And the observation that follows is what decides the state; a nudge carries no data at all.

**A nudge must not interact with debounce.** This is the one place the fast path and the settling policy below genuinely meet, and getting it wrong is a security bug rather than a bug. Debounce reports a changed value only after N consecutive observations — but if a nudge can cause an observation, then anyone able to send nudges can also supply those N samples, and push a debounced key through its settling time in a fraction of the intended window. The fix is one condition, using `StateStore.Proving` to ask whether anything is part-way through its threshold:

```go
case <-p.Nudge:
    if p.Store.Proving(unifi.ProviderName) {
        // A delivery may make Reactor look sooner. It may not make
        // Reactor believe something sooner.
        continue
    }
```

A nudge is still allowed to *start* a debounce — that is "look sooner", and it costs one observation. Every later nudge is refused until the value is either promoted or abandoned, so the samples that promote it always come from the poll cadence. If your provider has an inbound trigger and any debounced key, copy this condition too.

### 4. `cmd/main.go` — the wiring

The provider is constructed only when its configuration is present, and its absence is logged rather than fatal:

```go
unifiConfig, unifiEnabled, err := unifi.ConfigFromEnv(os.Getenv)
if err != nil { /* fatal */ }
if unifiEnabled {
    if err := setupUniFi(mgr, unifiConfig, store, wake); err != nil { /* fatal */ }
} else {
    setupLog.Info("UniFi provider disabled (UNIFI_URL not set); state triggers will stay pending")
}
```

Reading the environment lives in the provider package, not in `main`: `unifi.ConfigFromEnv` takes a `lookup func(string) string`, returns a `Config` plus whether the provider is configured at all, and is unit-tested without touching process state. `main` keeps only the wiring. Copy that split — `main` grows one block per provider and stays readable, and the mapping from environment to behaviour gets tests instead of a code review.

Both `store` and `wake` are created once and shared by every provider. A second provider adds a second block against the same two values. Missing *credentials* when the provider is enabled is fatal — `ConfigFromEnv` resolves the key and `main` exits if it cannot be read at all — while a *disabled* provider is not. Follow that split: an operator that silently polls nothing is worse than one that refuses to start.

The one place that rule bends is an **optional** part of a provider that is not the mechanism of record. `setupUniFi` validates the webhook fast path separately, and a fast path that cannot start is logged and skipped while the poller is added regardless:

```go
switch err := cfg.Webhook.Validate(); {
case err != nil:
    setupLog.Error(err, "Webhook fast path not started; UniFi state still converges on the poll interval")
case cfg.Webhook.Enabled:
    // ... receiver, and optionally self-registration ...
}
if err := mgr.Add(poller); err != nil { return err }
```

The test for which side of that line something sits on: if it is broken, does the operator still converge? A missing credential means it never observes anything, so that is fatal. A misconfigured optimization only costs latency, so it is a log line.

### Credentials: resolve per request, not at startup

Worth copying wholesale, because getting it wrong produces a support burden rather than a crash. The client does not hold a key string; it holds a function:

```go
// APIKey supplies the key sent with a request. It is resolved per request
// rather than held from startup so that rotating the credential does not
// require restarting the operator.
type APIKey func() (string, error)
```

`FileAPIKey(path)` reads a mounted Secret on every use; `StaticAPIKey(key)` closes over an environment variable, which cannot change for the life of the process. `Observe` calls the function per request and propagates its error like any other observation failure.

The payoff is that rotating the credential needs no restart — the kubelet updates a mounted Secret in place and the next poll picks it up. Two constraints make that true, and both belong in your chart:

- **Mount the whole directory, never through `subPath`.** A `subPath` mount is a copy taken at container start and is never refreshed, which silently restores the restart requirement.
- **Resolve once at startup too**, so an unreadable or empty credential fails fast instead of failing every poll forever.

An environment variable holding a secret is the easy default and the wrong one. Take the file.

## Designing the state vocabulary

The rules below come from mistakes already made once in this project. They are the parts a new provider gets wrong.

**Model state, not events.** If it has an observable current value, it is state. Events cannot be re-observed after a restart, so anything expressed as an event is something the operator cannot recover from a missed delivery. "The UPS switched to battery" is an event; `ups: on-battery` is state, and it is still true after a controller restart.

**One key answers one question.** Do not build escalating enums. The UniFi provider publishes `ups` and `ups.battery` separately rather than one `online | on-battery | low-battery | critical` ladder, because with a ladder an Automation matching `on-battery` *stops matching* when the battery drops to low — running `onExit` and scaling workloads back up in the middle of a power failure. Split the axes and let users match both keys.

This is the single most important rule on this page, and the one that looks like over-engineering right up until it bites.

**Values are a closed set.** Enumerate them in your constants file. Never pass through a vendor string that could gain a new variant in a firmware update. Lowercase, kebab-case, no spaces.

The UniFi provider breaks this rule exactly once, and the shape of the exception is the useful part. `isp` passes a vendor string through, because the whole point of the key is to name a carrier the operator recognises, and an enum of carriers is not a thing anyone can write down. What it does instead is normalize the string into a slug (lowercase, non-alphanumerics collapsed to hyphens) so the value is always writable in YAML, and reserve one closed value — `unknown` — for "the field is there and says nothing". Break the rule only when the key's *purpose* is to carry an identity you cannot enumerate, and then still guarantee the shape of what you publish.

**Dots namespace a sub-aspect.** `ups.battery` qualifies `ups`. Keep the hierarchy shallow.

**Omit unobservable keys entirely**, as described above.

**Declare the closed value sets, and leave the open ones out.** `StateVocabulary()`
in `state.go` returns key → values for every key whose values you can enumerate.
It is what lets `reactor_state_info` report `0` for the values a key does not
currently hold instead of leaving a stale series at `1`, and — more importantly —
it is what keeps a key with an *open* value set from ever becoming a metric
label. The UniFi provider leaves `isp` out for exactly that reason: one time
series per carrier ever geolocated is how a Prometheus instance gets hurt. If
you break the closed-set rule for a key, break it here too.

**Bucket a measurement before it becomes a key.** The constraint above has a
second edge, and it decides the shape of any key derived from a number rather
than from a switch position. `spec.when` compares strings, so a continuous
value cannot be matched at all; and it has no closed set, so it could never be
declared here. `wan.quality` is the worked example: the console reports
availability as a percentage and latency in milliseconds, and the provider
publishes `good | degraded` against operator-configurable thresholds. The
numbers stay in a debug log line, where they are diagnostics rather than API.
Decide the bucket boundaries with the user's automation in mind — two levels
they can act on beats five they have to reason about.

**A key whose NAME is open is opt-in.** The rule above is about values; this one
is about the other half of the label pair, and it is the harder case. `devices`
and `device.<name>` are the worked example: the aggregate is one key with two
values, while the per-device form derives its *key name* from a device name, so
the set of keys only exists at runtime and grows with someone's rack. That cannot
be enumerated in `StateVocabulary()` — the map is returned once at startup — and
enumerating it would be the wrong thing to want, because one series per device
is a cost the operator has to choose rather than discover.

So: publish the aggregate by default, put the per-entity keys behind a config
flag that defaults off, leave them out of `StateVocabulary()` entirely, and log a
line at startup when they are on. The aggregate is nearly always the key people
should be matching on anyway — "is anything down" is the question; "which one" is
answered by `status.observedState`, an Event, and a `V(1)` log line. Do the same
for `client.<name>` when it lands.

Two details fall out of it. Give the derived name a **slug rule** (the UniFi
provider reuses `isp`'s) so the key is always writable in YAML, and decide what a
**collision** does: two devices whose names slugify alike publish neither key and
count a disagreement, because picking one is arbitrary and the arbitrary pick can
be the one that hides the failure.

The other detail is debounce, and it needs the engine rather than the provider:
a `PerKey` entry may end in `*`, so `device.*: 2` settles a group whose members
are not known when the chart is written. The engine still learns nothing — it
matches a string against a pattern it was handed as data.

**Key names are a compatibility promise.** They appear in user YAML. Renaming one breaks every Automation using it. Choose as if you cannot rename — because you cannot.

## Fixtures: capture, allowlist, commit

Parsers are written against real captured responses, never against assumed formats. This policy is not negotiable and it is not a style preference:

**Capture with an allowlist, never a denylist.** The capture script keeps an explicit list of fields and discards everything else. It does not strip the sensitive fields someone thought of. An earlier version of the UniFi fixtures did exactly that, and a live device credential reached this repository's history as a result.

For a new provider, add a `hack/capture-<provider>.sh` in the same shape as `hack/capture-unifi.sh`: fetch, project down to the allowlisted fields with `jq`, replace the few remaining identifying values with placeholders (documentation-range IPs, fixed dummy MACs and IDs), write into `testdata/<provider>/`. Supporting a new field in your parser means adding it to that allowlist deliberately, one at a time.

**Extend the safety net.** `hack/verify-testdata.sh` runs as part of `make test` and rejects unredacted secret fields, routable IPs, and real MACs. Add your provider's secret-bearing field names to it. The script is the net; the capture script is the mechanism. Never hand-edit a captured response into "safe" — capture it again with the allowlist fixed.

**Write a `testdata/<provider>/README.md`** recording what hardware and what software version produced the captures, which fields each file documents, and which mappings are *inferred* rather than observed. The UniFi one flags that the `wan` mapping has never seen a real failover, and that honesty is worth more than the fixture.

**Consider a mock.** `hack/mock-unifi/` serves the captured payloads over HTTP and exposes endpoints to drive transitions by hand (`POST /flip`, `POST /ups?mode=battery&level=80`, `POST /internet?status=error`, `POST /quality?availability=97`, `POST /device?name=…&state=offline`). It is what makes `make dev-mock` work without hardware, and the e2e suites drive the same endpoints, so a key that cannot be rehearsed by hand also cannot be rehearsed in CI. Do it for any provider whose hardware is not on every contributor's desk.

## Tests

Three layers, all runnable with `make test` and none needing hardware:

- **Derivation tests** against the captured fixtures — the bulk of the work. `internal/providers/unifi/client_test.go` loads real payloads from `testdata/` and asserts the resulting state map, including the cases that encode design decisions: `TestUPSStaysOnBatteryAcrossBatteryLevels` exists to stop anyone collapsing the two UPS keys back into a ladder. Write that kind of test for your own invariants; a comment does not survive a refactor, a test does.
- **Poller tests** — `internal/controller/unifi_poller_test.go` covers the loop with a fake client: transitions reach the store, wakes are enqueued, a full channel does not block.
- **Reconciler tests** — already provider-agnostic. They drive the store directly with synthetic observations, so they cover your provider's semantics for free once your keys are in the store.

## Checklist

- [ ] `internal/providers/<name>/state.go` — `ProviderName`, unexported key and value constants, and `StateVocabulary()` for the keys whose values are a closed set
- [ ] `internal/providers/<name>/client.go` — `Observe(ctx) (map[string]string, error)`, transport split from derivation
- [ ] `internal/providers/<name>/config.go` — `ConfigFromEnv(lookup)`, so the environment mapping is tested rather than reviewed
- [ ] `hack/capture-<name>.sh` — allowlist-first capture, placeholders for anything identifying
- [ ] `testdata/<name>/` fixtures plus a README recording hardware, version, and inferred mappings
- [ ] `hack/verify-testdata.sh` extended with the new provider's secret-bearing fields
- [ ] `internal/controller/<name>_poller.go` — `Runnable`, `NeedLeaderElection() true`, non-blocking wake
- [ ] `cmd/main.go` — construct only when configured, log clearly when disabled, share `store` and `wake`, call `metrics.SetVocabulary`; anything optional fails soft while the poller is added regardless
- [ ] `charts/reactor/values.yaml` and `templates/deployment.yaml` — config values and env, credentials mounted as a directory and pointed at by a `*_FILE` variable
- [ ] Tests at all three layers
- [ ] Docs: the state-key table in `README.md` and `charts/reactor/README.md`
- [ ] `make test` and `make lint` pass; `make manifests generate` output committed

## When you need the engine to change

Some things genuinely belong in the core, and the test is whether the engine would have to learn a provider's vocabulary:

- **Debounce** — requiring N consecutive identical observations before a value is reported. This lives in `StateStore.Observe`, not in your provider, so that every Automation reads the same value. Configuration arrives from the provider as an opaque key → sample-count map; the engine never learns that `ups` should react fast. Supply yours with `engine.WithDebounce(ProviderName, ...)`, and if you also have an inbound trigger, see the `Proving` note above. An entry may end in `*` to cover a group of keys whose names are only known at runtime (`device.*`); exact entries win over patterns, and the longest prefix wins between patterns. That was the one engine change the per-device keys needed, and it is still opaque data — a string matched against a pattern, with no idea what either means.
- **A webhook fast path** — a receiver that triggers an immediate re-observation. It never executes actions and never bypasses the poller; it just makes the next observation happen sooner. This one turned out **not** to need the engine at all: `internal/providers/unifi/receiver.go` is an HTTP `Runnable` that authenticates a delivery, discards its body without reading it, and sends on the poller's `Nudge` channel. Because no state is derived from a payload, a dropped, duplicated, replayed or forged delivery costs at most one extra observation. If your upstream can push, copy that shape rather than parsing what it pushes.

And some things are a different extension point entirely. **Action types are not providers.** A provider observes; an action acts. Adding `kubernetes.restart` or an HTTP action means extending the action side of the reconciler and the `Action` type's enum, and touches none of the above.

There is one case where the two meet, and it is worth knowing the shape of before inventing your own. An action that writes to **the console your provider observes** — `unifi.wlan.*`, `unifi.poe.cycle` — is still an action, and it still goes in the `Action` enum and in `internal/actions`' type list. But its *execution* lives in your provider package, behind the `controller.ConsoleWriter` interface, next to the client that already talks to that hardware. That is what keeps `internal/controller` free of any provider's field names while the credentials, the endpoint knowledge and the install-level "what may be changed here" allowlist stay with the provider that understands them. If you build one: it is an edge action unless you can say where a baseline would live that outlives both the `Automation` and Reactor, and check before you write.

If you find yourself adding a provider name, a key name, or a value string to anything under `internal/engine/`, stop. That is the seam failing, and it is worth an issue rather than a workaround.

## See also

- [Design spec](spec.md) — the state-first rationale in full, and the sketched future providers (NUT, Proxmox, Prometheus, Home Assistant)
- [Development](development.md) — the dev loop, the mock, and the capture policy
- [Captured UniFi payloads](../testdata/unifi/README.md) — the fixtures and what they document
- [Troubleshooting](troubleshooting.md) — what the failure modes look like from the user's side
