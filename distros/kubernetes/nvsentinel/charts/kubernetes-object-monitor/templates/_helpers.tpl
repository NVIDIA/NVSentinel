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

