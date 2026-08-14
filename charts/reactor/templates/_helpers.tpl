{{- define "reactor.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "reactor.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "reactor.labels" -}}
app.kubernetes.io/name: {{ include "reactor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "reactor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "reactor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The namespace the operator may see. Namespace-scoped RBAC grants no cluster-wide
list, so watching every namespace would leave every informer failing while the
health probes still report the pod ready. Rendered into both the manager and the
pre-delete hook so they agree on scope.
*/}}
{{- define "reactor.watchNamespaceEnv" -}}
{{- if not .Values.rbac.clusterWide }}
- name: WATCH_NAMESPACE
  value: {{ .Release.Namespace | quote }}
{{- end }}
{{- end -}}

{{/*
The Automation CRD's name. It is the plural and the API group, so the one object
the adoption hook may touch is derived rather than kept in step by hand.
*/}}
{{- define "reactor.crdName" -}}
automations.reactor.robbeverhelst.com
{{- end -}}

{{/*
"adopt" when this release has to take over a CRD that belongs to no release
before it can template it, and empty otherwise.

Chart 0.3.0 and earlier installed the CRD through crds/, which Helm applies and
then does not record as part of the release. Helm checks ownership while it
prepares the upgrade — before it runs a single hook — so a hook alone cannot
rescue that upgrade: it never gets to run. What makes it work is this lookup.
On the one upgrade where the CRD is owned by nobody, the chart leaves it out of
the release and the hook adopts it and puts the chart's schema live in its
place. From the next upgrade on, the CRD is owned, this is empty, and the chart
templates it exactly as it always did.

A CRD owned by a different release is never adopted. That is somebody else's
object, and taking it would leave the release that owns it unable to update its
own; the upgrade stops here instead, naming what it found.

`helm template` and a client-side `--dry-run` have no cluster to look in, so
both render the CRD — the state every install is in once this has happened once.
*/}}
{{- define "reactor.crdAdoption" -}}
{{- if .Values.crds.install -}}
{{- $live := lookup "apiextensions.k8s.io/v1" "CustomResourceDefinition" "" (include "reactor.crdName" .) -}}
{{- if $live -}}
{{- $annotations := default (dict) $live.metadata.annotations -}}
{{- $owner := default "" (index $annotations "meta.helm.sh/release-name") -}}
{{- $ownerNamespace := default "" (index $annotations "meta.helm.sh/release-namespace") -}}
{{- if eq $owner "" -}}
{{- if .Values.crds.adopt -}}adopt{{- end -}}
{{- else if or (ne $owner .Release.Name) (ne $ownerNamespace .Release.Namespace) -}}
{{- fail (printf (join "" (list
  "the CustomResourceDefinition %s belongs to the Helm release %q in namespace %q, not to %q in namespace %q. "
  "Reactor will not take a CRD from another release: upgrade with --set crds.install=false to leave it to "
  "whoever owns it, or hand it over deliberately and upgrade again."))
  (include "reactor.crdName" .) $owner $ownerNamespace .Release.Name .Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "reactor.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "reactor.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The Secret holding UNIFI_USERNAME and UNIFI_PASSWORD, or empty when nothing on
this install writes to the console.

Two features need that pair — Alarm Manager self-registration and the unifi.*
actions — and they are the same credential at the same layer, so this resolves
it once rather than letting the deployment grow two env blocks that could
disagree. The actions value wins when both are set, and setting both to
different Secrets is a configuration mistake the chart cannot resolve for you.
*/}}
{{- define "reactor.consoleSecret" -}}
{{- if or .Values.unifi.actions.allowedWlans .Values.unifi.actions.allowedPoePorts -}}
{{- .Values.unifi.actions.existingSecret -}}
{{- else if and .Values.unifi.webhook.enabled .Values.unifi.webhook.registration.enabled -}}
{{- .Values.unifi.webhook.registration.existingSecret -}}
{{- end -}}
{{- end -}}
