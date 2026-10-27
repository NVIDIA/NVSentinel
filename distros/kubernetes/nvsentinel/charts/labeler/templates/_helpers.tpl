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
{{- if .enabled -}}
{{- if contains "resourceSlices" (default "" .currentExpression) -}}
{{- $requires = true -}}
{{- end -}}
{{- range .sources }}
{{- if or .resourceSliceDriver (contains "resourceSlices" (default "" .currentExpression)) (contains "resourceSlices" (default "" .groupingExpression)) -}}
{{- $requires = true -}}
{{- end -}}
{{- end -}}
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
Whether labeler should assume DCGM is available from an existing dcgm.version
label when no DCGM pod source is present. This is used by external-hostengine
and embedded-mode, where operators provide the same node label used by GPU
health monitor scheduling.
*/}}
{{- define "labeler.assumeDCGMAvailable" -}}
{{- $mode := "" -}}
{{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.mode -}}
{{- $mode = .Values.global.dcgm.mode -}}
{{- end -}}
{{- if has $mode (list "external-hostengine" "embedded-mode") -}}
true
{{- end -}}
{{- end -}}

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
{{- if not .sources }}
currentExpression = '''
{{ trimSuffix "\n" (default "" .currentExpression) }}
'''
{{- else if .currentExpression }}
currentExpression = '''
{{ trimSuffix "\n" .currentExpression }}
'''
{{- end }}

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
{{- range .sources }}

[[classes.sources]]
name = {{ .name | quote }}
{{- with .resourceSliceDriver }}
resourceSliceDriver = {{ . | quote }}
{{- end }}
{{- with .groupingLabels }}
groupingLabels = [{{- range $i, $label := . }}{{ if $i }}, {{ end }}{{ $label | quote }}{{- end }}]
{{- end }}
currentExpression = '''
{{ trimSuffix "\n" (default "" .currentExpression) }}
'''
{{- with .groupingExpression }}
groupingExpression = '''
{{ trimSuffix "\n" . }}
'''
{{- end }}
{{- end }}
{{- end }}
{{- end }}
