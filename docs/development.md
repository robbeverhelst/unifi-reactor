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
charts/reactor/        Helm chart (CRDs are synced here by `make manifests`)
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

Parsers are written and tested against real responses in [`testdata/unifi/`](../testdata/unifi/README.md), never against assumed formats. When adding support for new hardware or a new state key:

1. Capture the real response from a live console.
2. Sanitize it — public IPs, MAC addresses, serials, site IDs, and anything under `x_` (auth keys) must go. Field *structure* stays untouched.
3. Commit it to `testdata/unifi/api/` and document the fields in that directory's README.
4. Write the parser against the committed file.

`hack/webhook-logger.mjs` dumps incoming webhook deliveries verbatim to `testdata/unifi/webhooks/raw/` (gitignored) when capturing from a real Alarm Manager.

## Releasing

Releases are cut entirely by CI from a tag; nothing is published from a developer machine.

```sh
git tag v0.3.0 && git push origin v0.3.0
```

That builds the multi-arch image, packages the chart with `version`/`appVersion` taken from the tag, pushes both to GHCR, and attaches `install.yaml` to a GitHub Release with generated notes. Image and chart versions always move together.

Use conventional commits (`feat:`, `fix:`, `docs:`, …) — they drive the generated release notes.
