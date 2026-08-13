# Development

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
internal/controller/   the reconciler and the UniFi poller
charts/reactor/        Helm chart (`make manifests` regenerates its templated CRD)
hack/mock-unifi/       mock UniFi API serving the captured payloads
hack/dev/              demo Automations used by `make dev-hello`
testdata/unifi/        real captured API responses — the parsers' ground truth
```

The engine must never contain provider-specific logic. Providers translate vendor reality into normalized state; the engine only ever sees that. Keeping the seam clean is what lets new providers arrive without touching the core.

## Everyday commands

```sh
make test     # unit tests + envtest (controller-runtime)
make lint     # golangci-lint, same config CI runs
make build    # compile the manager binary
make help     # every target
```

CI runs lint, tests, e2e, and a manifest-drift check, so `make manifests generate` output must be committed.

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
```

Point the operator at it with `UNIFI_URL=http://<your-host>:9443 UNIFI_API_KEY=mock`. Use a LAN address rather than `localhost` so the pod can reach your machine.

## Captured payloads

Parsers are written and tested against real responses in [`testdata/unifi/`](../testdata/unifi/README.md), never against assumed formats. Capture them with:

```sh
UNIFI_URL=https://192.168.1.1 UNIFI_API_KEY=<key> ./hack/capture-unifi.sh
```

The script **keeps an explicit allowlist of fields and discards everything else**, then replaces the few remaining sensitive values with placeholders. Supporting a new field means adding it to the allowlist in that script, deliberately.

This is allowlist rather than denylist for a reason: `stat/device` returns whole device records containing management keys, syslog keys, and adoption identifiers, and the parser needs a dozen fields out of hundreds. An earlier version of these fixtures stripped the sensitive fields someone thought of instead of keeping only the needed ones, and a live credential reached this repository's history as a result.

`make test` runs `hack/verify-testdata.sh`, which rejects unredacted secret fields, routable IPs, and real MACs. That is the safety net; the capture script is the mechanism.

`hack/webhook-logger.mjs` dumps incoming webhook deliveries verbatim to `testdata/unifi/webhooks/raw/` (gitignored) when capturing from a real Alarm Manager. Apply the same allowlist discipline before committing any of it.

## Releasing

Releases are cut entirely by CI from a tag; nothing is published from a developer machine.

```sh
git tag v0.3.0 && git push origin v0.3.0
```

That builds the multi-arch image, packages the chart with `version`/`appVersion` taken from the tag, pushes both to GHCR, and attaches `install.yaml` to a GitHub Release with generated notes. Image and chart versions always move together.

Use conventional commits (`feat:`, `fix:`, `docs:`, …) — they drive the generated release notes.
