{{/*
Expand the name of the chart.
*/}}
{{- define "kubernetes-object-monitor.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kubernetes-object-monitor.fullname" -}}
{{- "kubernetes-object-monitor" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "kubernetes-object-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kubernetes-object-monitor.labels" -}}
helm.sh/chart: {{ include "kubernetes-object-monitor.chart" . }}
{{ include "kubernetes-object-monitor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kubernetes-object-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubernetes-object-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}


{{/*
Chart-local copies of the umbrella chart's nvsentinel.pcAuth.* helpers, so this
chart still renders on its own (`helm template` of this directory alone loads
the subchart without its parent, which `make helm-lint` does for every chart).

They are deliberately named kubernetes-object-monitor.* rather than nvsentinel.*:
Helm template names are GLOBAL across the whole chart tree, and a subchart
definition wins over the parent's, so reusing the nvsentinel.* names here would
silently override the umbrella's helpers for every other chart — making later
edits to nvsentinel/templates/_helpers.tpl take no effect at all.

They read the same global values as the umbrella versions AND must apply the
same strictness. An earlier copy skipped the boolean check here, so a quoted
"false" was refused by the umbrella but silently ENABLED token injection when
this chart was rendered standalone. Keep the checks identical to that file
until the shared helpers move into a proper Helm library chart; the chart test
in tests/pc_auth_strictness_test.yaml is what catches the two drifting again.
*/}}
{{- define "kubernetes-object-monitor.pcAuth.enabled" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- $enabled := $auth.enabled -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.platformConnectorAuth.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable authentication." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{- define "kubernetes-object-monitor.pcAuth.mountPath" -}}
{{- required "global.platformConnectorAuth.tokenMountPath is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).tokenMountPath) -}}
{{- end -}}

{{- define "kubernetes-object-monitor.pcAuth.tokenPath" -}}
{{- printf "%s/token" (include "kubernetes-object-monitor.pcAuth.mountPath" .) -}}
{{- end -}}

{{- define "kubernetes-object-monitor.pcAuth.volume" -}}
- name: platform-connector-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ required "global.platformConnectorAuth.audience is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).audience) | quote }}
          expirationSeconds: {{ include "kubernetes-object-monitor.pcAuth.expirationSeconds" . }}
          path: token
{{- end -}}

{{- define "kubernetes-object-monitor.pcAuth.volumeMount" -}}
- name: platform-connector-token
  mountPath: {{ include "kubernetes-object-monitor.pcAuth.mountPath" . }}
  readOnly: true
{{- end -}}

{{- define "kubernetes-object-monitor.pcAuth.expirationSeconds" -}}
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
