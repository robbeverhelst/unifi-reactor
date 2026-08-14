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

## Outbound actions

`http.request`, `notification.*` and the named integrations (`homeassistant.service`) all leave the cluster. They change the operator's exposure more than anything else it does, so the reasoning is written down rather than left implicit.

**They are one client, deliberately.** Every controlled property below — the allowlist, the address floor, the redirect refusal, the origin-only reporting, the Secret rules — is a property of a single outbound HTTP client in `internal/actions`, not of any action type. A named integration is a *shape* over that client: it decides what the URL and the body are, and inherits everything else. A second HTTP client anywhere in this repository would be a security regression by construction, because it would be a second place for each of these to be got wrong.

**The threat.** An operator that will issue HTTP requests on demand is a confused deputy. It runs inside the cluster with a ServiceAccount and a network position — it can reach `ClusterIP` Services, the API server, node-local endpoints and your LAN — and `spec.actions` is writable by anyone who can create an `Automation` in their own namespace. Without controls, "create an Automation" would become "make a privileged request from inside the cluster and, via the response or the body, get something back out."

**What follows from that.**

- **Destinations are the operator's decision, not the Automation's.** `actions.allowedDestinations` is a Helm value, empty by default, and empty means every outbound action is refused with a reason naming the destination to add. There is no per-Automation override and no way to widen it from a namespace. `*` is available and is an explicit choice to run without this control.
- **A floor applies whatever is allowlisted.** The loopback interface and link-local addresses (`169.254.0.0/16`, `fe80::/10` — where cloud instance metadata services and the credentials they hand out live) are refused, as are the unspecified and multicast addresses. This is enforced in the dialer, on the address actually connected to, so a hostname that resolves somewhere other than it appears to — deliberately, as in DNS rebinding, or by accident — is refused too. **Redirects are never followed**, because a redirect names a destination the allowlist never approved.
- **Private ranges are *not* blocked.** An ntfy box on the LAN is a first-class destination for a homelab, and no address rule can tell that apart from a `ClusterIP` Service. The allowlist is what draws that line, which is why it is default-deny rather than default-allow.
- **A named integration narrows the request; it never widens it.** `homeassistant.service` builds its path from a `domain` and a `service`, each restricted to a bare slug at admission and again when the URL is built, so it cannot address anything on the allowed host other than a service call. That is strictly less reach than the `http.request` the same allowlist entry already permits — the point is that the action *says* what it is, so an allowlist entry for a Home Assistant instance does not have to be read as "and anything else on that host". It is not a substitute for the allowlist and does not relax it.
- **State can reach the body, and nothing else.** `title`, `message`, `body` and a service call's `data` are templated; the URL and the headers are literal. Templating the destination would hand back the choice the allowlist exists to take away, and templating a header would invite building a credential out of state. The template context is deliberately limited to the Automation's own identity and the provider state it named in `spec.when` — so it can carry nothing its author does not already have, to a host the operator already approved. There is no function, and no field, that reaches a Secret, the environment, the filesystem, another Automation, or provider state this Automation did not ask for.

  `isp` is the first state key whose values are an open set rather than an enum, so it is the first case where the *content* of a rendered body is not drawn from a list Reactor wrote. That does not change the argument above — an `isp` value is still state this Automation named in `spec.when`, going to a host the operator allowed — but it does mean the handling has to be safe for an arbitrary string rather than for a known one, and it is: notification bodies are built with `encoding/json` rather than formatted, the `json` template function is there so a value can be embedded in an `http.request` body without hand-quoting it, a value travelling in a header (ntfy's title) is reduced to printable ASCII so it cannot split a request, and the rendered result is length-capped. None of that relies on the provider having sanitized the value first, which is the right way round: the provider's slugging is a nicety, not a control.
- **Credentials come from Secrets, in the Automation's own namespace only.** There is no namespace field on `secretRef` and no inline credential field: an inline one would be readable by everyone who can read the Automation, and a cross-namespace read would let a namespace tenant borrow Reactor's cluster-wide reach to fetch a credential they cannot read themselves. Anyone who can write an Automation can already write a Secret beside it, so nothing is lost.
- **The Secret read is opt-in and uncached.** `get` on Secrets is granted only when `actions.allowedDestinations` is non-empty, so an install that does not use the feature never gives the operator read access to Secrets at all. Reads go through an uncached client, because a cached `Get` on a Secret would start an informer and hold every Secret in the cluster in the operator's memory for the life of the process.
- **A destination is reported as `scheme://host:port` and never more.** For every notification transport shipped, the URL *is* the credential and the secret part of it is the path — so the path, the query and any user information are stripped from every log line, status field, `Event` and error, including the errors `net/http` raises, which quote the full URL by default. Response bodies are read only to allow connection reuse and are never recorded, because a response can echo a request back.
- **Sending is bounded.** One attempt is bounded by `timeoutSeconds`, an action retries at most three times and only when repeating it is known to be harmless, and all the edge actions on one transition share a one-minute budget. A rendered template is capped, so a body cannot be grown into a bulk transfer out of the cluster.

**Residual risk, stated plainly.** With a destination allowlisted, anyone who can create an `Automation` in any namespace can cause requests to that destination carrying provider state, and can use a credential Secret they can already read. If the allowlisted destination is an in-cluster Service, that reachability is real and intended — this is why the list is yours to write. Reactor's own ServiceAccount token is never attached to an outbound request.

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
