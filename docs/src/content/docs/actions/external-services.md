---
title: "Home Assistant and qBittorrent actions"
description: "Two named integrations over the generic http.request: calling a Home Assistant service, and pausing qBittorrent without killing the container — including what neither can arbitrate."
---

## Acting on things outside the cluster

`http.request` can already reach anything with an HTTP API, and for a one-off webhook that is the right tool. The action types below exist because two integrations are worth naming: they are used often enough that writing the URL out every time is a papercut, and — more importantly — a named action can constrain the request in ways a generic one cannot.

They are **the same transport**. The same install-level `actions.allowedDestinations` allowlist, the same address floor enforced in the dialer, the same refusal to follow redirects, the same rule that credentials come only from a Secret in the automation's own namespace, and the same origin-only reporting. There is one outbound HTTP client in Reactor and everything here goes through it. [SECURITY.md](https://github.com/robbeverhelst/unifi-reactor/blob/main/SECURITY.md#outbound-actions) has the reasoning.

### Home Assistant

```yaml
  actions:
    - type: homeassistant.service
      homeAssistant:
        url: https://home-assistant.example.com
        secretRef: {name: home-assistant-credentials}
        domain: notify
        service: persistent_notification
        data: '{"message": "the cluster is on battery", "title": "Reactor"}'
```

