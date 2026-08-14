# Contributing

Thanks for helping. This is a small project with a few unusual rules — mainly around test fixtures — so it is worth reading this page before your first PR rather than discovering them in review.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## The dev loop

```sh
make test          # unit tests + envtest (controller-runtime), plus fixture verification
make lint          # golangci-lint, same config CI runs
make build         # compile the manager binary
make help          # every target
```

Both `make test` and `make lint` must pass before a PR is ready. CI runs lint, tests, three e2e suites, and a manifest-drift check. The e2e suites (`make test-reaction`, `make test-lifecycle`, `make test-e2e`) each create and delete their own Kind cluster; see [Development](https://reactor.robbeverhelst.com/contributing/development/#end-to-end-tests) for what each one covers. The chart tests in `test/chart/` need `helm` on your PATH to run at all — they render the chart and check, among other things, that its templated CRD has not drifted from `config/crd/bases`.

**No UniFi hardware is required.** `make dev-mock` serves the captured payloads on `:9443` and lets you drive state transitions by hand:

```sh
make dev-mock

curl -X POST http://localhost:9443/flip                        # WAN primary <-> backup
curl -X POST 'http://localhost:9443/ups?mode=battery&level=80' # power outage
curl -X POST 'http://localhost:9443/ups?level=5'               # battery critical
curl -X POST 'http://localhost:9443/ups?mode=mains&level=100'  # power restored
```

Point an operator at it with `UNIFI_URL=http://<your-lan-address>:9443 UNIFI_API_KEY=mock`. [Development](https://reactor.robbeverhelst.com/contributing/development/) has the full setup, including running against a real cluster with `make dev-deploy`.

## Generated files must be committed

Editing `api/v1alpha1/*_types.go` or any kubebuilder marker changes generated output:

```sh
make manifests generate
```

CI fails on drift, so commit whatever those produce. Never hand-edit `config/crd/bases/*`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `PROJECT`, or `charts/reactor/templates/crds.yaml` — they are regenerated. The chart's CRD is a *template* rather than a file under `crds/` because Helm installs `crds/` on first install only and never upgrades it; `make manifests` syncs it via `hack/sync-chart-crds.sh`.

## Test fixtures: capture with an allowlist, never by hand

**This is the rule nobody guesses, and the one with real consequences.**

Parsers are written and tested against real captured API responses committed under `testdata/`, never against assumed formats. Those captures come from exactly one place:

```sh
UNIFI_URL=https://192.0.2.10 UNIFI_API_KEY=<key> ./hack/capture-unifi.sh
```

That script **keeps an explicit allowlist of fields and discards everything else**, then replaces the few remaining identifying values with placeholders. Supporting a new field in a parser means adding it to the allowlist in that script, deliberately, one at a time.

Why it is an allowlist and not a denylist: `stat/device` returns whole device records — roughly 8 KB each, carrying device management keys (`x_authkey`), syslog keys, adoption identifiers, and full topology tables. The parser needs about a dozen fields. An earlier version of these fixtures was produced by *removing* the sensitive fields someone thought of rather than *keeping* only the needed ones, and a live credential reached this repository's history as a result.

So, concretely:

- **Never paste a raw API response into a test, a fixture, or an issue.** Not "just this once", not with a couple of fields blanked out.
- **Never hand-edit a committed capture** to look sanitized. Fix the allowlist and capture again.
- `hack/verify-testdata.sh` runs as part of `make test` and rejects unredacted secret fields, routable IPs, and real MACs. It is the safety net, not the mechanism — passing it is not evidence that a hand-made fixture is safe.
- Webhook captures follow the same discipline. `hack/webhook-logger.mjs` writes deliveries verbatim to a gitignored directory; allowlist them before committing anything.

[`testdata/unifi/README.md`](testdata/unifi/README.md) documents what each fixture contains and which mappings are inferred rather than observed.

## Keep the engine provider-agnostic

`internal/engine/` must never contain provider-specific logic — no provider names, no state key names, no vendor value strings. Providers translate vendor reality into a normalized state map; the engine only ever sees that map. That seam is the project's central architectural claim.

If you are adding a provider, [Adding a provider](https://reactor.robbeverhelst.com/contributing/adding-a-provider/) walks the whole contract through the UniFi implementation.

## Commits and PRs

**Conventional commits**, because they drive the generated release notes:

```text
feat: add kubernetes.restart action
fix(unifi): hold state when the UPS drops off the controller
docs: document the capture policy
chore(deps): bump controller-runtime
```

Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`. Scope is optional; `unifi`, `engine`, `chart`, and `testdata` are the usual ones. Mark a breaking change with `!` (`feat!:`) and explain the migration in the PR body — the API is `v1alpha1` and pre-1.0, so breaking changes are allowed, but they are never silent.

For the PR itself:

- One logical change per PR, and a commit per issue when a PR closes more than one.
- Reference the issue (`Closes #12`).
- Say what you could not verify. Documentation about hardware you do not own, or behaviour you could not reproduce, is welcome — flag the specific claims so a reviewer knows what to check.
- Behaviour changes need a test. `internal/providers/unifi/client_test.go` has the pattern for fixture-driven tests, several of which exist specifically to stop a design decision being refactored away.

## Reporting bugs

Use the [bug report template](https://github.com/robbeverhelst/unifi-reactor/issues/new/choose). It asks for the UniFi Network version, console model, chart version, and `kubectl get automation -o yaml` — those four are what makes anything reproducible, and every one of them is missing from the average report.

Check the [troubleshooting guide](https://reactor.robbeverhelst.com/troubleshooting/) first; several of the most common reports are documented behaviour with a documented fix.

**Redact before posting.** Logs and resource dumps can carry your public IP, your ISP, internal hostnames, and site identifiers. Nothing in a bug report needs them.

**Security problems do not go in a public issue.** [SECURITY.md](SECURITY.md) has the reporting route and what is in scope — Reactor holds a credential to your network infrastructure and, by default, cluster-wide permission to patch Deployments, so anything widening that reach belongs there rather than here.

## Releases

Releases are cut entirely by CI from a tag; nothing is published from a developer machine. Tagging `vX.Y.Z` builds the multi-arch image, packages the chart, and attaches `install.yaml` to a GitHub Release with generated notes. See [CHANGELOG.md](CHANGELOG.md) for how release notes work.
