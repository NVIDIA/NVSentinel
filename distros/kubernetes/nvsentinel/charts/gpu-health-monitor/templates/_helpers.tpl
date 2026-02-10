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
DCGM service endpoint - uses global.dcgm.service.endpoint with fallback to local
*/}}
{{- define "gpu-health-monitor.dcgmEndpoint" -}}
{{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.service }}
{{- .Values.global.dcgm.service.endpoint | default .Values.dcgm.service.endpoint }}
{{- else }}
{{- .Values.dcgm.service.endpoint }}
{{- end }}
{{- end }}

{{/*
DCGM service port - uses global.dcgm.service.port with fallback to local
*/}}
{{- define "gpu-health-monitor.dcgmPort" -}}
{{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.service }}
{{- .Values.global.dcgm.service.port | default .Values.dcgm.service.port }}
{{- else }}
{{- .Values.dcgm.service.port }}
{{- end }}
{{- end }}

{{/*
DCGM address - combines endpoint and port
*/}}
{{- define "gpu-health-monitor.dcgmAddr" -}}
{{- printf "%s:%v" (include "gpu-health-monitor.dcgmEndpoint" .) (include "gpu-health-monitor.dcgmPort" .) }}
{{- end }}