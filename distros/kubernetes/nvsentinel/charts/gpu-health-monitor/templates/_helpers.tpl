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

