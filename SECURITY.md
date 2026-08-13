# Security Policy

## Reporting a vulnerability

Report privately through GitHub: **[open a security advisory](https://github.com/robbeverhelst/unifi-reactor/security/advisories/new)** (Security → Advisories → Report a vulnerability). Please don't open a public issue for something exploitable.

Useful in a report: what an attacker gains, the version or commit, and the smallest sequence that shows the problem.

This is a personally maintained project, not a vendor product. Expect an acknowledgement within a week and an honest answer about whether and when a fix will land. Fixes ship in a normal release with the advisory published alongside it. There is no bounty programme.

## What is worth reporting

Reactor runs with a credential to network infrastructure and, by default, cluster-wide permission to patch Deployments. Anything that widens that reach is in scope:

- exposure of the UniFi API key — in logs, in status, in events, or through the metrics endpoint
- an `Automation` acting outside the RBAC mode it was installed with, for example a namespaced install patching another namespace
- privilege escalation from `Automation` contents, since anyone who can create one can already scale workloads
- supply-chain problems in the published image or chart: unexpected contents, a signature that verifies when it should not

Out of scope: `unifi.insecureSkipVerify: true`, which is the documented default because UniFi OS consoles serve a self-signed certificate on the LAN; and the operator being able to scale the workloads its RBAC allows, which is the entire point.

## Supported versions

The project is pre-1.0. Fixes land on the latest release only; there are no maintained release branches.

## Verifying a release

Images and charts are signed with [cosign](https://docs.sigstore.dev/) keyless signing from the release workflow — there is no key to steal, and the signature records the workflow and tag that produced the artifact. Signing starts with the first release after this policy was added; v0.3.0 and earlier are unsigned.

```bash
IDENTITY='^https://github.com/robbeverhelst/unifi-reactor/.github/workflows/release.yaml@refs/tags/v.*$'
ISSUER=https://token.actions.githubusercontent.com

# the image, at the tag you are running
cosign verify ghcr.io/robbeverhelst/unifi-reactor:<vX.Y.Z> \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"

# the chart, which is versioned without the leading v
cosign verify ghcr.io/robbeverhelst/charts/reactor:<X.Y.Z> \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
```

The image also carries an SBOM and full build provenance as attestations:

```bash
docker buildx imagetools inspect ghcr.io/robbeverhelst/unifi-reactor:<vX.Y.Z> \
  --format '{{ json .Provenance }}'
```
