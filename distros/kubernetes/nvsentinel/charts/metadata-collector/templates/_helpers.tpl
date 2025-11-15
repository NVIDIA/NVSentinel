{{/*
Expand the name of the chart.
*/}}
{{- define "metadata-collector.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "metadata-collector.fullname" -}}
{{- "metadata-collector" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "metadata-collector.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "metadata-collector.labels" -}}
helm.sh/chart: {{ include "metadata-collector.chart" . }}
{{ include "metadata-collector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "metadata-collector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "metadata-collector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

