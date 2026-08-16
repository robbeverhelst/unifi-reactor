---
title: "Automation API"
description: "Every field of the Automation custom resource: type, required, enum values and defaults, generated from the API types."
editUrl: false
tableOfContents:
  maxHeadingLevel: 2
---

:::note[Generated from source]
This page is generated from `api/v1alpha1` by `make docs`. Change it there — CI fails when this file and its source disagree.
:::

Every field of the `Automation` custom resource: its type, whether it is required, the values it accepts and what it defaults to. Generated from the Go types the CRD itself is generated from, so the two cannot disagree.

[Your first Automation](/start/first-automation/) is the shortest way in; [Actions](/actions/kubernetes/) explains what each action type does.

## Resource Types
- [Automation](#automation)

## Action

Action is a single normalized action. Type selects the action provider;
the provider-specific fields are flat and validated per type.

Types divide into two kinds. A desired-state action (kubernetes.scale,
kubernetes.cronjob.suspend) declares a level and is arbitrated continuously
across every Automation sharing its target. An edge action (kubernetes.restart,
http.request, notification.*, homeassistant.service, qbittorrent.*,
unifi.wlan.*, unifi.poe.cycle) expresses an occurrence: it fires on this Automation's own
transitions, owns no target and arbitrates with nothing.

The dividing line is not "does this express a level" — pausing a torrent
client plainly does. It is whether there is somewhere to record the value the
target held before Reactor claimed it, so that release can put it back. For a
Kubernetes object that is an annotation on the object; for anything else
there is no answer yet, which is why qbittorrent.* is an edge action and is
named as a verb. See the QBittorrent type.

A desired-state action's level is an integer the arbiter orders and nothing
more, so a boolean level is carried as its own field — replicas for a count,
suspended and cordoned for a switch — rather than by overloading one of them.
The units differ; the ordering does not.

_Appears in:_
- [AutomationSpec](#automationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type of the action, e.g. "kubernetes.scale". |  | Enum: [kubernetes.scale kubernetes.cronjob.suspend kubernetes.cordon kubernetes.restart http.request notification.ntfy notification.discord notification.slack homeassistant.service qbittorrent.pause qbittorrent.resume unifi.wlan.enable unifi.wlan.disable unifi.poe.cycle] <br /> |
| `target` _[TargetRef](#targetref)_ | Target of a kubernetes.* action. |  | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas is the desired replica count for kubernetes.scale. |  | Minimum: 0 <br />Optional: \{\} <br /> |
| `suspended` _boolean_ | Suspended is whether kubernetes.cronjob.suspend wants the target CronJob<br />suspended. Omitting it means true, which is what the action is named<br />after; write suspended: false in spec.onExit to ask for it back.<br />Suspending stops new Jobs being created. It deliberately does nothing to<br />a Job already running: killing work in flight is a different and much<br />more dangerous action than declining to start more of it, and mid-flight<br />deletion is not something an outage should decide on your behalf. |  | Optional: \{\} <br /> |
| `cordoned` _boolean_ | Cordoned is whether kubernetes.cordon wants the target Node closed to new<br />scheduling. Omitting it means true, which is what the action is named<br />after; write cordoned: false in spec.onExit to reopen it explicitly.<br />Cordoning stops new Pods being scheduled onto the Node and moves nothing<br />that is already running. Evicting those — draining — is not offered, and<br />not because it was not built: an eviction cannot be reversed, so it has no<br />level to arbitrate and no reversal to declare, which is the one property<br />every other action here has. See<br />https://reactor.robbeverhelst.com/design/spec/.<br />Node actions need cluster-scoped RBAC that the chart grants only when<br />rbac.allowNodeActions is set. Without it the Automation reports the target<br />as unreachable and names the value to set. |  | Optional: \{\} <br /> |
| `request` _[HTTPRequest](#httprequest)_ | Request describes the outbound call an http.request action makes. |  | Optional: \{\} <br /> |
| `notification` _[Notification](#notification)_ | Notification is the message a notification.* action sends. |  | Optional: \{\} <br /> |
| `homeAssistant` _[HomeAssistantService](#homeassistantservice)_ | HomeAssistant is the service call a homeassistant.service action makes. |  | Optional: \{\} <br /> |
| `qbittorrent` _[QBittorrent](#qbittorrent)_ | QBittorrent is the instance a qbittorrent.* action acts on. |  | Optional: \{\} <br /> |
| `wlan` _[WLAN](#wlan)_ | WLAN is the wireless network a unifi.wlan.* action acts on. |  | Optional: \{\} <br /> |
| `poe` _[PoEPort](#poeport)_ | PoE is the switch port a unifi.poe.cycle power-cycles. |  | Optional: \{\} <br /> |
| `timeoutSeconds` _integer_ | TimeoutSeconds bounds a single attempt at this action, so an<br />unreachable target or endpoint cannot occupy a reconcile indefinitely.<br />Defaults to 30 for the kubernetes.* actions and for the unifi.* console<br />ones — which are a login, a check and a write rather than a single request<br />— and to 10 for the outbound ones, which may retry within the same<br />reconcile. Exceeding it is recorded as a failed execution, not held open. |  | Maximum: 600 <br />Minimum: 1 <br />Optional: \{\} <br /> |

## Automation

Automation reacts to provider state or events with declarative actions.

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `reactor.robbeverhelst.com/v1alpha1` | | |
| `kind` _string_ | `Automation` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[AutomationSpec](#automationspec)_ |  |  | Required: \{\} <br /> |
| `status` _[AutomationStatus](#automationstatus)_ |  |  | Optional: \{\} <br /> |

## AutomationSpec

AutomationSpec defines the desired automation: the state condition to watch
and the actions to run while it holds.

v1alpha1 has one trigger kind. The event-shaped `spec.trigger` this schema
used to accept was removed because nothing implemented it: no captured
delivery payload exists to match against, and every action type is a
desired-state action that is arbitrated continuously rather than fired on an
occurrence. It returns in a later API version once both exist. Nothing about
`when` changes when it does.

_Appears in:_
- [Automation](#automation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `when` _[StateTrigger](#statetrigger)_ | When is a state trigger: active while the provider state matches. |  | Required: \{\} <br /> |
| `actions` _[Action](#action) array_ | Actions run while the state condition holds. |  | MinItems: 1 <br /> |
| `onExit` _[Action](#action) array_ | OnExit declares what this Automation wants for its targets once its<br />condition stops holding. For desired-state actions this is a level that<br />is arbitrated against every other Automation sharing the target, not a<br />list executed on the exit edge: leaving the matching state does not<br />raise a target another Automation still holds down. |  | Optional: \{\} <br /> |
| `reversal` _[ReversalPolicy](#reversalpolicy)_ | Reversal selects what this Automation wants while its condition does not<br />hold. Defaults to Declared when spec.onExit is set, and to Baseline —<br />restore what the target was before Reactor first touched it — when it is<br />not. Set None to leave targets wherever they were left. |  | Enum: [Declared Baseline None] <br />Optional: \{\} <br /> |
| `dryRun` _boolean_ | DryRun evaluates this Automation fully and reports what it would do,<br />without touching anything.<br />It is out of force exactly as Suspend is: it claims no target, writes<br />nothing, and cannot change what any other Automation's targets resolve<br />to — which is what makes it safe to apply one next to policies that are<br />live. What it adds is status.targets[].preview: the arbitration<br />recomputed as if this Automation's condition held and it were in force,<br />naming what each target would be held at, who would outvote it, who it<br />would outvote, and what it would hand back afterwards.<br />Turning it on for an Automation that is currently holding a target is a<br />release, exactly as suspending it is. It stops being in force, so the<br />target goes back to whatever the remaining claims want.<br />A preview is a fact about the moment it was computed and not a promise<br />about the next one: the peers, the observed state and the target can all<br />change before the condition it describes actually holds. See the README. | false | Optional: \{\} <br /> |
| `suspend` _boolean_ | Suspend takes this Automation out of force without deleting it: it goes<br />on observing state and reporting it, and stops claiming its targets<br />entirely.<br />Suspending is a reversible delete, not a freeze. Targets are arbitrated<br />as if this Automation did not exist, so whatever it was holding down is<br />handed back to the other Automations claiming it — or, if none do, to<br />this Automation's own spec.reversal, exactly as deleting it would. Which<br />also means a suspended Automation writes nothing and can hold nothing<br />down: scale its targets by hand while you work.<br />Resuming re-evaluates against current state rather than replaying<br />anything, so an Automation whose condition still holds re-claims its<br />targets on the next reconcile. | false | Optional: \{\} <br /> |

## AutomationStatus

AutomationStatus is the observed state of an Automation.

_Appears in:_
- [Automation](#automation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions represent the current service state. "Ready" reports whether<br />the Automation is valid and being reconciled; "Applied" reports whether<br />what it wants is what its targets actually have. |  | Optional: \{\} <br /> |
| `matching` _boolean_ | Matching is true while a state trigger's condition currently matches. |  | Optional: \{\} <br /> |
| `observedState` _object (keys:string, values:string)_ | ObservedState is the provider state relevant to this Automation at the<br />last reconcile, e.g. \{"wan": "backup"\}. |  | Optional: \{\} <br /> |
| `observedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | ObservedAt is when the provider state above was read from the provider,<br />which is not the same as when this Automation last reconciled.<br />It is the qualifier on every other field here. A decision is only as<br />current as the observation it was taken against, and the two windows that<br />separate them are very different: a value that CHANGED reaches this<br />object within one poll interval times the samples its key must hold for,<br />while a provider that has stopped answering leaves this timestamp<br />standing still and every decision below being re-taken against it. Past<br />the age the install allows, Ready goes False with reason<br />ObservationStale — and Reactor still acts, because withdrawing state it<br />cannot confirm would release claims mid-incident.<br />Absent until the provider has reported anything at all. |  | Optional: \{\} <br /> |
| `lastTransition` _[StateTransition](#statetransition)_ | LastTransition is the state change that last flipped Matching. |  | Optional: \{\} <br /> |
| `lastExecution` _[ExecutionStatus](#executionstatus)_ | LastExecution is the outcome of the most recent desired-state action run,<br />including onExit runs (recorded for auditability). |  | Optional: \{\} <br /> |
| `edgeActions` _[EdgeExecutionStatus](#edgeexecutionstatus) array_ | EdgeActions is what the edge actions did on the last transition, one<br />entry per action that fired, in spec order. It is replaced wholesale on<br />each transition rather than appended to: it answers "what happened when<br />this last changed", not "what has ever happened". |  | Optional: \{\} <br /> |
| `targets` _[TargetStatus](#targetstatus) array_ | Targets reports the arbitrated outcome per target, explaining why an<br />Automation that wants something is not getting it. |  | Optional: \{\} <br /> |
| `releaseAttempts` _integer_ | ReleaseAttempts counts how many times handing this Automation's targets<br />back has failed during deletion. Deletion gives up once it is exhausted<br />rather than leaving the resource stuck terminating. |  | Optional: \{\} <br /> |

## EdgeExecutionStatus

EdgeExecutionStatus records what one edge action did on this Automation's
last transition.

Edge actions are reported separately from LastExecution because they fail
differently: a desired-state action that fails is corrected by the next
reconcile, so its failure is the Automation's problem. An edge action fires
on an occurrence that has already passed, so a failure is a thing that did
not happen — worth reporting, but not a reason to call an Automation whose
workload was scaled correctly unhealthy.

_Appears in:_
- [AutomationStatus](#automationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type is the action type this entry describes, e.g. "notification.ntfy". |  |  |
| `status` _string_ | Status is "Success", "Failed", or "Skipped". |  |  |
| `reason` _string_ | Reason explains a failure or a skip. It never contains a credential: a<br />destination is reported as scheme, host and port only, with the path and<br />query — the part of a webhook URL that is the secret — left out, and a<br />response body is never included. |  | Optional: \{\} <br /> |
| `destination` _string_ | Destination is the scheme, host and port the request went to, for the<br />same reason and with the same omissions.<br />An action that writes to a provider's own console reports the object it<br />acted on instead — "unifi/wlan/Guest" — because the console's address is<br />install configuration that is the same for every Automation, while which<br />object was touched is the part worth reading. |  | Optional: \{\} <br /> |
| `attempts` _integer_ | Attempts counts how many times this action was tried. More than one only<br />happens for actions declared safe to repeat. |  | Optional: \{\} <br /> |
| `onExit` _boolean_ | OnExit is true when this action ran from spec.onExit. |  | Optional: \{\} <br /> |
| `time` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | Time is when the attempt finished. |  | Optional: \{\} <br /> |

## ExecutionStatus

ExecutionStatus records the outcome of the most recent action execution.

_Appears in:_
- [AutomationStatus](#automationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `status` _string_ | Status is "Success" or "Failed". |  |  |
| `reason` _string_ | Reason holds a human-readable error when Status is "Failed". |  |  |
| `time` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | Time is when execution finished. |  |  |
| `onExit` _boolean_ | OnExit is true when this execution ran the onExit actions. |  |  |
| `attempts` _integer_ | Attempts counts consecutive failures. Retries stop once the budget is<br />exhausted, leaving the Automation to recover on the next state change<br />rather than retrying a hopeless action forever. |  | Optional: \{\} <br /> |

## HTTPHeader

HTTPHeader is one header sent with an http.request action.

Values are literal and never templated, and this is not where credentials
go: everything here is readable by anyone who can read the Automation.
Authorization and API-key headers come from the referenced Secret.

_Appears in:_
- [HTTPRequest](#httprequest)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the header. Authorization is rejected here — it comes from the<br />Secret. |  | MaxLength: 128 <br />MinLength: 1 <br />Pattern: `^[A-Za-z0-9!#$%&'*+.^_\|~-]+$` <br /> |
| `value` _string_ | Value of the header. Literal, never templated. |  | MaxLength: 1024 <br />Optional: \{\} <br /> |

## HTTPRequest

HTTPRequest describes an outbound request for an http.request action.

The destination is constrained twice over, because an operator that issues
requests on demand is reachable by anyone who can create an Automation: the
install-level allowlist decides which hosts Reactor may talk to at all, and
loopback and link-local addresses are refused whatever the allowlist says.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `method` _string_ | Method of the request. Defaults to POST. | POST | Enum: [GET POST PUT PATCH] <br />Optional: \{\} <br /> |
| `url` _string_ | URL to request. Must be http or https, must carry no user information,<br />and must be allowed by the install's destination allowlist. Never<br />templated: the destination is a fixed decision, not something observed<br />state gets to influence.<br />Omit it to take the URL from the Secret's url key instead, which is how<br />a URL that is itself a credential stays out of the Automation. Exactly<br />one of the two must supply it. |  | MaxLength: 2048 <br />Optional: \{\} <br /> |
| `secretRef` _[SecretReference](#secretreference)_ | SecretRef names a Secret in this Automation's namespace holding the<br />credentials for this request. Recognised keys: url, authorization, and<br />any key prefixed header- whose remainder is the header name. |  | Optional: \{\} <br /> |
| `headers` _[HTTPHeader](#httpheader) array_ | Headers sent with the request, in addition to those from the Secret.<br />Literal values only. |  | MaxItems: 16 <br />Optional: \{\} <br /> |
| `body` _string_ | Body of the request, rendered as a Go text/template against the<br />transition. Available fields are Automation, Namespace, Name, Provider,<br />Matching, Key, From, To, State and Time; a json function quotes a value<br />safely for embedding in JSON. See the README for the syntax.<br />The body is the only part of the request state can reach, and it only<br />ever carries values this Automation already observes, to a destination<br />the operator allowed.<br />State carries the keys in spec.when.state and nothing else. A reference<br />to any other key — or to a field the context does not have — is reported<br />on the object as Ready=False with reason TemplateWillNotRender when it is<br />reconciled, rather than failing when the action fires. |  | MaxLength: 4096 <br />Optional: \{\} <br /> |
| `idempotent` _boolean_ | Idempotent declares that repeating this request is harmless, which is<br />what lets Reactor retry it after a timeout or a 5xx. GET and PUT are<br />treated as idempotent without saying so; POST and PATCH are not, and are<br />attempted exactly once so a transient failure cannot turn into a second<br />order, message or payment. |  | Optional: \{\} <br /> |

## HomeAssistantService

HomeAssistantService is one Home Assistant service call.

It is a shape over the same outbound transport http.request uses — the same
install-level destination allowlist, the same address floor in the dialer,
the same rule that credentials come only from a Secret in this Automation's
own namespace. What it adds is that the request path is built from a domain
and a service rather than written out, so the action states what it is and
cannot be turned into an arbitrary request to an allowed host.

The direction matters and is the reason this exists. Home Assistant can
already see UniFi; what it cannot see is the cluster. This is the seam
Reactor reaches it through, and it is also why Reactor does not observe
presence itself.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `url` _string_ | URL is the base address of the Home Assistant instance, e.g.<br />https://home-assistant.example.com. It may carry a path, for an instance<br />behind a reverse proxy, and takes no query or fragment. The service path<br />is appended by Reactor and is not expressible here.<br />Omit it to take the base address from the Secret's url key instead.<br />Exactly one of the two must supply it. |  | MaxLength: 2048 <br />Optional: \{\} <br /> |
| `secretRef` _[SecretReference](#secretreference)_ | SecretRef names a Secret in this Automation's namespace holding the<br />long-lived access token, under the authorization key and in the form<br />"Bearer &lt;token>". It may also hold the base address under url.<br />A token is required: Home Assistant authenticates every API call, and<br />there is no unauthenticated shape of this action to fall back to. |  |  |
| `domain` _string_ | Domain of the service being called, e.g. "light", "script", "notify". |  | MaxLength: 64 <br />MinLength: 1 <br />Pattern: `^[a-z0-9_]+$` <br /> |
| `service` _string_ | Service to call within the domain, e.g. "turn_on". |  | MaxLength: 64 <br />MinLength: 1 <br />Pattern: `^[a-z0-9_]+$` <br /> |
| `data` _string_ | Data is the service data, rendered as a Go text/template against the<br />transition and sent as the JSON request body. It must render to a JSON<br />object; omitting it sends an empty one. Available fields are Automation,<br />Namespace, Name, Provider, Matching, Key, From, To, State and Time, and a<br />json function quotes a value safely for embedding. See the README.<br />State carries the keys in spec.when.state and nothing else. A reference<br />to any other key — or to a field the context does not have — is reported<br />on the object as Ready=False with reason TemplateWillNotRender when it is<br />reconciled, rather than failing when the action fires. |  | MaxLength: 4096 <br />Optional: \{\} <br /> |
| `idempotent` _boolean_ | Idempotent declares that calling this service twice is the same as<br />calling it once, which is what lets Reactor retry it after a timeout or a<br />5xx. It defaults to false, and has to: light.turn_on is idempotent and<br />script.turn_on, notify.mobile_app and button.press are not, and Reactor<br />cannot tell which one it was handed. A duplicate announcement or a second<br />press is worse than a missed one when nobody knows what was pressed. |  | Optional: \{\} <br /> |

## Notification

Notification is the message a notification.* action sends.

The destination is not expressible here at all: it comes from the referenced
Secret, because for every transport shipped the URL is itself the credential.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretRef` _[SecretReference](#secretreference)_ | SecretRef names a Secret in this Automation's namespace holding the<br />destination. Required keys: url. Optional: authorization, sent as the<br />Authorization header. |  |  |
| `title` _string_ | Title of the notification, rendered as a Go text/template against the<br />transition. Transports without a title concept prepend it to the message. |  | MaxLength: 256 <br />Optional: \{\} <br /> |
| `message` _string_ | Message body, rendered as a Go text/template against the transition.<br />Available fields are Automation, Namespace, Name, Provider, Matching,<br />Key, From, To, State and Time. See the README for the syntax.<br />State carries the keys in spec.when.state and nothing else. A reference<br />to any other key — or to a field the context does not have — is reported<br />on the object as Ready=False with reason TemplateWillNotRender when it is<br />reconciled, rather than failing when the action fires. |  | MaxLength: 2048 <br />MinLength: 1 <br /> |

## PoEPort

PoEPort is the switch port a unifi.poe.cycle power-cycles.

This is the most dangerous action Reactor has, and the danger is not the
write — it is the identity. Cutting power to the wrong port drops an access
point, a camera, or the uplink carrying the cluster, and it does so silently
from Reactor's point of view: the console accepts the command either way.

So a port is identified by three things that must all agree, checked against
the switch's own port table immediately before the command is sent:

  - device, the switch's MAC. Not its name, which is a label somebody can
    change without changing which hardware it is.
  - port, the index on that switch.
  - portName, the name that port carries in the switch's configuration.

The third is the one doing the real work, and it is required rather than
optional for that reason. A port index alone means something different after
somebody re-patches a rack: slot 7 is still slot 7, and the thing plugged
into it is not. Naming what is supposed to be there turns a re-patch from a
silent mis-cycle into a refused action with a sentence saying the port is
called something else now.

Three refusals apply whatever the install's allowlist says, in the same way
the outbound dialer refuses loopback whatever the destination allowlist says.
A port the switch reports as its uplink is never cycled — that is the port
carrying everything behind the switch, including, possibly, Reactor's own
path to the console. A port the switch does not report as PoE-capable is
never cycled, because there is nothing there to cycle and the identity is
probably wrong. And a switch that does not report those fields at all is
refused rather than assumed safe: a guard that silently does not apply is
worse than one that declines.

Which ports may be cycled at all is the operator's decision at install time —
unifi.actions.allowedPoePorts, empty by default and refusing everything.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `device` _string_ | Device is the MAC address of the switch, lowercase and colon-separated,<br />e.g. "aa:bb:cc:00:11:22". A MAC rather than a device name because a name<br />is a label: renaming a switch would silently repoint this action, and a<br />MAC identifies the hardware. |  | Pattern: `^[0-9a-f]\{2\}(:[0-9a-f]\{2\})\{5\}$` <br /> |
| `port` _integer_ | Port is the port index on that switch, as the console numbers it — the<br />number on the front panel, starting at 1. |  | Maximum: 64 <br />Minimum: 1 <br /> |
| `portName` _string_ | PortName is the name that port carries in the switch's configuration, and<br />it is checked before anything is sent. It is required, and it is the whole<br />defence against a re-patched rack: an index means "whatever is in slot 7<br />now", and this means "the thing I meant".<br />If it stops matching, the action is refused and says so. That is the<br />intended outcome — name your ports, and a change to the wiring becomes a<br />visible refusal instead of a power cut to something else. |  | MaxLength: 128 <br />MinLength: 1 <br /> |

## QBittorrent

QBittorrent is the instance a qbittorrent.pause or qbittorrent.resume acts on.

Pausing is a level in the world — paused or running — and an edge action
here, which is the one thing about this type worth understanding before
using it.

A desired-state action is arbitrated across every Automation claiming its
target, and what makes that possible is not the fold: it is that the target
is a Kubernetes object, so the value it held before Reactor first touched it
can be recorded as an annotation ON that object, where it outlives both the
Automation and Reactor itself. A qBittorrent instance reached over HTTP has
no such place. It has no Kubernetes identity to arbitrate over, no annotation
to hold a baseline, and no way for the pre-delete sweep — which reads those
annotations with no credentials and no allowlist — to hand it back.

So the honest shape is an edge action, and two limitations follow from that
rather than being oversights:

  - It is not arbitrated. Two Automations pausing the same instance for
    unrelated reasons do not resolve to one claim; each fires on its own
    transition, and whichever resumes first resumes everything.
  - It has no baseline. A resume resumes every torrent, including ones
    paused by hand before Reactor ever ran. Nothing here can tell those
    apart, because nothing recorded which they were.

A design for non-Kubernetes desired-state targets — somewhere legitimate to
keep a baseline and a claim for a thing with no object to hang them on — does
not exist yet, and inventing one inside this type would be the worst place to
try. See the README for the alternatives that were considered.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `url` _string_ | URL is the base address of the qBittorrent WebUI, e.g.<br />http://qbittorrent.media.svc.cluster.local:8080. The API paths are<br />appended by Reactor and are not expressible here.<br />Omit it to take the base address from the Secret's url key instead.<br />Exactly one of the two must supply it. |  | MaxLength: 2048 <br />Optional: \{\} <br /> |
| `secretRef` _[SecretReference](#secretreference)_ | SecretRef names a Secret in this Automation's namespace holding the WebUI<br />username and password, under the username and password keys. It may also<br />hold the base address under url.<br />Both are required. qBittorrent issues a session cookie rather than<br />accepting a static token, and that login is the entire reason this action<br />exists rather than being an http.request — an instance configured to<br />bypass authentication for its subnet is expressible as one POST with<br />http.request, and that is the honest thing to write for it. |  |  |

## ReversalIntent

ReversalIntent is one Automation's declared reversal level for a target: what
it says that target should be once nothing claims it any more.

It is reported only as part of a disagreement, because that is the only time
naming one is worth anything. An Automation's own reversal is already in
TargetStatus.Desired while it is not matching; what this adds is the other
Automations' answers to the same question, alongside it, so the contradiction
reads as one fact rather than as something to be assembled from two objects.

_Appears in:_
- [TargetStatus](#targetstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `claimant` _string_ | Claimant is the Automation declaring this level, as "namespace/name". |  |  |
| `desired` _integer_ | Desired is the level it would hand the target back to. Ordered exactly as<br />TargetStatus.Desired is: lower is more restrictive, and the lowest is the<br />one that wins. |  |  |
| `level` _string_ | Level renders Desired in the units of the action that set it, for the<br />same reason TargetStatus.Level does — a disagreement between "suspended"<br />and "running" is not readable as one between 0 and 1. |  | Optional: \{\} <br /> |

## ReversalPolicy

_Underlying type:_ _string_

ReversalPolicy selects what an Automation wants for its targets while its
condition does not hold.

A target's value is arbitrated across every Automation that references it,
so an Automation is never simply "done" — it always either claims the target
or declares what it wants once nothing claims it any more.

_Validation:_
- Enum: [Declared Baseline None]

_Appears in:_
- [AutomationSpec](#automationspec)

| Field | Description |
| --- | --- |
| `Declared` | ReversalDeclared restores the values in spec.onExit. The default when<br />spec.onExit is set.<br /> |
| `Baseline` | ReversalBaseline restores what the target was set to before Reactor<br />first claimed it. The default when spec.onExit is omitted.<br /> |
| `None` | ReversalNone leaves the target wherever it was left. Reactor stops<br />asserting a value for it entirely.<br /> |

## SecretReference

SecretReference names a Secret in the Automation's own namespace.

There is deliberately no namespace field. An Automation may only ever read
credentials from the namespace it lives in, because anyone able to create an
Automation can already create a Secret there — while a cross-namespace read
would let them borrow the operator's cluster-wide reach to pull a credential
they have no access to themselves.

_Appears in:_
- [HTTPRequest](#httprequest)
- [HomeAssistantService](#homeassistantservice)
- [Notification](#notification)
- [QBittorrent](#qbittorrent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the Secret in this Automation's namespace. |  | MinLength: 1 <br /> |

## StateTransition

StateTransition records the last observed provider state transition that
changed this Automation's matching.

_Appears in:_
- [AutomationStatus](#automationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | Key is the state key that transitioned, e.g. "wan". |  |  |
| `from` _string_ | From is the previous value. |  |  |
| `to` _string_ | To is the new value. |  |  |
| `time` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | Time is when the transition was observed. |  |  |

## StateTrigger

StateTrigger fires while a provider's observed state matches. Actions run on
entering the matching state; OnExit actions run on leaving it. Repeated
identical observations are no-ops by design.

_Appears in:_
- [AutomationSpec](#automationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `provider` _string_ | Provider is the event/state provider, e.g. "unifi". |  | MinLength: 1 <br /> |
| `state` _object (keys:string, values:string)_ | State is the provider-scoped key/value condition, e.g. \{"wan": "backup"\}.<br />All entries must match the observed state for the trigger to be active. |  | MinProperties: 1 <br /> |

## TargetPreview

TargetPreview is what would happen to one target if this Automation's
condition held and it were in force, arbitrated against the claims that exist
right now.

It is answerable without writing anything because arbitration is a pure
function of the claims that hold: the same fold that decides a target's value
answers the counterfactual with one more claim in it. It is reported while an
Automation is deliberately out of force — spec.dryRun, or spec.suspend, where
it answers "what would resuming this do" — and is absent otherwise, because
an Automation that is in force is already described by the fields above.

It is also absent on a target ManagedBy names, and that is an answer rather
than a gap: such a target would be declined rather than claimed, so there is
no level to preview.

Three things it cannot promise, all the same thing said three ways: it is
computed from the peers, the observed state and the target as they are at
this moment, and any of them can differ by the time the condition actually
holds. It also says nothing about whether the write would succeed — RBAC, an
admission webhook and a target that has since been deleted are all outside
what a fold can know.

_Appears in:_
- [TargetStatus](#targetstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `desired` _integer_ | Desired is the level this Automation would ask for. Absent when it would<br />ask for nothing on this target. |  | Optional: \{\} <br /> |
| `effective` _integer_ | Effective is what arbitration would resolve to across every claim,<br />including this one. |  | Optional: \{\} <br /> |
| `level` _string_ | Level renders Effective in the units of the action that would set it, for<br />the same reason TargetStatus.Level does. |  | Optional: \{\} <br /> |
| `deferredBy` _string array_ | DeferredBy names the Automations whose more restrictive claim would<br />outvote this one, leaving the target exactly where it already is. |  | Optional: \{\} <br /> |
| `wouldDefer` _string array_ | WouldDefer names the Automations this claim would outvote: the ones<br />getting what they want now that would stop getting it. A peer already<br />outvoted by a third automation is not listed, because it is deferred<br />either way and this claim is not what did it. |  | Optional: \{\} <br /> |
| `onExit` _string_ | OnExit says what this Automation would want for the target once its<br />condition stopped holding, in words — "3 replicas", "running", or "left<br />as found" under reversal None.<br />Rendered rather than numeric because it is read rather than computed<br />against, and because under reversal Baseline it may describe a value<br />Reactor has not recorded yet: on a target nothing has ever claimed, the<br />baseline a claim would capture is simply what the target is at now. |  | Optional: \{\} <br /> |

## TargetRef

TargetRef identifies the Kubernetes object an action operates on.

The cluster-scope rule below reads __namespace__ rather than namespace, and
has to: namespace is a CEL reserved word, so Kubernetes exposes the property
under its escaped name. Written the obvious way the rule does not fail at
admission — it fails to compile, which makes the whole CRD unapplyable and
takes every other field down with it. No unit test catches that, because a
CRD is only compiled by a real API server; the e2e suites are what found it.

It lives on TargetRef rather than on Action because the constraint is a
property of a target reference, not of any particular action that holds one.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ | Kind of the target resource.<br />kubernetes.scale works through the scale subresource, so its executor is<br />kind-agnostic: any object exposing /scale can be held at a replica count<br />without Reactor knowing where that kind keeps its replicas. This list is<br />nevertheless closed, and that is the deliberate half of the trade-off<br />#17 asks about.<br />An open field would buy nothing an operator can use. A kind is only<br />reachable if the chart granted RBAC for it, and RBAC has to name<br />resources explicitly — so an open enum would accept a kind Reactor cannot<br />touch and turn a typo into a Forbidden discovered during the outage the<br />Automation was written for, instead of a rejected write at admission.<br />Adding a kind is an entry here and a rule in the chart, and no executor<br />code either way. |  | Enum: [Deployment StatefulSet CronJob Node] <br /> |
| `name` _string_ | Name of the target resource. |  | MinLength: 1 <br /> |
| `namespace` _string_ | Namespace of the target resource. Defaults to the Automation's own<br />namespace. Cross-namespace targets require the controller to run with<br />cluster-wide RBAC; otherwise the Automation reports Ready=False.<br />Rejected on a cluster-scoped kind. A Node addressed inside a namespace is<br />not a different Node, it is a lookup that cannot succeed. |  | Optional: \{\} <br /> |

## TargetStatus

TargetStatus reports what this Automation wants for one target and what the
arbitration across every Automation sharing that target actually resolved
to. When the two differ, DeferredBy names who is holding it there.

_Appears in:_
- [AutomationStatus](#automationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ref` _string_ | Ref is the target this entry describes, as "Kind/namespace/name", or<br />"Kind/name" for a cluster-scoped target. |  |  |
| `desired` _integer_ | Desired is the level this Automation alone wants right now, whether it<br />is currently matching or reversing. Absent when it wants nothing —<br />reversal None, or a reversal value it has no way to know.<br />A level is ordered and nothing more: lower is more restrictive, and the<br />arbiter resolves a shared target by taking the lowest. What the number<br />counts depends on the action that produced it — replicas for<br />kubernetes.scale, and 0 suspended / 1 running for<br />kubernetes.cronjob.suspend. Level spells the same value out in words. |  | Optional: \{\} <br /> |
| `effective` _integer_ | Effective is the level arbitration resolved to across every Automation<br />claiming this target. Absent while nothing claims it. |  | Optional: \{\} <br /> |
| `level` _string_ | Level renders Effective in the units of the action that set it — "3<br />replicas", "suspended" — because a bare number stops explaining itself<br />once a target's level is a switch rather than a count. |  | Optional: \{\} <br /> |
| `deferredBy` _string array_ | DeferredBy names the Automations whose more restrictive claim is holding<br />the target away from Desired, as "namespace/name". Empty when this<br />Automation's intent is the one in effect. |  | Optional: \{\} <br /> |
| `preview` _[TargetPreview](#targetpreview)_ | Preview is what would happen here if this Automation were in force,<br />reported while it deliberately is not. |  | Optional: \{\} <br /> |
| `reversalDisagreement` _[ReversalIntent](#reversalintent) array_ | ReversalDisagreement names every Automation declaring a reversal level<br />for this target, and the level each one declares, whenever they do not<br />all declare the same one. Empty when they agree, which is the normal<br />case, and when only one Automation has an opinion at all.<br />Two Automations that both want a workload down are both right, and<br />resolving that is what DeferredBy reports. Two Automations declaring<br />different levels for the same workload once NOTHING claims it cannot both<br />be right: a workload has one normal size, and these specs disagree about<br />what it is. Reactor does not resolve that — it takes the most restrictive<br />level, the same order-independent tie-break it uses for a live claim, and<br />it says which specs contradicted each other so somebody can fix one.<br />It is reported while the disagreement EXISTS rather than at the moment of<br />release, because the value of knowing is finding out before the outage<br />ends, not after the workload has already come back at the wrong number.<br />Reversal None contributes no level and is never part of one. Two<br />Automations both on Baseline agree by construction, resolving to the same<br />recorded baseline, so the cases reported are Declared against Declared<br />and Declared against Baseline. |  | Optional: \{\} <br /> |
| `managedBy` _string_ | ManagedBy names a controller other than Reactor that already drives this<br />target's level, as "Kind/namespace/name".<br />Arbitration reaches only the Automations, so a claimant that is not one<br />cannot be folded in and cannot be resolved against — it can only be<br />fought, which neither side wins. Reactor therefore declines to claim a<br />target named here and writes nothing to it, and Applied is False with<br />reason TargetManagedByHPA. The Automation stays Ready: it is correctly<br />configured, it simply cannot act on this target.<br />Currently only a HorizontalPodAutoscaler, and only on an install that<br />turned detection on. Nothing here promises the field is uncontested when<br />it is empty: KEDA, a GitOps controller correcting drift and a cron job<br />running kubectl own spec.replicas just as hard, and none of them is<br />discoverable. |  | Optional: \{\} <br /> |

## WLAN

WLAN is the wireless network a unifi.wlan.enable or unifi.wlan.disable acts
on.

This is the first action that writes to the UniFi console, and the first that
can take something away from people who are not running the cluster. Read the
two paragraphs below before using it.

It is an edge action, and the reason is the rule the QBittorrent type states:
a desired-state action is arbitrated, and what makes that possible is not the
fold but that the target is a Kubernetes object, so the value it held before
Reactor claimed it can be recorded as an annotation ON that object. A UniFi
WLAN has no such place. Writing Reactor's bookkeeping into the WLAN's own
configuration is the same mistake a torrent tag would have been — it is the
user's config, they can edit it, and the write that carries it is a
read-modify-write with no concurrency control. And a baseline nobody can read
is not a baseline: releasing a WLAN means a credentialed write to the
console, which the pre-delete sweep during an uninstall is designed to be
incapable of.

So two limitations follow, and they are louder here than for a torrent
client:

  - It is not arbitrated. Two Automations disabling the same WLAN do not
    resolve to one claim; whichever enables it first enables it.
  - Nothing hands it back. If the exit transition never arrives — the
    Automation is deleted, Reactor is uninstalled, the state key stops being
    observable — the WLAN stays as Reactor last left it. A guest network that
    was turned off stays off until a human turns it back on.

The HorizontalPodAutoscaler decline path is the clearest illustration of what
is missing here. A scalable target an HPA already drives can be refused and,
if Reactor was already holding it, put back where it was — and it can be put
back precisely because the baseline is an annotation on the object. A WLAN has
no equivalent, so there is no state it could be declined back to.

Which SSIDs may be touched at all is the operator's decision at install time,
not the Automation's: unifi.actions.allowedWlans is empty by default and
empty refuses everything.

_Appears in:_
- [Action](#action)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the SSID exactly as the console spells it, matched<br />case-sensitively against the WLAN configuration on the site Reactor<br />polls. It must also appear in the install's allowed WLAN list.<br />A name that matches nothing is refused rather than guessed at, and the<br />refusal does not list the WLANs that do exist: an Automation is readable<br />by anyone in its namespace, and the network's SSIDs are not theirs. |  | MaxLength: 64 <br />MinLength: 1 <br /> |
