{{/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "preflight.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "preflight.fullname" -}}
{{- "preflight" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "preflight.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "preflight.labels" -}}
helm.sh/chart: {{ include "preflight.chart" . }}
{{ include "preflight.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "preflight.selectorLabels" -}}
app.kubernetes.io/name: {{ include "preflight.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image cache DaemonSet name.
*/}}
{{- define "preflight.imageCacheName" -}}
{{- printf "%s-image-cache" (include "preflight.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Image cache selector labels. Keep these distinct from the webhook Deployment selector.
*/}}
{{- define "preflight.imageCacheSelectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-image-cache" (include "preflight.name" .) | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: image-cache
{{- end }}

{{/*
Image cache common labels.
*/}}
{{- define "preflight.imageCacheLabels" -}}
helm.sh/chart: {{ include "preflight.chart" . }}
{{ include "preflight.imageCacheSelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/part-of: {{ include "preflight.name" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Resolve a configured preflight init container image, honoring global.image.tag.
*/}}
{{- define "preflight.initContainerImage" -}}
{{- $root := .root -}}
{{- $container := .container -}}
{{- $image := $container.image -}}
{{- if kindIs "string" $image -}}
{{- $image -}}
{{- else -}}
{{- $global := $root.Values.global | default dict -}}
{{- $globalImage := $global.image | default dict -}}
{{- $tag := $image.tag | default $globalImage.tag | default $root.Chart.AppVersion -}}
{{- printf "%s:%s" $image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "preflight.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "preflight.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Webhook name for MutatingWebhookConfiguration
*/}}
{{- define "preflight.webhookName" -}}
{{ include "preflight.name" . }}.nvsentinel.nvidia.com
{{- end }}

{{/*
Certificate secret name
*/}}
{{- define "preflight.certSecretName" -}}
{{ include "preflight.fullname" . }}-webhook-tls
{{- end }}

{{/*
Certificate DNS names
*/}}
{{- define "preflight.certDnsNames" -}}
- {{ include "preflight.fullname" . }}
- {{ include "preflight.fullname" . }}.{{ .Release.Namespace }}
- {{ include "preflight.fullname" . }}.{{ .Release.Namespace }}.svc
- {{ include "preflight.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local
{{- end }}

{{/*
Event processing strategy
*/}}
{{- define "preflight.processingStrategy" -}}
{{- .Values.processingStrategy | default "EXECUTE_REMEDIATION" }}
{{- end }}

{{/*
Platform connector socket path for health event reporting
Uses global.socketPath with unix:// prefix
*/}}
{{- define "preflight.connectorSocket" -}}
{{- if and .Values.global .Values.global.socketPath }}
{{- printf "unix://%s" .Values.global.socketPath }}
{{- else }}
{{- "unix:///var/run/nvsentinel.sock" }}
{{- end }}
{{- end }}


{{/*
Whether platform-connector auth is on, refusing the value shapes Go-template
truthiness would misread.

Deliberately named preflight.* rather than nvsentinel.*: Helm template names are
GLOBAL across the chart tree and a subchart definition wins over the parent's,
so reusing the umbrella name here would override it for every other chart. The
checks must stay identical to nvsentinel/templates/_helpers.tpl — a copy without
the boolean check is how a quoted "false" came to mean "enabled" in a standalone
render.
*/}}
{{- define "preflight.pcAuth.enabled" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- $enabled := $auth.enabled -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.platformConnectorAuth.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable authentication." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{- define "preflight.pcAuth.expirationSeconds" -}}
{{- $v := (((.Values.global).platformConnectorAuth)).tokenExpirationSeconds -}}
{{- if kindIs "invalid" $v -}}
{{- fail "global.platformConnectorAuth.tokenExpirationSeconds is required when platform-connector auth is enabled" -}}
{{- end -}}
{{- if not (or (kindIs "float64" $v) (kindIs "int" $v) (kindIs "int64" $v)) -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds must be an integer, got %s %#v." (kindOf $v) $v) -}}
{{- end -}}
{{- /*
YAML numbers reach templates as float64, so a fractional value passes a bare
numeric check and then renders into an integer Kubernetes field, which the API
server rejects when the pod is created.
*/ -}}
{{- if ne (float64 $v) (floor (float64 $v)) -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds must be a whole number of seconds, got %v." $v) -}}
{{- end -}}
{{- /*
Kubernetes rejects a projected ServiceAccount token lifetime below 10 minutes or
above 2^32 seconds (core validation, volume projection). Out-of-range values
render fine and are then refused by the API server when the pod is created, so
the workload never starts and the reason is a long way from the values file.
*/ -}}
{{- if lt (float64 $v) 600.0 -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds is %v, but Kubernetes rejects a projected token lifetime under 600 seconds (10 minutes)." $v) -}}
{{- end -}}
{{- if gt (float64 $v) 4294967296.0 -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds is %v, but Kubernetes rejects a projected token lifetime over 2^32 seconds." $v) -}}
{{- end -}}
{{- int64 $v -}}
{{- end -}}