It calls `POST /api/services/<domain>/<service>` — `light.turn_on`, `script.turn_on`, `notify.mobile_app_phone`, anything Home Assistant exposes as a service. The credential is a [long-lived access token](https://www.home-assistant.io/docs/authentication/#your-account-profile):

```sh
kubectl -n media create secret generic home-assistant-credentials \
  --from-literal=authorization="Bearer eyJhbGci..."
# the base address may live in the secret instead, under url
```

**The direction is the point.** Home Assistant can already see UniFi — it has an integration for it, and it does presence better than Reactor ever would. What it cannot see is the cluster. So the interesting flow is not Reactor learning that a phone came home; it is Reactor telling Home Assistant that the uplink failed over, the UPS is on battery, or a node was cordoned, and letting Home Assistant do the physical thing. That is why [#10 (`client.<name>`) and #26 (`unifi.client.block`)](https://github.com/robbeverhelst/unifi-reactor/issues/27) were closed rather than built, and this action is the seam that decision rests on.

Three things are narrower than `http.request` on purpose:

- **The path is built, not written.** `domain` and `service` are the only part an automation chooses, and both are restricted to a bare slug — lowercase letters, digits and underscores — at admission *and* again when the URL is built. Without that, "call a service" would quietly be "make any request to an allowed host", which is a real action with a real name and this is not it. A base URL carrying a path is fine (an instance behind a reverse proxy keeps its prefix); a query or fragment on it is refused.
- **`data` must render to a JSON object.** It is templated like any other body, and checked before it is sent — a template that rendered to a list, a bare string or nothing is reported against the automation rather than collected as a 400 from Home Assistant with nothing naming who produced it. Omit it and Reactor sends `{}`.
- **It is attempted exactly once.** `light.turn_on` is idempotent; `script.turn_on`, `notify.*` and `button.press` are not, and Reactor cannot tell which one it was handed. You can: set `idempotent: true` and it retries a timeout or a 5xx like a notification does.

A missing `authorization` key is refused before anything is sent, because Home Assistant answers an unauthenticated call with a 401 that says nothing about which automation produced it.

### qBittorrent

Scaling qBittorrent to zero is the [first automation in this README](/start/first-automation/), and it is a blunt instrument: it kills the container, drops every in-progress connection, and relies on qBittorrent recovering its session from disk. Pausing is what you actually wanted — traffic stops, state is preserved, resume is instant:

```yaml
  actions:
    - type: qbittorrent.pause
      qbittorrent:
        url: http://qbittorrent.media.svc.cluster.local:8080
        secretRef: {name: qbittorrent-credentials}

  onExit:
    - type: qbittorrent.resume
      qbittorrent:
        url: http://qbittorrent.media.svc.cluster.local:8080
        secretRef: {name: qbittorrent-credentials}
```

```sh
kubectl -n media create secret generic qbittorrent-credentials \
  --from-literal=username=reactor \
  --from-literal=password='...'
```

Both credentials are required. An instance configured to bypass authentication for its subnet is already expressible as a single `http.request` — a `POST` to `/api/v2/torrents/pause` with `hashes=all` — and that is the honest thing to write for it. The login round trip is the entire reason this action exists rather than being an example.

#### It is a level in the world and an edge action here

This is the part worth reading before using it, because the limitation is real and stated rather than hidden.

Paused-versus-running is a **level**. Every level Reactor holds is arbitrated: two automations pausing the same thing for unrelated reasons resolve to one claim, and it stays paused until *neither* wants it paused. That is how [`kubernetes.scale` behaves](/concepts/arbitration/#when-two-automations-share-a-workload), and it is what you would want here.

What makes arbitration possible is not the fold. It is that the target is a **Kubernetes object**, so the value it held before Reactor first touched it can be written as an annotation *on that object* — `reactor.robbeverhelst.com/baseline-replicas` — where it outlives the automation, outlives Reactor, and can be read by the pre-delete sweep during an uninstall. A qBittorrent instance reached over HTTP has none of that: no Kubernetes identity to arbitrate over, nowhere to put a baseline, and nothing the uninstall hook could reach even if there were, since it runs with no credentials and no destination allowlist.

Three ways out were available:

| | Why not |
| --- | --- |
| Keep the baseline in the automation's `status` | It dies with the automation — which is exactly the case where release matters. The annotation lives on the *target* for this reason. |
| Keep it in qBittorrent, as a tag or a category | It writes Reactor's bookkeeping into your torrent data, where you can edit it, and it does not survive a torrent being removed and re-added. Reading it back would also mean parsing a response body into Reactor, which the outbound client deliberately cannot do. |
| Synthesize a Kubernetes identity for it | It would arbitrate on string equality of a URL, silently stop arbitrating when two automations spelled the same instance differently, and produce a `status.targets` entry Reactor cannot verify. Arbitration that is sometimes right is worse than none. |

So it ships as an edge action, named as a verb, and **two limitations follow**:

- **It is not arbitrated.** Two automations pausing the same instance do not resolve to one claim. Each fires on its own transition, and whichever resumes first resumes everything. If you need arbitration today, use `kubernetes.scale` on the Deployment — bluntness buys you the fold.
- **There is no baseline, so `resume` resumes everything.** Including torrents you had paused by hand before Reactor ever ran. Nothing can tell those apart, because nothing recorded which they were. This is the same failure mode the [node cordon baseline](/actions/kubernetes/#closing-a-node-to-new-work) exists to prevent, and here it cannot be prevented.

A design for non-Kubernetes desired-state targets — somewhere legitimate to keep a baseline and a claim for a thing with no object to hang them on — **does not exist yet**. When it does, this action becomes a level and the two limitations go away. Until then, an edge action that says what it is beats a desired-state action that pretends to arbitrate.

The two are also complementary rather than exclusive. Pause on the way in *and* let a `kubernetes.scale` claim the Deployment for the harder cases; the scale is still arbitrated normally, because a Deployment is still a Deployment.

#### The session, and the rule it had to fit

Everything else here authenticates with a token you hold. qBittorrent issues one: `POST /api/v2/auth/login` returns a `SID` cookie, and that cookie is a bearer of the same authority as the password. Reactor's rule is that a credential is never held longer than the request that uses it, and a cached session would be exactly the thing that rule forbids for the password itself.

So there is no session cache and no session store. **The login happens inside the one action**, the cookie lives in a local variable for the two requests that need it, and a `POST /api/v2/auth/logout` ends the session on the far end rather than leaving it to expire. The cost is one extra round trip per action. The benefit is that the rule holds as written, on both ends of the connection — and a retry logs in again rather than reusing a cookie from the attempt that just failed.

Three details that follow:

- **A rejected credential is a 200.** qBittorrent answers a wrong username or password with `200 OK` and the body `Fails.`, setting no cookie — so the *absence* of the cookie is the authentication check, and it is reported as a failure rather than a success. It is not retried: a rejected credential does not get better by asking again.
- **Every leg is checked against the allowlist separately**, and the whole exchange is one attempt for retry purposes.
- **This is the one edge action that can argue idempotence** rather than assert it, so it retries a timeout or a 5xx like a notification does: pausing a paused torrent is a no-op, and so is resuming a running one.

`pause` and `resume` are the long-standing WebUI API names. qBittorrent 5.0 introduced `stop` and `start` and deprecated these; deprecated is not removed, and an instance that has removed them answers `404`, which lands in `status.edgeActions[].reason` with the status in it.

**Every torrent, or none.** There is no category or tag filter. Narrowing to one would mean listing torrents and reading the response back into Reactor, and the outbound client deliberately drains and discards every response body — a response can echo a request back, credentials included. That capability is not worth adding for a filter.
