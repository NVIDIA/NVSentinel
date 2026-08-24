{{/*
Expand the name of the chart.
*/}}
{{- define "gpu-health-monitor.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "gpu-health-monitor.fullname" -}}
{{- "gpu-health-monitor" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "gpu-health-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gpu-health-monitor.labels" -}}
helm.sh/chart: {{ include "gpu-health-monitor.chart" . }}
{{ include "gpu-health-monitor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "gpu-health-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gpu-health-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
DCGM service enabled - uses global.dcgm.enabled with fallback to local
*/}}
{{- define "gpu-health-monitor.dcgmEnabled" -}}
{{- if and .Values.global .Values.global.dcgm }}
{{- .Values.global.dcgm.enabled }}
{{- else }}
{{- .Values.dcgm.dcgmK8sServiceEnabled }}
{{- end }}
{{- end }}

{{/*
DCGM source mode.
*/}}
{{- define "gpu-health-monitor.dcgmMode" -}}
{{- $mode := "" -}}
{{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.mode -}}
{{- $mode = .Values.global.dcgm.mode -}}
{{- else -}}
{{- $mode = (.Values.dcgm.mode | default "operator-service") -}}
{{- end -}}
{{- if not (has $mode (list "operator-service" "external-hostengine" "embedded-mode")) -}}
{{- fail (printf "unsupported DCGM source mode %q; expected operator-service, external-hostengine, or embedded-mode" $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end }}

{{/*
DCGM endpoint for the selected source mode. Global values take precedence over
the corresponding chart-local values.
*/}}
{{- define "gpu-health-monitor.dcgmEndpoint" -}}
{{- $mode := include "gpu-health-monitor.dcgmMode" . -}}
{{- $endpoint := "" -}}
{{- if eq $mode "external-hostengine" -}}
  {{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.externalHostengine -}}
    {{- $endpoint = (.Values.global.dcgm.externalHostengine.endpoint | default .Values.dcgm.externalHostengine.endpoint) -}}
  {{- else -}}
    {{- $endpoint = .Values.dcgm.externalHostengine.endpoint -}}
  {{- end -}}
{{- else if eq $mode "embedded-mode" -}}
  {{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.embedded -}}
    {{- $endpoint = (.Values.global.dcgm.embedded.endpoint | default .Values.dcgm.embedded.endpoint) -}}
  {{- else -}}
    {{- $endpoint = .Values.dcgm.embedded.endpoint -}}
  {{- end -}}
  {{- if not (has $endpoint (list "localhost" "127.0.0.1" "::1")) -}}
    {{- fail (printf "embedded-mode DCGM endpoint %q must be a loopback address: localhost, 127.0.0.1, or ::1" $endpoint) -}}
  {{- end -}}
{{- else -}}
  {{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.service -}}
    {{- $endpoint = (.Values.global.dcgm.service.endpoint | default .Values.dcgm.service.endpoint) -}}
  {{- else -}}
    {{- $endpoint = .Values.dcgm.service.endpoint -}}
  {{- end -}}
{{- end -}}
{{- $endpoint -}}
{{- end }}

{{/*
DCGM port for the selected source mode. Global values take precedence over the
corresponding chart-local values.
*/}}
{{- define "gpu-health-monitor.dcgmPort" -}}
{{- $mode := include "gpu-health-monitor.dcgmMode" . -}}
{{- if eq $mode "external-hostengine" -}}
  {{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.externalHostengine -}}
    {{- .Values.global.dcgm.externalHostengine.port | default .Values.dcgm.externalHostengine.port -}}
  {{- else -}}
    {{- .Values.dcgm.externalHostengine.port -}}
  {{- end -}}
{{- else if eq $mode "embedded-mode" -}}
  {{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.embedded -}}
    {{- .Values.global.dcgm.embedded.port | default .Values.dcgm.embedded.port -}}
  {{- else -}}
    {{- .Values.dcgm.embedded.port -}}
  {{- end -}}
{{- else -}}
  {{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.service -}}
    {{- .Values.global.dcgm.service.port | default .Values.dcgm.service.port -}}
  {{- else -}}
    {{- .Values.dcgm.service.port -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{/*
DCGM address for the selected source mode.
*/}}
{{- define "gpu-health-monitor.dcgmAddr" -}}
{{- printf "%s:%v" (include "gpu-health-monitor.dcgmEndpoint" .) (include "gpu-health-monitor.dcgmPort" .) }}
{{- end }}

{{/*
GPU health monitor CLI mode passed to --dcgm-mode. Derived from the source mode
so the two can never drift: embedded-mode runs an in-process embedded DCGM
hostengine with a loopback listener ("local-managed"); operator-service and
external-hostengine connect remotely ("remote").
*/}}
{{- define "gpu-health-monitor.dcgmCliMode" -}}
{{- if eq (include "gpu-health-monitor.dcgmMode" .) "embedded-mode" -}}local-managed{{- else -}}remote{{- end -}}
{{- end }}

{{/*
Whether the GPU health monitor pod should use host networking. External
hostengine mode defaults to localhost, so it must resolve in the host network
namespace. An endpoint override does not change this networking contract.
*/}}
{{- define "gpu-health-monitor.useHostNetworking" -}}
{{- if or .Values.useHostNetworking (eq (include "gpu-health-monitor.dcgmMode" .) "external-hostengine") -}}true{{- end -}}
{{- end }}


{{/*
Chart-local copies of the umbrella chart's nvsentinel.pcAuth.* helpers, so this
chart still renders on its own (`helm template` of this directory alone loads
the subchart without its parent, which `make helm-lint` does for every chart).

They are deliberately named gpu-health-monitor.* rather than nvsentinel.*:
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
{{- define "gpu-health-monitor.pcAuth.enabled" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- $enabled := $auth.enabled -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.platformConnectorAuth.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable authentication." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{- define "gpu-health-monitor.pcAuth.mountPath" -}}
{{- required "global.platformConnectorAuth.tokenMountPath is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).tokenMountPath) -}}
{{- end -}}

{{- define "gpu-health-monitor.pcAuth.tokenPath" -}}
{{- printf "%s/token" (include "gpu-health-monitor.pcAuth.mountPath" .) -}}
{{- end -}}

{{- define "gpu-health-monitor.pcAuth.volume" -}}
- name: platform-connector-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ required "global.platformConnectorAuth.audience is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).audience) | quote }}
          expirationSeconds: {{ include "gpu-health-monitor.pcAuth.expirationSeconds" . }}
          path: token
{{- end -}}

{{- define "gpu-health-monitor.pcAuth.volumeMount" -}}
- name: platform-connector-token
  mountPath: {{ include "gpu-health-monitor.pcAuth.mountPath" . }}
  readOnly: true
{{- end -}}

{{- define "gpu-health-monitor.pcAuth.expirationSeconds" -}}
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
