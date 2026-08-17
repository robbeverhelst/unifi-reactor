---
title: "Contributing to UniFi Reactor"
description: "The short version of the dev loop — make test, make lint, and a mock console that rehearses a WAN failover without hardware — plus the fixture capture policy you would not guess."
---

PRs welcome — [CONTRIBUTING.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/.github/CONTRIBUTING.md) has the full version, including the fixture capture policy, which is a genuinely unusual rule and not one you would guess. The short version:

```sh
make test          # unit + envtest
make lint          # golangci-lint
make dev-deploy DEV_CONTEXT=<your-cluster> UNIFI_URL=... UNIFI_API_KEY=...
```

No UniFi hardware needed — `make dev-mock` serves the captured payloads and rehearses a WAN failover or a power outage on demand. Conventional commits; tagging `vX.Y.Z` builds and publishes the multi-arch image, the OCI chart, and `install.yaml` from CI, with [generated release notes standing in for a changelog](https://github.com/robbeverhelst/unifi-reactor/blob/main/CHANGELOG.md).

Bug reports go through the [issue templates](https://github.com/robbeverhelst/unifi-reactor/issues/new/choose), which ask for the four things that make a report reproducible. Participation is covered by the [Code of Conduct](https://github.com/robbeverhelst/unifi-reactor/blob/main/.github/CODE_OF_CONDUCT.md).
