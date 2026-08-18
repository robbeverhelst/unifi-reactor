---
title: "Development"
description: "Building, testing and running Reactor against a cluster, with a mock UniFi console that serves the captured payloads and rehearses a failover or a power cut on demand."
---

## Prerequisites

- Go (see `go.mod` for the version)
- Docker, for building the image
- A Kubernetes cluster to test against — bring your own: kind, k3d, minikube, OrbStack, or a real one
- No UniFi hardware required; see [Running without a UDM](#running-without-a-udm)

## Repository layout

```text
api/v1alpha1/          Automation CRD types
internal/engine/       provider-agnostic core: state store, transition detection
internal/events/       normalized Event and Observation models
internal/providers/    provider implementations (currently unifi)
internal/actions/      outbound edge actions: the destination allowlist, the
                       HTTP transport, templating, notification transports
internal/controller/   the reconciler and the UniFi poller
charts/reactor/        Helm chart (`make manifests` regenerates its templated CRD)
hack/mock-unifi/       mock UniFi API serving the captured payloads
hack/dev/              demo Automations used by `make dev-hello`
testdata/unifi/        real captured API responses — the parsers' ground truth
```

`internal/actions/` is provider-agnostic for the same reason the engine is: a notification action must have no idea what `wan` means. It is also where Reactor's outbound reach is bounded — read the package comment before changing anything in it, and [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#outbound-actions) for why the bounds are where they are.

The engine must never contain provider-specific logic. Providers translate vendor reality into normalized state; the engine only ever sees that. Keeping the seam clean is what lets new providers arrive without touching the core — see [Adding a provider](/contributing/adding-a-provider/) for the contract and a walkthrough of the UniFi one.

## Everyday commands

```sh
make test     # unit tests + envtest (controller-runtime)
make lint     # golangci-lint, same config CI runs
make build    # compile the manager binary
make help     # every target
```

CI runs lint, tests, e2e, and a manifest-drift check, so `make manifests generate` output must be committed.

## End-to-end tests

Three suites, each in its own throwaway Kind cluster, each its own CI job:

```sh
make test-e2e        # the manager comes up under the kustomize manifests
make test-reaction   # reactions, restarts, and arbitration against a real API server
make test-lifecycle  # helm uninstall and the upgrade from the crds/ packaging
```

The two new ones install the Helm chart and point it at a rehearsed UniFi console running inside the cluster, then assert on what happened to real workloads: replicas, the `reactor.robbeverhelst.com/*` annotations, `status.targets[]`, and the `Ready` and `Applied` conditions. They cover the things that cannot be reached from a unit test — converging on a state that changed while the operator was down, two Automations arbitrating one workload, and what `helm uninstall` leaves behind.

Each target creates its Kind cluster, runs the suite, and deletes the cluster whether or not it passed. Every `kubectl` and `helm` call inside the suites names its cluster explicitly and refuses to address anything that is not a local Kind context — they install cluster-wide RBAC and delete CRDs, so an unpinned command is not a failed test but an outage.

The suites reach the mock over a fixed node port mapped to the host by `test/e2e/kind-config.yaml`, which is why they create their own clusters rather than reusing one you already have.

### When cluster creation fails on the node port

```text
ERROR: failed to create cluster: ... Bind for 127.0.0.1:30943 failed: port is already allocated
```

That node port is fixed, so only one of these suites can exist at a time — and a suite whose cleanup never ran leaves a cluster holding it. `kind get clusters` finds the orphan; deleting it frees the port. The same message also appears for a few seconds after a cluster is deleted, while Docker still holds the reservation, so a rerun that fails immediately after a teardown is worth trying once more before hunting for a cause.

A suite takes a few minutes and is worth capturing to a file. If your shell sets `noclobber` — zsh does under many dotfile setups — `> run.log` **refuses to overwrite an existing file** and the run never starts, while the previous run's log sits there looking like a fresh failure. Use `>|`, or delete the file first.

`make manifests` also regenerates `charts/reactor/templates/crds.yaml` via `hack/sync-chart-crds.sh`. The CRD is a chart *template* deliberately: Helm installs a chart's `crds/` directory on first install only and never upgrades it, so every later schema change would ship silently broken. Don't hand-edit the chart's copy — the tests in `test/chart/` fail when it drifts from `config/crd/bases`, and they need `helm` on your PATH to run at all.

## Running against a cluster

Targets act on `DEV_CONTEXT`, which defaults to your current kubectl context. **Pass it explicitly** — these targets install an operator with cluster-wide RBAC, and the current context can change between two commands in the same target.

```sh
make dev-context DEV_CONTEXT=kind-reactor   # print what you're about to hit

make dev-deploy \
  DEV_CONTEXT=kind-reactor \
  UNIFI_URL=https://192.168.1.1 \
  UNIFI_API_KEY=<key>

make dev-hello DEV_CONTEXT=kind-reactor     # demo workloads + Automations
make dev-clean DEV_CONTEXT=kind-reactor     # remove it all
```

`dev-deploy` builds the image, creates the credentials Secret, and installs the local chart. Local images work on clusters that share the host Docker daemon (OrbStack, k3d with the shared registry); with kind, load the image first via `kind load docker-image`.

## Running without a UDM

`make dev-mock` serves the captured payloads from `testdata/` on `:9443` and lets you drive state transitions by hand:

```sh
make dev-mock

curl -X POST http://localhost:9443/flip                        # WAN primary <-> backup
curl -X POST 'http://localhost:9443/ups?mode=battery&level=80' # power outage
curl -X POST 'http://localhost:9443/ups?level=5'               # battery critical
curl -X POST 'http://localhost:9443/ups?mode=mains&level=100'  # power restored
curl -X POST 'http://localhost:9443/ups?present=false'         # UPS drops off the console
curl -X POST 'http://localhost:9443/ups?runtime=150'           # minutes of runtime left
curl -X POST 'http://localhost:9443/ups?runtime=0'             # the UPS offers no estimate
curl -X POST 'http://localhost:9443/ups?output=850'            # a heavy load on the same budget

curl -X POST 'http://localhost:9443/internet?status=error'     # no internet, link unchanged
curl -X POST 'http://localhost:9443/internet?present=false'    # the www subsystem vanishes
curl -X POST 'http://localhost:9443/quality?availability=97'   # the live uplink gets flaky
curl -X POST 'http://localhost:9443/quality?latency=400'       # ...or just slow
curl -X POST 'http://localhost:9443/quality?reset=true'        # back to the capture

curl http://localhost:9443/device                              # what the capture holds, and each device's key
curl -X POST 'http://localhost:9443/device?name=ups-2u&state=offline'    # a device dies
curl -X POST 'http://localhost:9443/device?name=ups-2u&rename=Rack+UPS'  # ...or is renamed
curl -X POST 'http://localhost:9443/device?reset=true'         # back to the capture

curl -X POST 'http://localhost:9443/wifi?disconnected=1'       # one access point drops
curl -X POST 'http://localhost:9443/wifi?disconnected=3'       # all of them: wifi error

curl -X POST 'http://localhost:9443/firmware?upgradable=true'  # an update is waiting
curl -X POST 'http://localhost:9443/temperature?celsius=82'    # a device runs hot
curl -X POST 'http://localhost:9443/poe?watts=55&budget=60'    # the PoE budget fills up
curl -X POST 'http://localhost:9443/poe?silent=true'           # a powered port reports no wattage
curl -X POST 'http://localhost:9443/poe?port=7&name=re-patched' # ...and the write path's identity check

curl http://localhost:9443/outlets                             # outlets, banks, and every write Reactor made
curl -X POST 'http://localhost:9443/outlets?outlet=5&state=off'          # one outlet opens
curl -X POST 'http://localhost:9443/outlets?switching=group&outlet=5&state=off'   # ...and takes 5-8 with it
curl -X POST 'http://localhost:9443/outlets?outlet=5&label=nas'          # key becomes outlet.nas
curl -X POST 'http://localhost:9443/outlets?caps=false'        # ...and the write path's battery-bank floor
curl -X POST 'http://localhost:9443/outlets?overrides=false'   # nothing for the write to modify
curl -X POST 'http://localhost:9443/outlets?reset=true'        # back to the capture
```

`/poe` drives both halves of the PoE story, because the mock has one synthetic switch and one `port_table` and they are the same one: `watts`/`budget`/`silent` move what the `poe` **state key** measures, while `port`/`name`/`uplink`/`poe` break the identity checks the `unifi.poe.cycle` **action** makes. That switch is adopted and online, so it is part of the fleet `devices` counts and is addressable as `mock-switch` through `/device`.

> **`/firmware`, `/temperature` and `/poe` serve fields no capture contains.** The committed records carry no upgrade flags, no thermals and no `port_table`, so those three endpoints render the shape UniFi's API *documents* — including `poe_power` as a string, which is the form most likely to break a parser. Driving them exercises the derivation; it does not confirm a console reports any of it. Until one does, the mock's honest default is what the captures show: those keys are simply absent, and `present=false` puts each back to that state. See [the capture notes](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#what-is-not-captured-yet).

Per-device keys are opt-in in Reactor (`unifi.devices.perDeviceKeys`), so `device.<name>` will not appear until you ask for it — `devices` is published either way. A device is addressed by the slug of the name it was *captured* under even after `rename=`, which is what makes the rename rehearsal reversible: renaming makes the old key **vanish**, and the reconciler holds the last known state rather than treating it as a recovery.

`/outlets` drives both halves of the outlet story the way `/poe` does. Most of what it serves **is** captured — `index`, `name`, `relay_state` and `relay_group` are all in `stat-device-ups.json` — while `_id`, `outlet_caps` and `outlet_overrides` are the shape read off the real UPS on 2026-08-15, when the write that unblocked [#23](https://github.com/robbeverhelst/unifi-reactor/issues/23) was made. `switching=` still imitates both answers to "does this UPS move an outlet or a bank": the real one moved one outlet and left its three group siblings on, but a parser that had only ever seen the answer it expected is a parser tested against one device.

`caps=false` and `overrides=false` break the two floors the `unifi.outlet.*` **actions** stand on — an outlet whose bank Reactor cannot read is refused whatever the allowlist says, and a UPS with no `outlet_overrides` gives the write nothing to modify and Reactor will not compose one. `outlet=5&label=nas` is the other thing worth rehearsing: it names an outlet, which both moves the state key off the index *and* is what makes the outlet switchable at all, since one still called `Outlet 5` is refused.

`PUT rest/device/<id>` is the endpoint Reactor actually calls, and the mock enforces the one thing a real console cannot: that the body is the `outlet_overrides` array just read with **exactly one** `relay_state` changed. `curl http://localhost:9443/outlets` lists every write it accepted, so "did Reactor cut the right socket" is one request rather than a log search.

`/wifi` drives the `wlan` subsystem's AP counts, because that is what `wifi` is derived from. `?status=` moves the console's own wording *without* moving the counts, which is how you rehearse the disagreement Reactor reports rather than silently resolving.

`present=false` removes the UPS from the device list rather than reporting a value for it, so the `ups` keys vanish entirely. That is the case an Automation has to distinguish from "the outage ended", and the one the reconciler answers with `StateKeyUnavailable`. `/internet?present=false` does the same for the `www` health subsystem.

`runtime=0` is the narrower version of the same idea: the UPS is still there and still reports charge, but offers no runtime estimate, so `ups.runtime` alone disappears while `ups` and `ups.battery` stay. Per-key degradation is meant to be that granular.

`/internet` is the rehearsal you cannot reach through `/flip` or `/wan` at all, and that is the point of the key: the link stays up, the uplink is unchanged, and there is no internet. `/quality` drives the live uplink's `uptime_stats` — availability as a percentage and latency in milliseconds, which on real hardware are averages over the console's 24-hour uptime window and here move instantly. Both follow whichever uplink `/wan` says is live.

> The statuses `/internet` will serve — `warning` and `error`, which map to `degraded` and `down` — have never been seen on a real console's `www` subsystem. Rehearsing them shows what Reactor does with them; it does not confirm a console ever sends them. See [the capture notes](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#internet-reachability-and-link-quality-stathealth).

### Rehearsing failovers, including the shapes not yet observed

`/flip` moves every WAN signal at once, which is the `clean` shape below. The one failover observed on real hardware ([#34](https://github.com/robbeverhelst/unifi-reactor/issues/34)) did not look like that: it was a failover to a cellular backup, whose record carries no `is_uplink` at all, so `is_uplink` named nobody and the uplink-interface fallback resolved the key — the `no-uplink` shape. A wired-to-wired failover has still not been observed, so the mock renders every plausible shape — because a parser tested against one hypothesis is only tested against one hypothesis:

```sh
curl http://localhost:9443/wan          # current state, and what each variant means

curl -X POST 'http://localhost:9443/wan?link=backup&variant=is-uplink-pinned'
curl -X POST 'http://localhost:9443/wan?link=primary'
```

| Variant | What it says a failover looks like | What Reactor should do |
| --- | --- | --- |
| `clean` | every signal moves together | report `wan: backup`, quietly |
| `is-uplink-only` | only `is_uplink` moves | report `backup`, and log that `uplink.name` disagrees |
| `is-uplink-pinned` | `is_uplink` means "configured as primary" and never moves | report `primary` **through a failover** — the silent failure, so it logs loudly instead |
| `both-uplinks` | both ports claim `is_uplink` | fall back to `uplink.name` rather than guessing |
| `no-uplink` | neither claims it, mid-switchover | fall back to `uplink.name` instead of dropping the key |

Add `&isp=<name>` to rehearse the carrier changing too; the default is an obviously synthetic one, because the real backup carrier has never been seen.

The same hypotheses are asserted in `internal/providers/unifi/wan_test.go`, derived from the committed capture in code. Neither the mock nor those tests produce a fixture: settling which hypothesis is real for a wired-to-wired failover needs hardware, and the [capture runbook](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md#capturing-a-real-failover) is the procedure for it.

Point the operator at it with `UNIFI_URL=http://<your-host>:9443 UNIFI_API_KEY=mock`. Use a LAN address rather than `localhost` so the pod can reach your machine.

The mock also answers the Integration API's `info` endpoint, which is what Reactor's compatibility guard reads at startup. It serves the captured version by default; pass a different one to rehearse the warning without owning the hardware that would produce it:

```sh
go run ./hack/mock-unifi -network-version 11.0.0
# INFO This UniFi Network version is newer than anything Reactor has been tested against ...
```

## Webhook fast path

The receiver turns a UniFi Alarm Manager delivery into an immediate re-observation. It is off by default, and everything about it degrades to poll-only:

```sh
make dev-deploy DEV_CONTEXT=kind-reactor \
  UNIFI_URL=http://<your-host>:9443 UNIFI_API_KEY=mock \
  HELM_EXTRA_ARGS="--set unifi.webhook.enabled=true"
```

The receiver needs a Secret with the shared secret it will demand from every delivery:

```sh
kubectl -n reactor-system create secret generic unifi-reactor-webhook \
  --from-literal=UNIFI_WEBHOOK_TOKEN="$(openssl rand -hex 32)"
```

Send a delivery by hand — the payload is never read, so any body will do:

```sh
curl -X POST http://<receiver>:9090/webhooks/unifi \
  -H 'Authorization: Bearer <token>' \
  -H 'Content-Type: application/json' \
  -d @hack/dev/webhook-delivery.json
```

The logs show the delivery and the observation it caused, in that order:

```sh
kubectl -n reactor-system logs deploy/reactor | grep -E 'webhook delivery|state observed'
```

`hack/dev/webhook-delivery.json` is **synthetic**, not a capture. Nothing in Reactor parses a delivery, so a stand-in is enough to drive the path.

### Rehearsing self-registration

`make dev-mock` also mocks the Alarm Manager API, so the registration path can be exercised without touching a real console. Run the mock, start Reactor with `unifi.webhook.registration.enabled=true`, and then have the mock fire a delivery at whatever rule Reactor registered:

```sh
make dev-webhook
```

The mock's alarm responses are built from the [Alarm Manager API notes](/contributing/unifi-alarm-manager-api/), not captured from a console. Registration succeeding against the mock proves Reactor sends what those notes describe. It does not prove a real console accepts it.

### Capturing real deliveries

`hack/webhook-logger.mjs` dumps incoming requests verbatim to `testdata/unifi/webhooks/raw/` (gitignored). Raw records contain every header, including the `Authorization` header carrying Reactor's own shared secret, so nothing goes from there into `testdata/` by hand:

```sh
node hack/webhook-logger.mjs 8080

./hack/sanitize-webhook-capture.sh --paths testdata/unifi/webhooks/raw/<file>.json
./hack/sanitize-webhook-capture.sh testdata/unifi/webhooks/raw/<file>.json \
  internet-disconnected alarm.trigger,alarm.title
```

The first command prints every field path in the body; the second keeps the ones you name and discards the rest, along with every header but `content-type`. Same allowlist discipline as `hack/capture-unifi.sh`, and `hack/verify-testdata.sh` rejects leftover credential material in `testdata/unifi/webhooks/` as a safety net.

## Captured payloads

Parsers are written and tested against real responses in [`testdata/unifi/`](https://github.com/robbeverhelst/unifi-reactor/blob/main/testdata/unifi/README.md), never against assumed formats. Capture them with:

```sh
UNIFI_URL=https://192.168.1.1 UNIFI_API_KEY=<key> ./hack/capture-unifi.sh
```

The script **keeps an explicit allowlist of fields and discards everything else**, then replaces the few remaining sensitive values with placeholders. Supporting a new field means adding it to the allowlist in that script, deliberately.

This is allowlist rather than denylist for a reason: `stat/device` returns whole device records containing management keys, syslog keys, and adoption identifiers, and the parser needs a dozen fields out of hundreds. An earlier version of these fixtures stripped the sensitive fields someone thought of instead of keeping only the needed ones, and a live credential reached this repository's history as a result.

`make test` runs `hack/verify-testdata.sh`, which rejects unredacted secret fields, routable IPs, real MACs, and carrier fields that are not the placeholder. That is the safety net; the capture script is the mechanism.

`hack/webhook-logger.mjs` dumps incoming webhook deliveries verbatim to `testdata/unifi/webhooks/raw/` (gitignored) when capturing from a real Alarm Manager. Apply the same allowlist discipline before committing any of it.

## Metrics

`make dev-deploy` leaves the metrics endpoint off, the same as a chart install.
Turn it on and read it without a Prometheus:

```sh
make dev-deploy DEV_CONTEXT=kind-reactor \
  UNIFI_URL=http://<your-host>:9443 UNIFI_API_KEY=mock \
  HELM_EXTRA_ARGS="--set metrics.enabled=true --set metrics.secure=false"

kubectl -n reactor-system port-forward deploy/reactor 8443:8443
curl -s localhost:8443/metrics | grep '^reactor_'
```

`metrics.secure=false` is a development convenience: the real posture is HTTPS
behind the API server's authn/authz filter, and a scrape needs a bearer token.

Driving `make dev-mock` is the fastest way to see the decision-layer series
move. `POST /flip` produces a `reactor_state_transitions_total` increment, a
`reactor_state_info` flip, an action, and a `reactor_reaction_latency_seconds`
observation, in that order.

New metrics go in `internal/metrics/`, never in a controller or a provider —
the definitions live in one file so the label decisions are reviewable in one
place. Read the package comment before adding a label: the rule is that a label
whose value set comes from the outside world does not go in. `reactor_state_info`
enforces that structurally, by publishing only the keys whose provider declared
a closed value set via `StateVocabulary`.

## Releasing

Releases are cut entirely by CI from a tag; nothing is published from a developer machine.

```sh
git tag v0.3.0 && git push origin v0.3.0
```

That builds the multi-arch image, packages the chart with `version`/`appVersion` taken from the tag, pushes both to GHCR, and attaches `install.yaml` to a GitHub Release with generated notes. Image and chart versions always move together.

Both artifacts are signed by cosign keyless signing, using the workflow's OIDC token — nothing to configure, no key anywhere. The image also gets an SBOM and build provenance. [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md) has the `cosign verify` invocations.

Use conventional commits (`feat:`, `fix:`, `docs:`, …) — they drive the generated release notes.
