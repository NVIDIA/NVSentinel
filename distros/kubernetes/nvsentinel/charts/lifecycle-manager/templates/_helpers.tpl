{{/*
Expand the name of the chart.
*/}}
{{- define "lifecycle-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "lifecycle-manager.fullname" -}}
{{- if .Values.fullNameOverride }}
{{- .Values.fullNameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "lifecycle-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "lifecycle-manager.labels" -}}
helm.sh/chart: {{ include "lifecycle-manager.chart" . }}
{{ include "lifecycle-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "lifecycle-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lifecycle-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "lifecycle-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "lifecycle-manager.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Chart-local copies of the umbrella chart's nvsentinel.pcAuth.* helpers, so this
chart still renders on its own (`helm template` of this directory alone loads
the subchart without its parent, which `make helm-lint` does for every chart).

They are deliberately named lifecycle-manager.* rather than nvsentinel.*: Helm
template names are GLOBAL across the whole chart tree, and a subchart definition
wins over the parent's, so reusing the nvsentinel.* names here would silently
override the umbrella's helpers for every other chart — making later edits to
nvsentinel/templates/_helpers.tpl take no effect at all.

They read the same global values as the umbrella versions AND must apply the
same strictness, or a quoted "false" refused by the umbrella would silently
ENABLE token injection when this chart is rendered standalone. Keep the checks
identical to that file until the shared helpers move into a proper Helm library
chart; tests/pc_auth_strictness_test.yaml is what catches the two drifting.
*/}}
{{- define "lifecycle-manager.pcAuth.enabled" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- $enabled := $auth.enabled -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.platformConnectorAuth.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable authentication." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{- define "lifecycle-manager.pcAuth.mountPath" -}}
{{- required "global.platformConnectorAuth.tokenMountPath is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).tokenMountPath) -}}
{{- end -}}

{{- define "lifecycle-manager.pcAuth.tokenPath" -}}
{{- printf "%s/token" (include "lifecycle-manager.pcAuth.mountPath" .) -}}
{{- end -}}

{{- define "lifecycle-manager.pcAuth.volume" -}}
- name: platform-connector-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ required "global.platformConnectorAuth.audience is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).audience) | quote }}
          expirationSeconds: {{ include "lifecycle-manager.pcAuth.expirationSeconds" . }}
          path: token
{{- end -}}

{{- define "lifecycle-manager.pcAuth.volumeMount" -}}
- name: platform-connector-token
  mountPath: {{ include "lifecycle-manager.pcAuth.mountPath" . }}
  readOnly: true
{{- end -}}

{{- define "lifecycle-manager.pcAuth.expirationSeconds" -}}
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
