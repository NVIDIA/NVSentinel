{{/*
Expand the name of the chart.
*/}}
{{- define "system-services-monitor.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "system-services-monitor.fullname" -}}
{{- "system-services-monitor" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "system-services-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "system-services-monitor.labels" -}}
helm.sh/chart: {{ include "system-services-monitor.chart" . }}
{{ include "system-services-monitor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: metrics
{{- end }}

{{/*
Selector labels
*/}}
{{- define "system-services-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "system-services-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
