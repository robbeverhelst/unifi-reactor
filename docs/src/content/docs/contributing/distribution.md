---
title: "Distribution"
description: "Where UniFi Reactor is published and what is automated: the Artifact Hub listing, why the chart carries no signKey, and which community lists it does and does not qualify for."
---

Where this project is published, what is automated, and what is left for a human to do. Everything here
is a fact about the repository or a criterion quoted from somebody else's contribution rules — where a
project does not meet one, that is written down rather than worked around.

- [Artifact Hub](#artifact-hub) — the chart listing, and the two steps only the maintainer can do
- [Why there is no `signKey`](#why-there-is-no-signkey)
- [OperatorHub.io](#operatorhubio) — scoped, and deliberately not started
- [Lists](#lists) — what each one requires, and when this project qualifies
- [Community posts](#community-posts) — drafted, unposted
- [Things only the maintainer can do](#things-only-the-maintainer-can-do)

## Artifact Hub

The chart is published to an OCI registry, so nothing about the listing works the way the HTTP-repository
documentation describes. Two things follow from that and both are already in the repository.

**The chart metadata** is in [`charts/reactor/Chart.yaml`](https://github.com/robbeverhelst/unifi-reactor/blob/main/charts/reactor/Chart.yaml) as
`artifacthub.io/*` annotations: the license and category, `operator: "true"`, the links, and the
`Automation` CRD together with two working examples — pausing downloads on a backup uplink, and shedding
load when the UPS goes to battery. Artifact Hub renders each CRD as a card and each example as something
you can open from it. The examples are the ones from the README, so there is one shape to keep right
rather than two.

**`artifacthub.io/changes` is not in that file.** It describes a single release, so a copy of it in git
would be stale the moment the next tag is cut — the failure [CHANGELOG.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/CHANGELOG.md) refuses to
sign up for anywhere else in this repository. It is generated at package time instead, by
[`hack/artifacthub-changes.sh`](https://github.com/robbeverhelst/unifi-reactor/blob/main/hack/artifacthub-changes.sh), from the commits between the tag being
released and the one before it. That is the same source the GitHub release notes come from, which is why
conventional commits are required. Run it by hand to see what a tag would publish:

```sh
./hack/artifacthub-changes.sh v1.1.0
```

Commit types that carry no user-visible change — `docs`, `test`, `ci`, `build`, `chore`, `style` — are
dropped rather than folded into `changed`, because a release note listing a lint fix hides the two lines
that mattered. A release containing nothing else is a normal outcome: the annotation is simply absent,
the workflow logs a warning, and the release proceeds.

**The ownership metadata** is [`artifacthub-repo.yml`](https://github.com/robbeverhelst/unifi-reactor/blob/main/artifacthub-repo.yml) at the repository root.
For an OCI-backed repository Artifact Hub does not fetch this over HTTP — it pulls it from the registry,
as a separate artifact under the reserved tag `artifacthub.io`. The file lives in git so that it is
reviewed and diffable, and is pushed from there with `oras`; the command is in the file's own header.

### Why there is no `signKey`

The image and the chart are both cosign-signed keylessly from the release workflow, and Artifact Hub
picks that up without being told: for any OCI-backed Helm repository, its tracker asks the registry
whether a cosign signature is attached to each chart version and badges the version `cosign` when one is.
There is no annotation that opts into this and none that improves it.

`artifacthub.io/signKey` is a different mechanism. It takes a `fingerprint` and a `url`, and it describes
a **PGP key** used for Helm's provenance-file signing — the `helm package --sign` path. Keyless signing
has no key and no fingerprint, which is the entire point of it: the signature records the workflow and
the tag that produced the artifact, and there is nothing to hold or leak. Filling those two fields with
anything at all would render a fingerprint in the Artifact Hub UI that verifies nothing.

So the annotation is omitted, and the `Verifying a signed release` link in `artifacthub.io/links` points
at [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#verifying-a-release), which carries the certificate identity and the OIDC
issuer that a reader actually needs to run `cosign verify`.

### Security report

Artifact Hub scans container images with Trivy on its own schedule and needs no annotation to do it: it
extracts the images from a dry-run install using the chart's default values. There is deliberately no
`artifacthub.io/images` annotation, because that annotation exists to declare images the dry run would
miss, and this chart renders exactly one container from its defaults.

## OperatorHub.io

**Scoped, and deliberately not started** — tracked as
[#79](https://github.com/robbeverhelst/unifi-reactor/issues/79) rather than half-built here, because a
partial bundle is worse than none: it would publish an install path nobody tests.

Reactor does qualify on the surface — it is an operator, it owns a CRD, it has a defined upgrade path.
What a listing actually costs is a bundle, and the bundle is not the hard part:

- `bundle/manifests/` containing a ClusterServiceVersion and the CRD, plus `bundle/metadata/annotations.yaml`
  and a `bundle.Dockerfile`, passing `operator-sdk bundle validate --select-optional suite=operatorframework`.
- A CSV that restates the Deployment, the ServiceAccount and **every RBAC rule** in its own schema. That
  is the objection that matters. [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md) argues from the permissions the chart
  grants, and several of those are conditional — `get` on Secrets only when outbound actions are
  configured, a `ClusterRole` over nodes only when `rbac.allowNodeActions` is on, and two whole RBAC modes
  from `rbac.clusterWide`. A CSV expresses one install. Restating the rules there creates a second source
  of truth for exactly the thing that argument depends on being single, and the two would drift silently.
- An upgrade graph maintained by hand — a channel plus `replaces`/`skips` per version — and a PR to
  `k8s-operatorhub/community-operators` for **every release**, with its own CI to keep passing.
- An icon, base64-encoded into the CSV. None exists ([see below](#things-only-the-maintainer-can-do)).
- Verification that OLM can actually install it into a cluster, which is not something this repository's
  tests cover today.

The honest order is: settle whether the conditional RBAC can be represented without duplicating it,
then build the bundle, then list. Note also that `operator-framework/awesome-operators` — one of the
lists below — is archived and points at OperatorHub.io as its replacement, so the two are one piece of
work rather than two.

## Lists

Each list's own rules, checked on 2026-08-14. Three of the four are blocked on criteria this project does
not meet, and the honest reading of that is that it is three days old rather than that the rules are
wrong.

| List | The rule | Status |
| --- | --- | --- |
| [AwesomeHomelab/awesome-homelab](https://github.com/AwesomeHomelab/awesome-homelab) | No stated age or popularity floor. Entries are a `name`/`url` pair in `data/<category>.yaml`; `README.md` is generated by `pnpm build` and must not be hand-edited. | **PR open** — [#120](https://github.com/AwesomeHomelab/awesome-homelab/pull/120), added to `data/automation.yaml` in the house style. |
| [ramitsurana/awesome-kubernetes](https://github.com/ramitsurana/awesome-kubernetes) | PR template: "Minimum of 25 GitHub Stars", "Minimum of 3+ contributors", "Proper documentation". Exception only for a project "hosted by a recognized organization". | **Not eligible.** One star, one contributor. Documentation is not the blocker. Resubmit when both counts are met. |
| [awesome-selfhosted](https://github.com/awesome-selfhosted/awesome-selfhosted-data) | PR checklist: "Any software project you are adding was first released more than 4 months ago." Entries are `software/<name>.yml` in the data repository, not the markdown one. | **Not eligible until 2026-12-12.** First release `v0.1.0` was 2026-08-12. See the scope question below before submitting. |
| [operator-framework/awesome-operators](https://github.com/operator-framework/awesome-operators) | — | **Archived.** Its README reads "Repository is obsolete… archived in favor of operatorhub.io". No PR is possible; this is subsumed by [OperatorHub.io](#operatorhubio) above. |

The awesome-selfhosted scope question, worth settling before December rather than in a PR thread: its
rules exclude "anything that is a generic container/deployment automation/virtualization/… tool" as
better suited to [awesome-sysadmin](https://github.com/awesome-foss/awesome-sysadmin). Reactor is not
generic deployment automation — it is a specific integration between one vendor's network hardware and a
cluster — but a Kubernetes operator is close enough to that line that the submission should say so in one
sentence rather than leave a maintainer to guess. awesome-sysadmin is the fallback and may simply be the
better list.

## Community posts

Drafted and **unposted**, in [`docs/community-posts.md`](https://github.com/robbeverhelst/unifi-reactor/blob/main/docs/src/content/docs/contributing/community-posts.md): r/Ubiquiti, r/homelab,
r/selfhosted, the Ubiquiti community forum, and a Show HN. Each has its own framing and each carries the
project's two documented uncertainties — the unobserved WAN failover, and the three keys parsed against a
documented shape rather than a capture. That file also records the self-promotion rule for each venue,
which is the thing most likely to get a first post removed.

Sending them is the maintainer's decision. Nothing in this repository posts anything anywhere.

## Things only the maintainer can do

In order. The first four are the Artifact Hub listing and are worth doing together.

1. **Fix the GitHub repository description.** It currently reads "A Kubernetes Operator that turns UniFi
   *events* into infrastructure actions" — which contradicts the design the README opens with, and is the
   one line that appears in GitHub search, in link previews, and in the generated awesome-homelab table.
   Something like "A Kubernetes operator that reacts to observed UniFi state — WAN failover, UPS on
   battery — with declarative actions on your cluster" matches what ships. It is a repository setting, not
   a file.
2. **Add the repository on Artifact Hub**, signed in as the account whose email matches the `owners` entry
   in [`artifacthub-repo.yml`](https://github.com/robbeverhelst/unifi-reactor/blob/main/artifacthub-repo.yml). Kind: Helm charts. URL, exactly:
   `oci://ghcr.io/robbeverhelst/charts/reactor`. One OCI repository is one chart; the package name comes
   from `Chart.yaml`, not from the URL.
3. **Push the ownership metadata** with the `oras` command in that file's header, then take the
   `repositoryID` Artifact Hub issues, uncomment it in the file, commit, and push the file again. The
   ownership claim works without it; the Verified publisher badge does not.
4. **Check the listing renders**: the CRD card with both examples, the `cosign` signature badge, and the
   Trivy security report. The annotations reach Artifact Hub with the **next release** — the current
   published chart, v1.1.0, was packaged before they existed.
5. **Decide the `Documentation` link.** `artifacthub.io/links` points at `https://reactor.robbeverhelst.com`,
   which does not exist yet ([#74](https://github.com/robbeverhelst/unifi-reactor/issues/74)). It is there
   because the Artifact Hub record should be right the day the site lands and metadata is cheap to update.
   Nothing else in this repository links to it. If the site is not up when the next tag is cut, delete
   those two lines from `Chart.yaml` and put them back later.
6. **An icon, eventually.** `helm lint` asks for one, Artifact Hub shows a placeholder without one, and a
   community-operators CSV would require one. The only artwork here is a wide banner, and cropping it
   would look like a cropped banner. This is a design task, most naturally done alongside the docs site.
