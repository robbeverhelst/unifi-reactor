---
title: "Credentials and reachability"
description: "Reactor cannot reach or authenticate to the console: 401s, self-signed certificates, a rotated API key, and the network path between the operator pod and your gateway."
---

## 3. Credentials and reachability

Every observation failure logs the same line, with the cause in the error:

```sh
kubectl -n reactor-system logs deploy/reactor | grep 'state observation failed'
```

| Error contains | Cause | Fix |
| --- | --- | --- |
| `unexpected status 401` / `403` | API key wrong, revoked, or from a different console | Re-create the key under **Settings → Control Plane → Integrations** and update the Secret |
| `unexpected status 404` | Wrong site. `unifi.site` is the *internal reference*, not the display name | Check the site list; `default` is right for single-site setups |
| `x509` / `certificate signed by unknown authority` | UniFi OS serves a self-signed certificate | `unifi.insecureSkipVerify: true` (the chart default), or trust the CA |
| `context deadline exceeded` / `no route to host` | Pod cannot reach the console | Check NetworkPolicy and that the URL is reachable *from the pod*, not from your laptop |
| `connection refused` on the API path | URL points at the wrong port or scheme | Use the console base URL only — no path, e.g. `https://192.0.2.10` |
| `no gateway with an active WAN uplink and no UPS found` | Reachable and authenticated, but nothing recognizable in the device list | Confirm the gateway is adopted on that site |

Test reachability from inside the cluster rather than from your machine:

```sh
kubectl -n reactor-system run unifi-probe --rm -it --restart=Never \
  --image=curlimages/curl -- \
  curl -sk -o /dev/null -w '%{http_code}\n' \
  -H "X-API-KEY: $UNIFI_API_KEY" \
  'https://192.0.2.10/proxy/network/api/s/default/stat/device'
```

`200` means the credential and the path are both fine and the problem is elsewhere.

### Rotating the API key

The Secret is **mounted**, and the key is re-read from the file on every poll. Rotation therefore needs no restart: update the Secret, and the next poll after the kubelet refreshes the file authenticates with the new key.

```sh
kubectl -n reactor-system create secret generic unifi-reactor-credentials \
  --from-literal=UNIFI_API_KEY=<new key> \
  --dry-run=client -o yaml | kubectl -n reactor-system apply -f -
```

Watch polling continue before revoking the old key in the UniFi UI. Four things can still go wrong:

**Nothing happens for up to a minute.** The kubelet refreshes a mounted Secret on its sync period, roughly a minute by default. A `401` in that window is expected if you revoked the old key first. Revoke second.

**The key file is unreadable or empty.** That poll fails with a distinctive error and the next one retries:

```text
ERROR state observation failed reading unifi api key from /etc/unifi-reactor/credentials/UNIFI_API_KEY: ...
ERROR state observation failed unifi api key file /etc/unifi-reactor/credentials/UNIFI_API_KEY is empty
```

Usually the Secret was replaced without the `UNIFI_API_KEY` key, or with an empty value. Note this is also checked at startup — the operator exits rather than starting up unable to authenticate.

**The Secret is mounted through `subPath`.** A `subPath` mount is a copy made at container start and is *never* updated in place, which silently restores the old "needs a restart" behaviour. The chart mounts the whole directory precisely to avoid this; if you have customised the mount, this is your answer.

**You are on chart 0.3.0 or earlier**, or you set `UNIFI_API_KEY` in the environment yourself. That path injects the key once at container start, where it is fixed for the life of the process. Confirm which one you are on:

```sh
kubectl -n reactor-system get deploy reactor \
  -o jsonpath='{.spec.template.spec.containers[0].env[*].name}'
# UNIFI_API_KEY_FILE  -> mounted, re-read per poll
# UNIFI_API_KEY       -> injected once, rotation needs a restart
```

For the env path, `kubectl -n reactor-system rollout restart deployment/reactor` after rotating. Better, upgrade the chart. If you would rather restart on change anyway — because you already run something like reloader — the chart takes Deployment `annotations` for it.

### The webhook shared secret

Only relevant with `unifi.webhook.enabled`. This is a **second, separate** credential from the API key, and its failures all look the same from the outside: reactions go back to taking up to `unifi.pollInterval`, because the fast path is an optimization and losing it breaks nothing else. Nothing is stuck — it is just slow again.

```sh
kubectl -n reactor-system logs deploy/reactor | grep -i 'webhook'
```

| What you see | Cause |
| --- | --- |
| `Rejected a webhook delivery presenting no valid token` (needs `log.level=debug`) | The console's rule does not carry the secret in `UNIFI_WEBHOOK_TOKEN`, or carries an old one |
| `Webhook fast path listening` and then nothing | Deliveries never arrive: the console cannot reach the receiver. It is a ClusterIP by default, and a `networkPolicy` with the default `ingress: []` also denies them |
| `Could not register the Alarm Manager rule` | Self-registration failed; the error names the reason. Polling is unaffected, and the rule can be created by hand in the UniFi UI |

Rotating this secret is not restart-free the way the API key is, and it takes two steps: the console holds a copy inside the Alarm Manager rule, and Reactor never edits that rule. Update the Secret, restart the pod, then update or recreate the rule in the UniFi UI. Deliveries are refused in between, which costs latency and nothing else.

---
