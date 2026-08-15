---
title: "State, not events"
description: "Reactor polls and reconciles rather than reacting to a stream of events, so a dropped webhook, a network blip or a restart cannot strand your cluster in the wrong mode."
---

## How it works

```mermaid
flowchart LR
    U["UniFi console<br/>gateway · UPS"] -->|"poll — source of truth"| P["UniFi provider<br/>observe · normalize"]
    P -->|"wan · internet · ups · ups.battery"| E["Reactor engine<br/>match · detect transitions"]
    E -->|"entered → actions<br/>left → onExit"| K["Kubernetes<br/>scale Deployments"]
```

The engine knows nothing about UniFi. A provider converts vendor-specific reality into normalized state, and the engine reconciles your `Automation` resources against it. That seam is what lets other providers — a UPS over NUT, Proxmox, Prometheus alerts — arrive later without touching the core.

Observing `wan: backup` fifty times in a row does nothing fifty times. Scaling is a **desired state**, not a command: Reactor works out what every automation currently wants for a workload and reconciles it there, so the result depends only on which conditions hold — never on the order they were observed in.
