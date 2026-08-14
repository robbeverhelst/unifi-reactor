---
title: "Still stuck"
description: "What to collect before opening an issue, so a bug report is reproducible: versions, the Automation, its status, and the log lines around the moment nothing happened."
---

## 16. Still stuck

Collect these and open an issue — the [bug report template](https://github.com/robbeverhelst/unifi-reactor/issues/new/choose) asks for exactly this, and without it nothing is reproducible:

```sh
# UniFi Network version and console model — UniFi UI → Settings → System

# Chart and image version
helm -n reactor-system list
kubectl -n reactor-system get deploy reactor \
  -o jsonpath='{.spec.template.spec.containers[0].image}'

# The Automation and its status
kubectl -n <ns> get automation <name> -o yaml

# Operator logs — ideally with log.level=debug set while reproducing
kubectl -n reactor-system logs deploy/reactor --tail=200
```

**Redact before posting.** Logs and resource dumps can contain your public IP, your ISP, internal hostnames, and site identifiers. Nothing in a bug report needs any of them.
