{{/*
Expand the name of the chart.
*/}}
{{- define "event-exporter.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "event-exporter.fullname" -}}
{{- "event-exporter" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "event-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "event-exporter.labels" -}}
helm.sh/chart: {{ include "event-exporter.chart" . }}
{{ include "event-exporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "event-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "event-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

