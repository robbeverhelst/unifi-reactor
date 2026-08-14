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
