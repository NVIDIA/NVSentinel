{{/*
Expand the name of the chart.
*/}}
{{- define "nic-health-monitor.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "nic-health-monitor.fullname" -}}
{{- "nic-health-monitor" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "nic-health-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "nic-health-monitor.labels" -}}
helm.sh/chart: {{ include "nic-health-monitor.chart" . }}
{{ include "nic-health-monitor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "nic-health-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nic-health-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
