{{/*
Expand the name of the chart.
*/}}
{{- define "slurm-drain-monitor.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "slurm-drain-monitor.fullname" -}}
{{- "slurm-drain-monitor" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "slurm-drain-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "slurm-drain-monitor.labels" -}}
helm.sh/chart: {{ include "slurm-drain-monitor.chart" . }}
{{ include "slurm-drain-monitor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "slurm-drain-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "slurm-drain-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
