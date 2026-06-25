{{/*
Expand the name of the chart.
*/}}
{{- define "labeler.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Whether expected device-count configuration needs ResourceSlice access.
*/}}
{{- define "labeler.expectedDeviceCountsRequiresResourceSlices" -}}
{{- $requires := false -}}
{{- if .Values.expectedDeviceCounts.enabled -}}
{{- range .Values.expectedDeviceCounts.classes }}
{{- if and .enabled (contains "resourceSlices" (default "" .currentExpression)) -}}
{{- $requires = true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $requires -}}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "labeler.fullname" -}}
{{- "labeler" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "labeler.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "labeler.labels" -}}
helm.sh/chart: {{ include "labeler.chart" . }}
{{ include "labeler.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "labeler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "labeler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ConfigMap name.
*/}}
{{- define "labeler.configMapName" -}}
{{ include "labeler.fullname" . }}
{{- end }}

{{/*
Expected device-count configuration content.
*/}}
{{- define "labeler.expectedDeviceCountsConfig" -}}
enabled = {{ .Values.expectedDeviceCounts.enabled }}
{{- range .Values.expectedDeviceCounts.classes }}

[[classes]]
name = {{ .name | quote }}
enabled = {{ .enabled }}
{{- with .groupingLabels }}
groupingLabels = [{{- range $i, $label := . }}{{ if $i }}, {{ end }}{{ $label | quote }}{{- end }}]
{{- end }}
currentExpression = '''
{{ trimSuffix "\n" (default "" .currentExpression) }}
'''

[classes.labels]
current = {{ .labels.current | quote }}
expected = {{ .labels.expected | quote }}
{{- range .expectedCountOverrides }}

[[classes.expectedCountOverrides]]
count = {{ .count }}
{{- with .matchLabels }}

[classes.expectedCountOverrides.matchLabels]
{{- range $key, $value := . }}
{{ $key | quote }} = {{ $value | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
